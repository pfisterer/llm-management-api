package workspace

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

var catalog = Config{
	Profiles: []Profile{{Slug: "cpu", Name: "CPU"}, {Slug: "gpu", Name: "GPU", GPU: true}, {Slug: SplitProfile, Name: "CPU + GPU-Kernel", GPU: true}},
	Bases:    []Base{{ID: "python", Name: "Python", Image: "reg/platform/base-python:v2"}, {ID: "pytorch", Name: "PyTorch", Image: "reg/platform/base-pytorch:v2", Default: true}, {ID: "tensorflow", Name: "TensorFlow", Image: "reg/platform/base-tensorflow:v2"}},
}

func service(running ...Running) *Service {
	s := NewService(NewMemoryStore(), catalog, func(context.Context) ([]Running, error) { return running, nil })
	s.SetGPUClasses(func(context.Context) ([]GPUClass, error) {
		return []GPUClass{{ID: "l4-3q", Name: "General · small"}, {ID: "general-medium", Name: "General · medium"}}, nil
	})
	return s
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
		{Slug: "ok", GPUClasses: []string{"h100"}},
		{Slug: "ok", Budgets: []Budget{{Unit: "euro", Window: "30d", Amount: 1}}},
	} {
		if _, err := s.create(ctx, bad, "admin"); reason(err) == "" || strings.HasPrefix(reason(err), "ERROR") {
			t.Errorf("%+v: want a refusal, got %v", bad, err)
		}
	}
	// A profile the catalogue no longer has is dropped, not refused.
	if p, err := s.create(ctx, Project{Slug: "alt", Profiles: []string{"cpu", "nope", "git-gpu", "tensorflow"}}, "admin"); err != nil || !slices.Equal(p.Profiles, []string{"cpu", "gpu"}) {
		t.Fatalf("stale profile: %+v %v", p, err)
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
		Profiles: []string{"cpu", "gpu"}, Limits: Limits{ServersPerPerson: 9}}, "manager@dhbw.de", false)
	if err != nil || out.Description != "neu" || len(out.Members) != 1 || out.Members[0].Role != RoleMember || len(out.Profiles) != 1 || out.Limits.ServersPerPerson != 1 {
		t.Fatalf("manager update: %+v %v", out, err)
	}
	// Budgets, GPUs, storage, end and no_workspaces stay too.
	out, err = s.Update(ctx, Project{Slug: "kurs", Members: out.Members, Budgets: []Budget{{Unit: "gpu_hours", Window: "30d", Amount: 1}}, GPUClasses: []string{"l4-3q"},
		GPUsByClass: map[string]int{"l4-3q": 9}, StorageGB: 99, NoWorkspaces: true, Limits: Limits{GPUsPerPerson: 99}}, "manager@dhbw.de", false)
	if err != nil || len(out.Budgets) != 0 || len(out.GPUClasses) != 0 || len(out.GPUsByClass) != 0 || out.StorageGB != 0 || out.NoWorkspaces || out.Limits.GPUsPerPerson != 0 {
		t.Fatalf("manager update of admin fields: %+v %v", out, err)
	}
}

func TestCheckStart(t *testing.T) {
	ctx := context.Background()
	me := []string{"user:a@dhbw.de", "group:wwi23seb"}
	running := []Running{
		{Server: "jupyter-a", Person: "a@dhbw.de", Project: "kurs", Profile: "cpu", CPU: 1, RAMGB: 4},
		{Server: "jupyter-b-gpu", Person: "b@dhbw.de", Project: "kurs", Profile: "gpu", GPUClass: "l4-3q", CPU: 1, RAMGB: 2, GPUs: 1},
		{Server: "jupyter-a-other", Person: "a@dhbw.de", Project: "anders", Profile: "cpu", CPU: 8, RAMGB: 30},
	}
	s := service(running...)
	yesterday := time.Now().Add(-48 * time.Hour)
	_, _ = s.create(ctx, Project{Slug: "kurs", Name: "Kurs", Members: []Member{{Token: "group:wwi23seb"}}, Profiles: []string{"cpu", "gpu"},
		GPUClasses: []string{"l4-3q"}, Limits: Limits{ServersPerPerson: 2, CPUPerPerson: 2, RAMGBPerPerson: 8, GPUsPerPerson: 1}, GPUsByClass: map[string]int{"l4-3q": 1}}, "admin")
	_, _ = s.create(ctx, Project{Slug: "cpu-only", Members: []Member{{Token: "user:a@dhbw.de"}}, Profiles: []string{"cpu", "gpu"}}, "admin")
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
		{"profile not allowed", StartRequest{Person: "a@dhbw.de", Project: "kurs", Profile: SplitProfile}, "nicht freigegeben"},
		{"no GPUs in project", StartRequest{Person: "a@dhbw.de", Project: "cpu-only", Profile: "gpu", GPUs: 1}, "keine GPUs"},
		{"cores per person", StartRequest{Person: "a@dhbw.de", Project: "kurs", Profile: "cpu", CPU: 1.5}, "Kerne"},
		{"memory per person", StartRequest{Person: "a@dhbw.de", Project: "kurs", Profile: "cpu", RAMGB: 5}, "Speicher"},
		{"project GPUs of the class busy", StartRequest{Person: "a@dhbw.de", Project: "kurs", Profile: "gpu", GPUs: 1}, "GPUs der Klasse General · small"},
		{"own restart does not count", StartRequest{Person: "b@dhbw.de", Project: "kurs", Profile: "gpu", GPUs: 1, Server: "jupyter-b-gpu"}, ""},
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

type fakeBuilder struct {
	builds  map[string]ImageBuild
	started int
	checked int
	bases   []string
}

func (f *fakeBuilder) BuildProjectImage(_ context.Context, slug, base, gitURL, _ string, rebuild bool) (ImageBuild, error) {
	f.bases = append(f.bases, base)
	if strings.Contains(gitURL, "evil") {
		return ImageBuild{}, errors.New("evil.example ist nicht zugelassen")
	}
	if b, ok := f.builds[slug]; ok && !rebuild {
		return b, nil
	}
	f.started++
	b := ImageBuild{Status: "building", Image: "reg/envs/" + slug + ":c-v1"}
	f.builds[slug] = b
	return b, nil
}
func (f *fakeBuilder) ProjectImageBuild(_ context.Context, slug string) (*ImageBuild, error) {
	b, ok := f.builds[slug]
	if !ok {
		return nil, nil
	}
	return &b, nil
}
func (f *fakeBuilder) ProjectImageLog(context.Context, string) (BuildLog, error) {
	return BuildLog{Log: "log"}, nil
}
func (f *fakeBuilder) ForgetProjectImage(_ context.Context, slug string) error {
	delete(f.builds, slug)
	return nil
}
func (f *fakeBuilder) CheckGitBranch(_ context.Context, gitURL, branch string) (string, error) {
	f.checked++
	if !strings.HasPrefix(gitURL, "https://github.com/") {
		return "", errors.New("nicht zugelassen")
	}
	if branch != "" && branch != "main" {
		return "", errors.New("Branch nicht gefunden")
	}
	return strings.TrimSuffix(gitURL, ".git"), nil
}

func TestMaterial(t *testing.T) {
	ctx := context.Background()
	s := service()
	mgr := []Member{{Token: "user:a@dhbw.de", Role: RoleManager}}
	_, _ = s.create(ctx, Project{Slug: "kurs", Profiles: []string{"cpu"}, Members: mgr}, "admin")
	if _, err := s.Update(ctx, Project{Slug: "kurs", Members: mgr, MaterialGitURL: "https://github.com/o/kurs"}, "a@dhbw.de", false); !strings.Contains(reason(err), "nicht eingerichtet") {
		t.Fatalf("without builder: %v", err)
	}
	f := &fakeBuilder{builds: map[string]ImageBuild{}}
	s.SetImageBuilder(f)
	for _, bad := range []Project{{MaterialGitURL: "https://evil.example/o/kurs"}, {MaterialGitURL: "https://github.com/o/kurs", MaterialGitRef: "v1"}} {
		bad.Slug, bad.Members = "kurs", mgr
		if _, err := s.Update(ctx, bad, "a@dhbw.de", false); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	// A manager sets the material next to a free image; it is stored in its canonical form, and no build starts.
	out, err := s.Update(ctx, Project{Slug: "kurs", Members: mgr, MaterialGitURL: " https://github.com/o/kurs.git ", MaterialGitRef: "main"}, "a@dhbw.de", false)
	if err != nil || out.MaterialGitURL != "https://github.com/o/kurs" || out.MaterialGitRef != "main" || f.started != 0 {
		t.Fatalf("material: %+v %v", out, err)
	}
	// Unchanged material is not checked again; removing it drops the branch too.
	checked := f.checked
	if _, err := s.Update(ctx, out, "a@dhbw.de", false); err != nil || f.checked != checked {
		t.Fatalf("checked again: %v %d", err, f.checked)
	}
	out.MaterialGitURL = ""
	if out, err = s.Update(ctx, out, "a@dhbw.de", false); err != nil || out.MaterialGitRef != "" {
		t.Fatalf("removed: %+v %v", out, err)
	}
}

func TestProjectImageFinished(t *testing.T) {
	ctx := context.Background()
	s := service()
	checked := 0
	s.SetImageInspector(func(_ context.Context, image string) (RegistryImage, error) {
		checked++
		switch image {
		case "ghcr.io/kurs/ml:1":
			return RegistryImage{Digest: "sha256:m1", Labels: map[string]string{BaseLabel: "notebook-v1"}}, nil
		case "ghcr.io/kurs/fremd:1":
			return RegistryImage{Digest: "sha256:f1", Labels: map[string]string{}}, nil
		}
		return RegistryImage{}, errors.New("not found")
	})
	_, _ = s.create(ctx, Project{Slug: "kurs", Profiles: []string{"cpu"}}, "admin")
	for image, want := range map[string]string{"ghcr.io/kurs/weg:1": "nicht abrufbar", "Kein Image": "kein Image-Name"} {
		if _, err := s.Update(ctx, Project{Slug: "kurs", ProjectImage: image}, "manager@dhbw.de", false); !strings.Contains(reason(err), want) {
			t.Errorf("%s: %v", image, err)
		}
	}
	// Any image is taken; one not derived from a base runs without the GPU kernel.
	out, err := s.Update(ctx, Project{Slug: "kurs", ProjectImage: "ghcr.io/kurs/fremd:1"}, "manager@dhbw.de", false)
	if err != nil || out.ProjectImage != "ghcr.io/kurs/fremd:1" || out.Derived() || s.EffectiveImage(out) != "ghcr.io/kurs/fremd:1@sha256:f1" {
		t.Fatalf("foreign image: %+v %v", out, err)
	}
	out, err = s.Update(ctx, Project{Slug: "kurs", ProjectImage: " ghcr.io/kurs/ml:1 "}, "manager@dhbw.de", false)
	if err != nil || out.ProjectImage != "ghcr.io/kurs/ml:1" || !out.Derived() {
		t.Fatalf("derived image: %+v %v", out, err)
	}
	n := checked
	if _, err := s.Update(ctx, Project{Slug: "kurs", ProjectImage: "ghcr.io/kurs/ml:1"}, "manager@dhbw.de", false); err != nil || checked != n {
		t.Errorf("unchanged image: %v, checked %d times", err, checked-n)
	}
	if out, err := s.Update(ctx, Project{Slug: "kurs"}, "manager@dhbw.de", false); err != nil || out.ProjectImage != "" || s.EffectiveImage(out) != "reg/platform/base-pytorch:v2" || s.ImageLabel(out) != "PyTorch" {
		t.Errorf("cleared: %+v %v", out, err)
	}
	if _, err := s.Update(ctx, Project{Slug: "kurs", ImageBase: "cobol"}, "manager@dhbw.de", false); !strings.Contains(reason(err), "Basis") {
		t.Errorf("unknown base: %v", err)
	}
}

// A finished image runs at its digest; the periodic check offers a moved tag, takes it with automatic updates, and keeps an unreachable image out of the prepull.
func TestCheckProjectImages(t *testing.T) {
	ctx := context.Background()
	s := service()
	digest := map[string]RegistryImage{"ghcr.io/kurs/ml:latest": {Digest: "sha256:a", Labels: map[string]string{BaseLabel: "notebook-v1"}}}
	s.SetImageInspector(func(_ context.Context, image string) (RegistryImage, error) {
		img, ok := digest[image]
		if !ok {
			return RegistryImage{}, errors.New("not found")
		}
		return img, nil
	})
	_, _ = s.create(ctx, Project{Slug: "kurs", Profiles: []string{"cpu"}}, "admin")
	out, err := s.Update(ctx, Project{Slug: "kurs", ProjectImage: "ghcr.io/kurs/ml:latest"}, "manager@dhbw.de", false)
	if err != nil || out.ImageDigest != "sha256:a" || s.EffectiveImage(out) != "ghcr.io/kurs/ml:latest@sha256:a" {
		t.Fatalf("pinned: %+v %v", out, err)
	}

	// The tag moves to an image without the label: offered, not taken; the servers keep running the old one.
	digest["ghcr.io/kurs/ml:latest"] = RegistryImage{Digest: "sha256:b", Labels: map[string]string{}}
	if err := s.CheckProjectImages(ctx); err != nil {
		t.Fatal(err)
	}
	p, _ := s.Get(ctx, "kurs")
	if p.ImageUpdate != "sha256:b" || p.ImageDigest != "sha256:a" || !p.Derived() {
		t.Fatalf("offered: %+v", p)
	}
	// Saving the project unchanged keeps the offer.
	if out, _ := s.Update(ctx, Project{Slug: "kurs", ProjectImage: "ghcr.io/kurs/ml:latest", Description: "neu"}, "manager@dhbw.de", false); out.ImageUpdate != "sha256:b" || out.ImageDigest != "sha256:a" {
		t.Errorf("unchanged save: %+v", out)
	}
	// Taken: the new digest, and its label is checked again.
	if p, err = s.UpdateProjectImage(ctx, "kurs", "manager@dhbw.de"); err != nil || p.ImageDigest != "sha256:b" || p.ImageUpdate != "" || p.Derived() {
		t.Fatalf("updated: %+v %v", p, err)
	}

	// With automatic updates the check takes a moved tag itself.
	p.ImageAutoUpdate = true
	if _, err := s.Update(ctx, p, "manager@dhbw.de", false); err != nil {
		t.Fatal(err)
	}
	digest["ghcr.io/kurs/ml:latest"] = RegistryImage{Digest: "sha256:c", Labels: map[string]string{BaseLabel: "notebook-v1"}}
	_ = s.CheckProjectImages(ctx)
	if p, _ = s.Get(ctx, "kurs"); p.ImageDigest != "sha256:c" || p.ImageUpdate != "" || !p.Derived() {
		t.Fatalf("auto update: %+v", p)
	}

	// A project from before the pinning gets its digest from the check.
	_ = s.store.Create(ctx, &Project{Slug: "alt", Profiles: []string{"cpu"}, ProjectImage: "ghcr.io/kurs/ml:latest"})
	_ = s.CheckProjectImages(ctx)
	if p, _ := s.Get(ctx, "alt"); p.ImageDigest != "sha256:c" || p.ImageUpdate != "" {
		t.Errorf("old project: %+v", p)
	}

	// An image whose registry no longer answers stays with its digest, but leaves the prepull until it answers again.
	delete(digest, "ghcr.io/kurs/ml:latest")
	if err := s.CheckProjectImages(ctx); err == nil {
		t.Error("unreachable registry not reported")
	}
	if images, _ := s.ProjectImages(ctx); slices.ContainsFunc(images, func(i string) bool { return strings.HasPrefix(i, "ghcr.io/kurs/ml") }) {
		t.Errorf("unreachable image in the prepull: %v", images)
	}
	digest["ghcr.io/kurs/ml:latest"] = RegistryImage{Digest: "sha256:c"}
	_ = s.CheckProjectImages(ctx)
	if images, _ := s.ProjectImages(ctx); !slices.Contains(images, "ghcr.io/kurs/ml:latest@sha256:c") {
		t.Errorf("image back in the prepull: %v", images)
	}

	// Without a finished image there is nothing to update.
	if _, err := s.Update(ctx, Project{Slug: "kurs"}, "manager@dhbw.de", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateProjectImage(ctx, "kurs", "manager@dhbw.de"); !strings.Contains(reason(err), "kein fertiges Image") {
		t.Errorf("update without image: %v", err)
	}
}

func TestSplitProfileNeedsDerivedImage(t *testing.T) {
	ctx := context.Background()
	s := service()
	me := []string{"user:a@dhbw.de"}
	_, _ = s.create(ctx, Project{Slug: "fremd", Members: []Member{{Token: "user:a@dhbw.de"}}, Profiles: []string{"cpu", SplitProfile}, ProjectImage: "ghcr.io/x/y:1"}, "admin")
	if got := reason(s.CheckStart(ctx, me, StartRequest{Person: "a@dhbw.de", Project: "fremd", Profile: SplitProfile})); !strings.Contains(got, "Basen") {
		t.Fatalf("split with a foreign image: %q", got)
	}
	if got := reason(s.CheckStart(ctx, me, StartRequest{Person: "a@dhbw.de", Project: "fremd", Profile: "cpu"})); got != "" {
		t.Fatalf("cpu with a foreign image: %q", got)
	}
}

func TestMigrateProfiles(t *testing.T) {
	ctx := context.Background()
	s := service()
	st := s.store
	_ = st.Create(ctx, &Project{Slug: "tf", Profiles: []string{"cpu", "tensorflow", "cpu-gpukernel"}})
	_ = st.Create(ctx, &Project{Slug: "both", Profiles: []string{"pytorch", "tensorflow", "git-cpu"}})
	if err := s.MigrateProfiles(ctx); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.Get(ctx, "tf"); !slices.Equal(p.Profiles, []string{"cpu", "gpu", SplitProfile}) || p.ImageBase != "tensorflow" {
		t.Errorf("tf: %+v", p)
	}
	if p, _ := s.Get(ctx, "both"); !slices.Equal(p.Profiles, []string{"gpu", "cpu"}) || p.ImageBase != "" {
		t.Errorf("both: %+v", p)
	}
}

func TestProjectImageFromGit(t *testing.T) {
	ctx := context.Background()
	s := service()
	// A manager's update carries the members too (managers edit them).
	mgr := []Member{{Token: "user:a@dhbw.de", Role: RoleManager}}
	_, _ = s.create(ctx, Project{Slug: "kurs", Profiles: []string{"cpu"}, Members: mgr}, "admin")
	if _, err := s.Update(ctx, Project{Slug: "kurs", Members: mgr, ImageGitURL: "https://github.com/o/kurs"}, "a@dhbw.de", false); !strings.Contains(reason(err), "nicht eingerichtet") {
		t.Fatalf("without builder: %v", err)
	}
	f := &fakeBuilder{builds: map[string]ImageBuild{}}
	s.SetImageBuilder(f)
	if _, err := s.Update(ctx, Project{Slug: "kurs", Members: mgr, ImageGitURL: "https://evil.example/o/kurs"}, "a@dhbw.de", false); !strings.Contains(reason(err), "nicht zugelassen") {
		t.Fatalf("bad repository: %v", err)
	}
	out, err := s.Update(ctx, Project{Slug: "kurs", Members: mgr, ImageBase: "tensorflow", ImageGitURL: "https://github.com/o/kurs", ProjectImage: "egal"}, "a@dhbw.de", false)
	if err != nil || out.ProjectImage != "" || f.started != 1 || f.bases[len(f.bases)-1] != "reg/platform/base-tensorflow:v2" || s.ImageLabel(out) != "TensorFlow + github.com/o/kurs" {
		t.Fatalf("building: %+v %v %d", out, err, f.started)
	}
	mine, _ := s.ForPerson(ctx, []string{"user:a@dhbw.de"}, "a@dhbw.de", false)
	if len(mine) != 1 || mine[0].ImageBuild == nil || mine[0].ImageBuild.Status != "building" {
		t.Fatalf("build state in the list: %+v", mine)
	}
	// The finished build becomes the project's image; one for another repository does not.
	_ = s.ProjectImageBuilt(ctx, "kurs", "https://github.com/o/anders", "reg/envs/x:1")
	_ = s.ProjectImageBuilt(ctx, "kurs", "https://github.com/o/kurs.git", "reg/envs/kurs:c-v1")
	if p, _ := s.Get(ctx, "kurs"); p.ProjectImage != "reg/envs/kurs:c-v1" {
		t.Fatalf("after build: %q", p.ProjectImage)
	}
	if imgs, _ := s.ProjectImages(ctx); len(imgs) != 1 || imgs[0] != "reg/envs/kurs:c-v1" {
		t.Fatalf("images %v", imgs)
	}
	if _, err := s.RebuildProjectImage(ctx, "kurs"); err != nil || f.started != 2 {
		t.Fatalf("rebuild: %v %d", err, f.started)
	}
	// Back to notebook-base: the builds are forgotten.
	if out, err := s.Update(ctx, Project{Slug: "kurs", Members: mgr}, "a@dhbw.de", false); err != nil || out.ProjectImage != "" || out.ImageGitURL != "" || len(f.builds) != 0 {
		t.Fatalf("cleared: %+v %v %v", out, err, f.builds)
	}
}

// A GPU kernel of a running "CPU + GPU-Kernel" server is no new server, but its GPU counts.
func TestCheckKernelStart(t *testing.T) {
	ctx := context.Background()
	me := []string{"user:a@dhbw.de"}
	s := service(Running{Server: "jupyter-a", Person: "a@dhbw.de", Project: "split", Profile: SplitProfile, CPU: 0.5, RAMGB: 2})
	_, _ = s.create(ctx, Project{Slug: "split", Members: []Member{{Token: "user:a@dhbw.de"}}, Profiles: []string{SplitProfile},
		GPUClasses: []string{"l4-3q"}, Limits: Limits{ServersPerPerson: 1, CPUPerPerson: 4, RAMGBPerPerson: 8, GPUsPerPerson: 1}}, "admin")
	kernel := StartRequest{Person: "a@dhbw.de", Project: "split", Profile: SplitProfile, Kernel: true, CPU: 1, RAMGB: 2, GPUs: 1}
	if got := reason(s.CheckStart(ctx, me, kernel)); got != "" {
		t.Errorf("kernel next to its server refused: %s", got)
	}
	if got := reason(s.CheckStart(ctx, me, StartRequest{Person: "a@dhbw.de", Project: "split", Profile: SplitProfile, Server: "jupyter-a-2", CPU: 0.5, RAMGB: 2})); !strings.Contains(got, "Server") {
		t.Errorf("second server allowed: %q", got)
	}
	busy := service(Running{Person: "a@dhbw.de", Project: "split", Profile: SplitProfile, CPU: 0.5, RAMGB: 2},
		Running{Server: "", Person: "a@dhbw.de", Project: "split", Profile: "gpu-kernel", GPUClass: "l4-3q", CPU: 1, RAMGB: 2, GPUs: 1})
	_, _ = busy.create(ctx, Project{Slug: "split", Members: []Member{{Token: "user:a@dhbw.de"}}, Profiles: []string{SplitProfile},
		GPUClasses: []string{"l4-3q"}, Limits: Limits{ServersPerPerson: 1, CPUPerPerson: 4, RAMGBPerPerson: 8, GPUsPerPerson: 1}}, "admin")
	if got := reason(busy.CheckStart(ctx, me, kernel)); !strings.Contains(got, "GPU") {
		t.Errorf("second GPU kernel allowed: %q", got)
	}
}

// The highest role of the matching tokens counts: a course group as reader, its tutors as members.
func TestRoleOf(t *testing.T) {
	p := Project{Members: []Member{{Token: "group:wwi23seb", Role: RoleReader}, {Token: "user:tutor@dhbw.de", Role: RoleMember}, {Token: "user:prof@dhbw.de", Role: RoleManager}, {Token: "user:alt@dhbw.de"}}}
	for tokens, want := range map[string]string{
		"group:wwi23seb":                    RoleReader,
		"group:wwi23seb user:tutor@dhbw.de": RoleMember,
		"user:prof@dhbw.de group:wwi23seb":  RoleManager,
		"user:alt@dhbw.de":                  RoleMember,
		"user:x@dhbw.de":                    "",
	} {
		if got := p.RoleOf(strings.Fields(tokens)); got != want {
			t.Errorf("%s: %q, want %q", tokens, got, want)
		}
	}
}

// With several GPU classes the start names one, and only a granted one.
// A project whose environment is built from Git starts only once the build has an image: before, its servers would run in the bare base.
func TestCheckStartWaitsForTheImage(t *testing.T) {
	ctx := context.Background()
	me := []string{"user:a@dhbw.de"}
	s := service()
	_, _ = s.create(ctx, Project{Slug: "git", Name: "Git", Members: []Member{{Token: "user:a@dhbw.de"}}, Profiles: []string{"cpu"}, ImageGitURL: "https://github.com/x/y",
		Limits: Limits{ServersPerPerson: 2, CPUPerPerson: 4, RAMGBPerPerson: 16}}, "admin")
	err := s.CheckStart(ctx, me, StartRequest{Person: "a@dhbw.de", Project: "git", Profile: "cpu", CPU: 1, RAMGB: 2})
	if err == nil || !strings.Contains(err.Error(), "noch nicht gebaut") {
		t.Fatalf("without image: %v", err)
	}
	p, _ := s.store.Get(ctx, "git")
	p.ProjectImage = "registry/envs/git:abc"
	if err := s.store.Update(ctx, &p); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckStart(ctx, me, StartRequest{Person: "a@dhbw.de", Project: "git", Profile: "cpu", CPU: 1, RAMGB: 2}); err != nil {
		t.Fatalf("with image: %v", err)
	}
}

func TestCheckStartClass(t *testing.T) {
	ctx := context.Background()
	me := []string{"user:a@dhbw.de"}
	s := service()
	_, _ = s.create(ctx, Project{Slug: "zwei", Members: []Member{{Token: "user:a@dhbw.de"}}, Profiles: []string{"gpu"},
		GPUClasses: []string{"l4-3q", "general-medium"}, Limits: Limits{ServersPerPerson: 2, CPUPerPerson: 4, RAMGBPerPerson: 16, GPUsPerPerson: 1}}, "admin")
	_, _ = s.create(ctx, Project{Slug: "eins", Members: []Member{{Token: "user:a@dhbw.de"}}, Profiles: []string{"gpu"},
		GPUClasses: []string{"l4-3q"}, Limits: Limits{ServersPerPerson: 2, CPUPerPerson: 4, RAMGBPerPerson: 16, GPUsPerPerson: 1}}, "admin")
	for _, c := range []struct {
		req  StartRequest
		want string
	}{
		{StartRequest{Person: "a@dhbw.de", Project: "zwei", Profile: "gpu", GPUs: 1}, "GPU-Klasse wählen"},
		{StartRequest{Person: "a@dhbw.de", Project: "zwei", Profile: "gpu", GPUs: 1, GPUClass: "general-medium"}, ""},
		{StartRequest{Person: "a@dhbw.de", Project: "eins", Profile: "gpu", GPUs: 1}, ""},
		{StartRequest{Person: "a@dhbw.de", Project: "eins", Profile: "gpu", GPUs: 1, GPUClass: "general-medium"}, "General · medium ist im Projekt"},
	} {
		if got := reason(s.CheckStart(ctx, me, c.req)); (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%+v: %q, want %q", c.req, got, c.want)
		}
	}
}

// The switch to pools and tiers: a project's old class id becomes the new one everywhere, once.
func TestRenameGPUClass(t *testing.T) {
	ctx := context.Background()
	s := service()
	_, _ = s.create(ctx, Project{Slug: "alt", Members: []Member{{Token: "user:a@dhbw.de"}}, Profiles: []string{"gpu"}, GPUClasses: []string{"l4-3q"},
		GPUsByClass: map[string]int{"l4-3q": 2}, Budgets: []Budget{{Unit: "gpu_hours", GPUClass: "l4-3q", Window: "30d", Amount: 10}}, Limits: Limits{GPUsPerPerson: 1}}, "admin")
	if n, err := s.RenameGPUClass(ctx, "l4-3q", "general-small"); err != nil || n != 1 {
		t.Fatalf("renamed %d, %v", n, err)
	}
	p, _ := s.store.Get(ctx, "alt")
	if !slices.Equal(p.GPUClasses, []string{"general-small"}) || p.GPUsByClass["general-small"] != 2 || len(p.GPUsByClass) != 1 || p.Budgets[0].GPUClass != "general-small" {
		t.Errorf("project after rename: %+v", p)
	}
	if n, _ := s.RenameGPUClass(ctx, "l4-3q", "general-small"); n != 0 {
		t.Errorf("second rename changed %d projects", n)
	}
}
