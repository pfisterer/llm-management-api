package workspace

import (
	"context"
	"strings"
	"testing"
)

func ip(v int) *int { return &v }

// releaseService has the inventory's runtime limits of today: CPU and "CPU + GPU-Kernel" 120 min / 12 h, GPU 30 min / 4 h.
func releaseService(running ...Running) *Service {
	s := NewService(NewMemoryStore(), Config{Profiles: []Profile{
		{Slug: "cpu", Name: "CPU", IdleMinutes: 120, MaxHours: 12},
		{Slug: SplitProfile, Name: "CPU + GPU-Kernel", GPU: true, IdleMinutes: 120, MaxHours: 12},
		{Slug: "gpu", Name: "GPU", GPU: true, IdleMinutes: 30, MaxHours: 4},
	}}, func(context.Context) ([]Running, error) { return running, nil })
	s.SetGPUClasses(func(context.Context) ([]GPUClass, error) { return []GPUClass{{ID: "l4-3q"}}, nil })
	return s
}

func TestReleaseDefaultsAreTodays(t *testing.T) {
	s := releaseService()
	e := s.EffectiveRelease(Project{Slug: "kurs"})
	want := map[string]ReleaseSeconds{"cpu": {7200, 7200, 43200}, SplitProfile: {7200, 7200, 43200}, "gpu": {1800, 1800, 14400}}
	for slug, w := range want {
		if e.Servers[slug] != w {
			t.Errorf("%s: %+v, want %+v", slug, e.Servers[slug], w)
		}
	}
	if e.Kernel != (ReleaseSeconds{300, 300, 14400}) {
		t.Errorf("kernel: %+v", e.Kernel)
	}
	// Without a budget's times a project may choose up to today's smallest per kind (the GPU kernel's 5 min for GPU), always with an upper bound.
	c := s.ReleaseCeiling(Project{})
	if *c.GPU.IdleOpenMinutes != 5 || *c.GPU.MaxHours != 4 || *c.CPU.IdleClosedMinutes != 120 || *c.CPU.MaxHours != 12 {
		t.Errorf("ceiling: cpu %+v gpu %+v", c.CPU, c.GPU)
	}
	// And no use gets more than today without the budget: a stored 20 min shortens nothing it should not and lengthens nothing.
	e = s.EffectiveRelease(Project{Slug: "kurs", Release: Release{GPU: ReleaseTimes{IdleOpenMinutes: ip(20)}}})
	if e.Kernel.IdleOpen != 300 || e.Servers["gpu"].IdleOpen != 1200 {
		t.Errorf("without budget: kernel %+v, gpu %+v", e.Kernel, e.Servers["gpu"])
	}
}

func TestReleaseInheritAndChoose(t *testing.T) {
	s := releaseService()
	p := Project{Slug: "kurs", ReleaseMax: Release{GPU: ReleaseTimes{IdleOpenMinutes: ip(60), IdleClosedMinutes: ip(10), MaxHours: ip(24)}}}
	e := s.EffectiveRelease(p)
	// Empty = the budget's, for GPU workspaces and GPU kernels alike; CPU keeps today's.
	if e.Kernel != (ReleaseSeconds{3600, 600, 86400}) || e.Servers["gpu"] != (ReleaseSeconds{3600, 600, 86400}) || e.Servers["cpu"] != (ReleaseSeconds{7200, 7200, 43200}) {
		t.Fatalf("inherited: %+v", e)
	}
	p.Release.GPU.IdleClosedMinutes = ip(3)
	if e := s.EffectiveRelease(p); e.Kernel.IdleClosed != 180 || e.Kernel.IdleOpen != 3600 {
		t.Fatalf("chosen: %+v", e.Kernel)
	}
	// A stored choice above a budget that shrank is capped where it is used.
	p.Release.GPU.MaxHours = ip(48)
	if e := s.EffectiveRelease(p); e.Kernel.Max != 86400 {
		t.Fatalf("capped: %+v", e.Kernel)
	}
}

func TestReleaseUnlimitedOnlyWithOwnGPUs(t *testing.T) {
	s := releaseService()
	shared := Project{Slug: "pool", GPUClasses: []string{"l4-3q"}, ReleaseMax: Release{GPU: ReleaseTimes{MaxHours: ip(0)}}, Release: Release{GPU: ReleaseTimes{MaxHours: ip(0)}}}
	if e := s.EffectiveRelease(shared); e.Kernel.Max != 4*3600 || e.Servers["gpu"].Max != 4*3600 {
		t.Fatalf("shared pool must keep an upper bound: %+v", e)
	}
	if err := checkReleaseMax(shared); !strings.Contains(reason(err), "eigenen GPUs") {
		t.Fatalf("budget without upper bound in the shared pool: %v", err)
	}
	own := shared
	own.GPUsByClass = map[string]int{"l4-3q": 2}
	if err := checkReleaseMax(own); err != nil {
		t.Fatal(err)
	}
	if e := s.EffectiveRelease(own); e.Kernel.Max != 0 || e.Servers["gpu"].Max != 0 || e.Servers["cpu"].Max != 12*3600 {
		t.Fatalf("own GPUs: %+v", e)
	}
	// The project inherits no upper bound, or chooses one.
	own.Release = Release{}
	if e := s.EffectiveRelease(own); e.Kernel.Max != 0 {
		t.Fatalf("inherited unlimited: %+v", e.Kernel)
	}
	own.Release.GPU.MaxHours = ip(8)
	if e := s.EffectiveRelease(own); e.Kernel.Max != 8*3600 {
		t.Fatalf("chosen bound: %+v", e.Kernel)
	}
	if err := checkReleaseMax(Project{ReleaseMax: Release{CPU: ReleaseTimes{MaxHours: ip(0)}}, GPUsByClass: map[string]int{"l4-3q": 1}, GPUClasses: []string{"l4-3q"}}); !strings.Contains(reason(err), "CPU") {
		t.Fatalf("CPU never without upper bound: %v", err)
	}
}

func TestReleaseUpdate(t *testing.T) {
	ctx := context.Background()
	s := releaseService()
	_, _ = s.create(ctx, Project{Slug: "kurs", Profiles: []string{"cpu", "gpu"}, GPUClasses: []string{"l4-3q"}}, "admin")
	// A manager chooses within today's times, but cannot set the budget's.
	out, err := s.Update(ctx, Project{Slug: "kurs", Release: Release{GPU: ReleaseTimes{IdleClosedMinutes: ip(5)}}, ReleaseMax: Release{GPU: ReleaseTimes{IdleOpenMinutes: ip(600)}}}, "m@dhbw.de", false)
	if err != nil || *out.Release.GPU.IdleClosedMinutes != 5 || out.ReleaseMax.GPU.IdleOpenMinutes != nil {
		t.Fatalf("manager: %+v %v", out.Release, err)
	}
	for _, bad := range []Release{
		{GPU: ReleaseTimes{IdleOpenMinutes: ip(31)}},
		{CPU: ReleaseTimes{MaxHours: ip(13)}},
		{GPU: ReleaseTimes{MaxHours: ip(0)}},
		{CPU: ReleaseTimes{IdleOpenMinutes: ip(0)}},
		{CPU: ReleaseTimes{IdleClosedMinutes: ip(MaxIdleMinutes + 1)}},
	} {
		if _, err := s.Update(ctx, Project{Slug: "kurs", Release: bad}, "m@dhbw.de", false); reason(err) == "" || strings.HasPrefix(reason(err), "ERROR") {
			t.Errorf("%+v: want a refusal, got %v", bad, err)
		}
	}
	// An admin sets the budget's while the tree sends none; then the project may go up to it.
	cur, _ := s.Get(ctx, "kurs")
	cur.ReleaseMax = Release{GPU: ReleaseTimes{IdleOpenMinutes: ip(90)}}
	cur.Release = Release{GPU: ReleaseTimes{IdleOpenMinutes: ip(90)}}
	if _, err := s.Update(ctx, cur, "admin", true); err != nil {
		t.Fatal(err)
	}
	// The budget shrinks below the project's choice: other changes still save.
	cur, _ = s.Get(ctx, "kurs")
	cur.ReleaseMax.GPU.IdleOpenMinutes = ip(45)
	if _, err := s.Update(ctx, cur, "admin", true); err != nil {
		t.Fatal(err)
	}
	cur, _ = s.Get(ctx, "kurs")
	cur.Description = "neu"
	if _, err := s.Update(ctx, cur, "m@dhbw.de", false); err != nil {
		t.Fatalf("stale choice blocks saving: %v", err)
	}
	if e := s.EffectiveRelease(cur); e.Servers["gpu"].IdleOpen != 45*60 {
		t.Fatalf("capped: %+v", e.Servers["gpu"])
	}
}

func TestReleaseFromTree(t *testing.T) {
	ctx := context.Background()
	s := releaseService()
	tp := TreeProject{Name: "Kurs", Workspaces: true, Budgets: []TreeBudget{{Unit: "gpu_hours", GPUClass: "l4-3q", Window: "30d", Amount: 10}},
		Release: &Release{GPU: ReleaseTimes{IdleOpenMinutes: ip(45), MaxHours: ip(0)}, CPU: ReleaseTimes{MaxHours: ip(0), IdleClosedMinutes: ip(9999)}}}
	st, err := s.UpsertFromTree(ctx, "p_1", tp)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := s.Get(ctx, st.Slug)
	// No upper bound needs GPUs of the project's own, CPU never has none, out-of-range is dropped: the sync does not fail over it.
	if !p.TreeRelease || *p.ReleaseMax.GPU.IdleOpenMinutes != 45 || p.ReleaseMax.GPU.MaxHours != nil || p.ReleaseMax.CPU.MaxHours != nil || p.ReleaseMax.CPU.IdleClosedMinutes != nil {
		t.Fatalf("from tree: %+v", p.ReleaseMax)
	}
	tp.GPUsByClass = map[string]int{"l4-3q": 1}
	if _, err := s.UpsertFromTree(ctx, "p_1", tp); err != nil {
		t.Fatal(err)
	}
	p, _ = s.Get(ctx, st.Slug)
	if p.ReleaseMax.GPU.MaxHours == nil || *p.ReleaseMax.GPU.MaxHours != 0 {
		t.Fatalf("own GPUs, no upper bound: %+v", p.ReleaseMax.GPU)
	}
	// Once the tree sends them, the budget's times are the tree's, also for admins.
	p.ReleaseMax.GPU.IdleOpenMinutes = ip(600)
	out, err := s.Update(ctx, p, "admin", true)
	if err != nil || *out.ReleaseMax.GPU.IdleOpenMinutes != 45 {
		t.Fatalf("admin over the tree: %+v %v", out.ReleaseMax.GPU, err)
	}
	// A tree from before (no release) keeps what is there.
	tp.Release = nil
	if _, err := s.UpsertFromTree(ctx, "p_1", tp); err != nil {
		t.Fatal(err)
	}
	if p, _ = s.Get(ctx, st.Slug); *p.ReleaseMax.GPU.IdleOpenMinutes != 45 {
		t.Fatalf("old tree: %+v", p.ReleaseMax.GPU)
	}
}

func TestGPUKernelsCountPerProject(t *testing.T) {
	ctx := context.Background()
	me := []string{"user:a@dhbw.de"}
	project := func(slug string) Project {
		return Project{Slug: slug, Members: []Member{{Token: "user:a@dhbw.de"}}, Profiles: []string{SplitProfile}, GPUClasses: []string{"l4-3q"}, Limits: Limits{GPUsPerPerson: 1}}
	}
	kernel := StartRequest{Person: "a@dhbw.de", Project: "kurs", Profile: SplitProfile, Kernel: true, GPUs: 1, GPUClass: "l4-3q"}
	// The project's limits decide (GPUs per person): a GPU kernel in another project does not count here.
	s := releaseService(Running{Server: "kernel-1", Person: "A@dhbw.de", Project: "anders", Profile: "gpu-kernel", GPUs: 1, GPUClass: "l4-3q"})
	_, _ = s.create(ctx, project("kurs"), "admin")
	if got := reason(s.CheckStart(ctx, me, kernel)); got != "" {
		t.Fatalf("kernel in another project counted: %q", got)
	}
	// One in the same project uses up its 1 GPU per person.
	s = releaseService(Running{Server: "kernel-2", Person: "a@dhbw.de", Project: "kurs", Profile: "gpu-kernel", GPUs: 1, GPUClass: "l4-3q"})
	_, _ = s.create(ctx, project("kurs"), "admin")
	if got := reason(s.CheckStart(ctx, me, kernel)); !strings.Contains(got, "1 GPU") {
		t.Fatalf("second kernel in the project: %q", got)
	}
}
