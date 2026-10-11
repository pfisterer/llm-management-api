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

func TestBootstrapAndValidation(t *testing.T) {
	ctx := context.Background()
	s := service()
	s.cfg.BootstrapMembers = []string{"user:a@dhbw.de"}
	if created, err := s.Bootstrap(ctx); !created || err != nil {
		t.Fatalf("bootstrap: %v %v", created, err)
	}
	if created, _ := s.Bootstrap(ctx); created {
		t.Fatal("bootstrap only when there is no project")
	}
	p, _ := s.Get(ctx, "pilot")
	if p.RoleOf([]string{"user:a@dhbw.de"}) != RoleManager || len(p.Profiles) != 3 || !p.AllowsGPUClass("l4-3q") {
		t.Fatalf("pilot: %+v", p)
	}
	for _, bad := range []Project{
		{Slug: "Bad Name"},
		{Slug: "ok", Members: []Member{{Token: "a@dhbw.de"}}},
		{Slug: "ok", Profiles: []string{"nope"}},
		{Slug: "ok", GPUClasses: []string{"h100"}},
		{Slug: "ok", Budgets: []Budget{{Unit: "euro", Window: "30d", Amount: 1}}},
	} {
		if _, err := s.Create(ctx, bad, "admin"); reason(err) == "" || strings.HasPrefix(reason(err), "ERROR") {
			t.Errorf("%+v: want a refusal, got %v", bad, err)
		}
	}
	if _, err := s.Create(ctx, Project{Slug: "pilot"}, "admin"); !strings.Contains(reason(err), "gibt es schon") {
		t.Fatalf("duplicate: %v", err)
	}
}

func TestManagerMayOnlyChangeMembersImagesDescription(t *testing.T) {
	ctx := context.Background()
	s := service()
	_, _ = s.Create(ctx, Project{Slug: "kurs", Profiles: []string{"cpu"}, Limits: Limits{ServersPerPerson: 1}}, "admin")
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
	_, _ = s.Create(ctx, Project{Slug: "kurs", Name: "Kurs", Members: []Member{{Token: "group:wwi23seb"}}, Profiles: []string{"cpu", "pytorch"},
		GPUClasses: []string{"l4-3q"}, Limits: Limits{ServersPerPerson: 2, CPUPerPerson: 2, RAMGBPerPerson: 8, GPUsPerPerson: 1, GPUs: 1}}, "admin")
	_, _ = s.Create(ctx, Project{Slug: "cpu-only", Members: []Member{{Token: "user:a@dhbw.de"}}, Profiles: []string{"cpu", "pytorch"}}, "admin")
	_, _ = s.Create(ctx, Project{Slug: "alt", Members: []Member{{Token: "user:a@dhbw.de"}}, Profiles: []string{"cpu"}, EndsAt: &yesterday}, "admin")

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
		{"no GPUs in project", StartRequest{Person: "a@dhbw.de", Project: "cpu-only", Profile: "pytorch", GPUs: 1}, "keine GPUs"},
		{"cores per person", StartRequest{Person: "a@dhbw.de", Project: "kurs", Profile: "cpu", CPU: 1.5}, "Kerne"},
		{"memory per person", StartRequest{Person: "a@dhbw.de", Project: "kurs", Profile: "cpu", RAMGB: 5}, "Speicher"},
		{"project GPUs busy", StartRequest{Person: "a@dhbw.de", Project: "kurs", Profile: "pytorch", GPUs: 1}, "GPUs des Projekts"},
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
	// Server limit: two running servers of a in kurs.
	s2 := service(Running{Server: "s1", Person: "a@dhbw.de", Project: "kurs", Profile: "cpu"}, Running{Server: "s2", Person: "a@dhbw.de", Project: "kurs", Profile: "cpu"},
		Running{Server: "k1", Person: "a@dhbw.de", Project: "kurs", Profile: "gpu-kernel"})
	_, _ = s2.Create(ctx, Project{Slug: "kurs", Members: []Member{{Token: "user:a@dhbw.de"}}, Profiles: []string{"cpu"}, Limits: Limits{ServersPerPerson: 2}}, "admin")
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
	_, _ = s.Create(ctx, Project{Slug: "a", Members: []Member{{Token: "group:g", Role: RoleManager}}}, "admin")
	_, _ = s.Create(ctx, Project{Slug: "b"}, "admin")
	_, _ = s.Create(ctx, Project{Slug: "c", Members: []Member{{Token: "group:g"}}, EndsAt: &old}, "admin")
	mine, _ := s.ForPerson(ctx, []string{"group:g"}, false)
	if len(mine) != 1 || mine[0].Slug != "a" || mine[0].Role != RoleManager {
		t.Fatalf("mine: %+v", mine)
	}
	all, _ := s.ForPerson(ctx, []string{"group:g"}, true)
	if len(all) != 3 || all[1].Role != "admin" {
		t.Fatalf("all: %+v", all)
	}
}
