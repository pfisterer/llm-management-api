package workspace

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

var catalog = Config{
	Profiles:   []Profile{{Slug: "cpu", Name: "CPU"}, {Slug: "pytorch", Name: "PyTorch", GPUClass: "l4-3q"}, {Slug: "git-cpu", Name: "Git"}},
	GPUClasses: []string{"l4-3q"},
}

func service(running ...Running) *Service {
	return NewService(NewMemoryStore(), catalog, func(context.Context) ([]Running, error) { return running, nil })
}

func reason(err error) string {
	var r *Refusal
	if errors.As(err, &r) {
		return r.Reason
	}
	if err != nil {
		return "ERROR " + err.Error()
	}
	return ""
}

func TestValidation(t *testing.T) {
	ctx := context.Background()
	s := service()
	if _, err := s.create(ctx, Project{Slug: "pilot", Profiles: []string{"cpu"}}, "admin"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []Project{
		{Slug: "Bad Name"},
		{Slug: "ok", Members: []Member{{Token: "a@dhbw.de"}}},
		{Slug: "ok", Profiles: []string{"nope"}},
		{Slug: "ok", GPUClasses: []string{"h100"}},
		{Slug: "ok", Budgets: []Budget{{Unit: "euro", Window: "30d", Amount: 1}}},
	} {
		if _, err := s.create(ctx, bad, "admin"); reason(err) == "" || strings.HasPrefix(reason(err), "ERROR") {
			t.Errorf("%+v: want a refusal, got %v", bad, err)
		}
	}
	if _, err := s.create(ctx, Project{Slug: "pilot"}, "admin"); !strings.Contains(reason(err), "gibt es schon") {
		t.Fatalf("duplicate: %v", err)
	}
}

func TestManagerMayOnlyChangeMembersImagesDescription(t *testing.T) {
	ctx := context.Background()
	s := service()
	_, _ = s.create(ctx, Project{Slug: "kurs", Profiles: []string{"cpu"}, Limits: Limits{ServersPerPerson: 1}}, "admin")
	out, err := s.Update(ctx, Project{Slug: "kurs", Description: "neu", Members: []Member{{Token: "group:wwi23seb"}},
		Profiles: []string{"cpu", "pytorch"}, Limits: Limits{ServersPerPerson: 9}}, "manager@dhbw.de", false)
	if err != nil || out.Description != "neu" || len(out.Members) != 1 || out.Members[0].Role != RoleMember || len(out.Profiles) != 1 || out.Limits.ServersPerPerson != 1 {
		t.Fatalf("manager update: %+v %v", out, err)
	}
}

func TestCheckStart(t *testing.T) {
	ctx := context.Background()
	me := []string{"user:a@dhbw.de", "group:wwi23seb"}
	running := []Running{
		{Server: "jupyter-a", Person: "a@dhbw.de", Project: "kurs", Profile: "cpu", CPU: 1, RAMGB: 4},
		{Server: "jupyter-b-gpu", Person: "b@dhbw.de", Project: "kurs", Profile: "pytorch", GPUClass: "l4-3q", CPU: 1, RAMGB: 2, GPUs: 1},
		{Server: "jupyter-a-other", Person: "a@dhbw.de", Project: "anders", Profile: "cpu", CPU: 8, RAMGB: 30},
	}
	s := service(running...)
	yesterday := time.Now().Add(-48 * time.Hour)
	_, _ = s.create(ctx, Project{Slug: "kurs", Name: "Kurs", Members: []Member{{Token: "group:wwi23seb"}}, Profiles: []string{"cpu", "pytorch"},
		GPUClasses: []string{"l4-3q"}, Limits: Limits{ServersPerPerson: 2, CPUPerPerson: 2, RAMGBPerPerson: 8, GPUsPerPerson: 1}, GPUsByClass: map[string]int{"l4-3q": 1}}, "admin")
	_, _ = s.create(ctx, Project{Slug: "cpu-only", Members: []Member{{Token: "user:a@dhbw.de"}}, Profiles: []string{"cpu", "pytorch"}}, "admin")
	_, _ = s.create(ctx, Project{Slug: "alt", Members: []Member{{Token: "user:a@dhbw.de"}}, Profiles: []string{"cpu"}, EndsAt: &yesterday}, "admin")

	cases := []struct {
		name string
		req  StartRequest
		want string // part of the reason; "" = allowed
	}{
		{"allowed", StartRequest{Person: "a@dhbw.de", Project: "kurs", Profile: "cpu", CPU: 0.5, RAMGB: 2}, ""},
		{"no project", StartRequest{Person: "a@dhbw.de", Profile: "cpu"}, "Kein Projekt"},
		{"unknown project", StartRequest{Person: "a@dhbw.de", Project: "x", Profile: "cpu"}, "gibt es nicht"},
		{"not a member", StartRequest{Person: "c@dhbw.de", Project: "cpu-only", Profile: "cpu"}, "Kein Mitglied"},
		{"ended", StartRequest{Person: "a@dhbw.de", Project: "alt", Profile: "cpu"}, "abgelaufen"},
		{"profile not allowed", StartRequest{Person: "a@dhbw.de", Project: "kurs", Profile: "git-cpu"}, "nicht freigegeben"},
		{"free images not allowed", StartRequest{Person: "a@dhbw.de", Project: "kurs", Profile: FreeProfile}, "nicht freigegeben"},
		{"no GPUs in project", StartRequest{Person: "a@dhbw.de", Project: "cpu-only", Profile: "pytorch", GPUs: 1}, "keine GPUs"},
		{"cores per person", StartRequest{Person: "a@dhbw.de", Project: "kurs", Profile: "cpu", CPU: 1.5}, "Kerne"},
		{"memory per person", StartRequest{Person: "a@dhbw.de", Project: "kurs", Profile: "cpu", RAMGB: 5}, "Speicher"},
		{"project GPUs of the class busy", StartRequest{Person: "a@dhbw.de", Project: "kurs", Profile: "pytorch", GPUs: 1}, "GPUs der Klasse l4-3q"},
		{"own restart does not count", StartRequest{Person: "b@dhbw.de", Project: "kurs", Profile: "pytorch", GPUs: 1, Server: "jupyter-b-gpu"}, ""},
	}
	for _, c := range cases {
		tokens := me
		if c.req.Person == "c@dhbw.de" {
			tokens = []string{"user:c@dhbw.de"}
		}
		if c.req.Person == "b@dhbw.de" {
			tokens = []string{"user:b@dhbw.de", "group:wwi23seb"}
		}
		got := reason(s.CheckStart(ctx, tokens, c.req))
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	// Free images where the project allows them.
	_, _ = s.create(ctx, Project{Slug: "frei", Members: []Member{{Token: "user:a@dhbw.de"}}, FreeImages: true}, "admin")
	if got := reason(s.CheckStart(ctx, me, StartRequest{Person: "a@dhbw.de", Project: "frei", Profile: FreeProfile, CPU: 0.25, RAMGB: 1})); got != "" {
		t.Fatalf("free image: %q", got)
	}
	// With a GPU: the project's GPU class and GPUs per person count as for the GPU profiles.
	if got := reason(s.CheckStart(ctx, me, StartRequest{Person: "a@dhbw.de", Project: "frei", Profile: FreeGPUProfile, GPUs: 1})); !strings.Contains(got, "keine GPUs") {
		t.Fatalf("free image with GPU, project without GPUs: %q", got)
	}
	_, _ = s.create(ctx, Project{Slug: "frei-gpu", Members: []Member{{Token: "user:a@dhbw.de"}}, FreeImages: true, Limits: Limits{GPUsPerPerson: 1}}, "admin")
	if got := reason(s.CheckStart(ctx, me, StartRequest{Person: "a@dhbw.de", Project: "frei-gpu", Profile: FreeGPUProfile, GPUs: 1})); !strings.Contains(got, "l4-3q") {
		t.Fatalf("free image with GPU, class not granted: %q", got)
	}
	// Server limit: two running servers of a in kurs.
	s2 := service(Running{Server: "s1", Person: "a@dhbw.de", Project: "kurs", Profile: "cpu"}, Running{Server: "s2", Person: "a@dhbw.de", Project: "kurs", Profile: "cpu"},
		Running{Server: "k1", Person: "a@dhbw.de", Project: "kurs", Profile: "gpu-kernel"})
	_, _ = s2.create(ctx, Project{Slug: "kurs", Members: []Member{{Token: "user:a@dhbw.de"}}, Profiles: []string{"cpu"}, Limits: Limits{ServersPerPerson: 2}}, "admin")
	if got := reason(s2.CheckStart(ctx, []string{"user:a@dhbw.de"}, StartRequest{Person: "a@dhbw.de", Project: "kurs", Profile: "cpu"})); !strings.Contains(got, "2 Server") {
		t.Fatalf("server limit: %q", got)
	}
	// Budget hook.
	s2.SetBudgetCheck(func(context.Context, Project, string, string) error { return refuse("Budget aufgebraucht") })
	if got := reason(s.CheckStart(ctx, me, cases[0].req)); got != "" {
		t.Fatalf("no budget hook on s: %q", got)
	}
	s.SetBudgetCheck(func(context.Context, Project, string, string) error { return refuse("Budget aufgebraucht") })
	if got := reason(s.CheckStart(ctx, me, cases[0].req)); got != "Budget aufgebraucht" {
		t.Fatalf("budget: %q", got)
	}
}

func TestForPerson(t *testing.T) {
	ctx := context.Background()
	s := service()
	old := time.Now().Add(-72 * time.Hour)
	_, _ = s.create(ctx, Project{Slug: "a", Members: []Member{{Token: "group:g", Role: RoleManager}}}, "admin")
	_, _ = s.create(ctx, Project{Slug: "b"}, "admin")
	_, _ = s.create(ctx, Project{Slug: "c", Members: []Member{{Token: "group:g"}}, EndsAt: &old}, "admin")
	mine, _ := s.ForPerson(ctx, []string{"group:g"}, "", false)
	if len(mine) != 1 || mine[0].Slug != "a" || mine[0].Role != RoleManager {
		t.Fatalf("mine: %+v", mine)
	}
	all, _ := s.ForPerson(ctx, []string{"group:g"}, "", true)
	if len(all) != 3 || all[1].Role != "admin" {
		t.Fatalf("all: %+v", all)
	}
}

func TestKernelImageMustDeriveFromBase(t *testing.T) {
	ctx := context.Background()
	s := service()
	checked := 0
	s.SetImageLabels(func(_ context.Context, image string) (map[string]string, error) {
		checked++
		switch image {
		case "ghcr.io/kurs/ml:1":
			return map[string]string{BaseLabel: "notebook-v1"}, nil
		case "ghcr.io/kurs/fremd:1":
			return map[string]string{}, nil
		}
		return nil, errors.New("not found")
	})
	_, _ = s.create(ctx, Project{Slug: "kurs", Profiles: []string{"cpu"}}, "admin")
	for image, want := range map[string]string{"ghcr.io/kurs/fremd:1": "nicht von", "ghcr.io/kurs/weg:1": "nicht abrufbar", "Kein Image": "kein Image-Name"} {
		if _, err := s.Update(ctx, Project{Slug: "kurs", KernelImage: image}, "manager@dhbw.de", false); !strings.Contains(reason(err), want) {
			t.Errorf("%s: %v", image, err)
		}
	}
	out, err := s.Update(ctx, Project{Slug: "kurs", KernelImage: " ghcr.io/kurs/ml:1 "}, "manager@dhbw.de", false)
	if err != nil || out.KernelImage != "ghcr.io/kurs/ml:1" {
		t.Fatalf("derived image: %+v %v", out, err)
	}
	// Unchanged: not looked up again (the registry may be away); empty: back to notebook-base.
	n := checked
	if _, err := s.Update(ctx, Project{Slug: "kurs", KernelImage: "ghcr.io/kurs/ml:1"}, "manager@dhbw.de", false); err != nil || checked != n {
		t.Errorf("unchanged image: %v, checked %d times", err, checked-n)
	}
	if out, err := s.Update(ctx, Project{Slug: "kurs"}, "manager@dhbw.de", false); err != nil || out.KernelImage != "" {
		t.Errorf("cleared: %+v %v", out, err)
	}
}
