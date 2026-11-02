package workspace

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pfisterer/llm-management-api/internal/usage"
)

func TestCheckJob(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	u := usage.NewMemoryStore()
	_ = u.Add(ctx, []usage.Row{{Day: usage.Day(now), Project: "kurs", Person: "a@dhbw.de", Profile: "job", GPUClass: "l4-3q", GPUHours: 3}})
	s := service()
	s.now = func() time.Time { return now }
	s.SetUsage(u)
	reserved := []Reservation{{Person: "b@dhbw.de", GPUClass: "l4-3q", GPUHours: 4}}
	s.SetJobReservations(func(context.Context, string) ([]Reservation, error) { return reserved, nil })
	p := Project{Slug: "kurs", Name: "Kurs", Members: []Member{{Token: "group:k"}}, Profiles: []string{"cpu"}, GPUClasses: []string{"l4-3q"}, StorageGB: 10,
		Limits: Limits{GPUsPerPerson: 1, JobsPerPerson: 2, JobMaxHours: 3}, GPUsByClass: map[string]int{"l4-3q": 1}, CreatedAt: now.AddDate(0, -1, 0),
		Budgets: []Budget{{Unit: "gpu_hours", Window: "30d", Amount: 12}}}
	if _, err := s.create(ctx, p, "admin"); err != nil {
		t.Fatal(err)
	}
	member := []string{"group:k"}

	g, err := s.CheckJob(ctx, member, JobRequest{Person: "a@dhbw.de", Project: "kurs", GPUs: 1})
	if err != nil {
		t.Fatal(err)
	}
	if g.GPUClass != "l4-3q" || g.MaxHours != 3 || g.Guaranteed {
		t.Errorf("grant %+v", g)
	}
	// Used 3 + reserved 4 + 3 = 10 fits into 12; a fourth hour more does not (the request's maximum is capped first).
	for name, c := range map[string]struct {
		tokens []string
		r      JobRequest
		want   string
	}{
		"no member":     {nil, JobRequest{Project: "kurs", GPUs: 1}, "kein Zugang"},
		"too many jobs": {member, JobRequest{Project: "kurs", GPUs: 1, Jobs: 2}, "2 Jobs gleichzeitig"},
		"too long":      {member, JobRequest{Project: "kurs", GPUs: 1, MaxHours: 4}, "Höchstlaufzeit"},
		"other class":   {member, JobRequest{Project: "kurs", GPUs: 1, GPUClass: "general-medium"}, "nicht freigegeben"},
		"two gpus":      {member, JobRequest{Project: "kurs", GPUs: 2}, "1 GPU der Klasse"},
	} {
		if _, err := s.CheckJob(ctx, c.tokens, c.r); !strings.Contains(reason(err), c.want) {
			t.Errorf("%s: %q, want %q", name, reason(err), c.want)
		}
	}
	reserved = append(reserved, Reservation{Person: "a@dhbw.de", GPUClass: "l4-3q", GPUHours: 3})
	if _, err := s.CheckJob(ctx, member, JobRequest{Person: "a@dhbw.de", Project: "kurs", GPUs: 1}); !strings.Contains(reason(err), "vorgemerkt 7") {
		t.Errorf("budget with reservations: %q", reason(err))
	}
	// Reservations count as used for starts and Enforce too.
	st, _ := s.BudgetStatus(ctx, p, "a@dhbw.de")
	if len(st) != 1 || st[0].Used != 3 || st[0].Reserved != 7 || st[0].Exhausted {
		t.Errorf("status %+v", st)
	}
	reserved = append(reserved, Reservation{Person: "b@dhbw.de", GPUClass: "l4-3q", GPUHours: 2})
	if st, _ := s.BudgetStatus(ctx, p, "a@dhbw.de"); !st[0].Exhausted {
		t.Errorf("used 3 + reserved 9 of 12 is not exhausted: %+v", st)
	}

	// Without batch jobs, without storage: refused.
	p.Slug, p.Limits.JobsPerPerson = "ohne", 0
	_, _ = s.create(ctx, p, "admin")
	if _, err := s.CheckJob(ctx, member, JobRequest{Project: "ohne", GPUs: 1}); !strings.Contains(reason(err), "keine Batch-Jobs") {
		t.Errorf("no jobs: %q", reason(err))
	}
	p.Slug, p.Limits.JobsPerPerson, p.StorageGB = "leer", 1, 0
	_, _ = s.create(ctx, p, "admin")
	if _, err := s.CheckJob(ctx, member, JobRequest{Project: "leer", GPUs: 1}); !strings.Contains(reason(err), "Workspace-Speicher") {
		t.Errorf("no storage: %q", reason(err))
	}

	qs, err := s.JobQueues(ctx)
	if err != nil || len(qs) != 2 || qs[0].GPUs != 1 {
		t.Errorf("queues %+v %v", qs, err)
	}
}

func TestJobLimitsValidated(t *testing.T) {
	s := service()
	for _, l := range []Limits{{JobsPerPerson: -1}, {JobMaxHours: 25}, {JobMaxHours: -1}} {
		p := Project{Slug: "x", Name: "x", Limits: l}
		if err := s.validate(&p); err == nil {
			t.Errorf("%+v accepted", l)
		}
	}
	p := Project{Slug: "x", Name: "x", BatchPriority: "hoch"}
	if err := s.validate(&p); err == nil {
		t.Errorf("batch priority %q accepted", p.BatchPriority)
	}
	if batchPriorityFromTree("guaranteed") != BatchGuaranteed || batchPriorityFromTree("preemptible") != "" {
		t.Errorf("tree values")
	}
}

// A running batch job counts neither for a person's GPUs nor for the project's GPUs of a class when starting a workspace: Kueue decides between them.
func TestJobsOutsideStartLimits(t *testing.T) {
	ctx := context.Background()
	s := service(Running{Server: "j-1", Person: "a@dhbw.de", Project: "kurs", Profile: JobProfile, GPUClass: "l4-3q", GPUs: 1})
	p := Project{Slug: "kurs", Name: "Kurs", Members: []Member{{Token: "group:k"}}, Profiles: []string{"gpu"}, GPUClasses: []string{"l4-3q"},
		Limits: Limits{GPUsPerPerson: 1}, GPUsByClass: map[string]int{"l4-3q": 1}}
	if _, err := s.create(ctx, p, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckStart(ctx, []string{"group:k"}, StartRequest{Person: "a@dhbw.de", Project: "kurs", Profile: "gpu", GPUs: 1, Server: "w-1"}); err != nil {
		t.Errorf("start next to a job: %v", err)
	}
}

// Batch jobs from the budget tree replace the project's own, which nobody changes here then, and a job is granted with them without an admin; off from the tree is off. A tree from before (no value) leaves what the admins set.
func TestTreeJobs(t *testing.T) {
	ctx := context.Background()
	s := service()
	tp := TreeProject{Name: "Kurs", Members: []Member{{Token: "group:k", Role: RoleMember}}, Workspaces: true, StorageGB: 10,
		Budgets: []TreeBudget{{Unit: "gpu_hours", GPUClass: "l4-3q", Window: "30d", Amount: 100}}}
	st, err := s.UpsertFromTree(ctx, "p_kurs", tp)
	if err != nil {
		t.Fatal(err)
	}
	member := []string{"group:k"}
	// A tree from before: the admins decide, as before.
	p := mustGet(t, s, st.Slug)
	p.Limits.JobsPerPerson, p.Limits.JobMaxHours = 3, 8
	if out, err := s.Update(ctx, p, "admin@dhbw.de", true); err != nil || out.Limits.JobsPerPerson != 3 || out.TreeJobs {
		t.Fatalf("admin sets the jobs: %+v %v", out.Limits, err)
	}
	if _, err := s.UpsertFromTree(ctx, "p_kurs", tp); err != nil {
		t.Fatal(err)
	}
	if p = mustGet(t, s, st.Slug); p.Limits.JobsPerPerson != 3 || p.TreeJobs {
		t.Fatalf("a tree from before changed the admins' jobs: %+v %v", p.Limits, p.TreeJobs)
	}

	tp.BatchJobs = &TreeJobs{JobsPerPerson: 2, MaxHours: 30}
	if _, err := s.UpsertFromTree(ctx, "p_kurs", tp); err != nil {
		t.Fatal(err)
	}
	p = mustGet(t, s, st.Slug)
	if !p.TreeJobs || p.Limits.JobsPerPerson != 2 || p.Limits.JobMaxHours != MaxJobHours {
		t.Fatalf("from the tree: %v %+v", p.TreeJobs, p.Limits)
	}
	g, err := s.CheckJob(ctx, member, JobRequest{Project: st.Slug, GPUs: 1})
	if err != nil || g.MaxHours != MaxJobHours {
		t.Fatalf("grant with the tree's values: %+v %v", g, err)
	}
	if _, err := s.CheckJob(ctx, member, JobRequest{Project: st.Slug, GPUs: 1, Jobs: 2}); !strings.Contains(reason(err), "2 Jobs gleichzeitig") {
		t.Errorf("the tree's jobs per person: %q", reason(err))
	}
	// Nobody changes them here, not even an admin.
	p.Limits.JobsPerPerson, p.Limits.JobMaxHours, p.TreeJobs = 9, 1, false
	if out, err := s.Update(ctx, p, "admin@dhbw.de", true); err != nil || out.Limits.JobsPerPerson != 2 || out.Limits.JobMaxHours != MaxJobHours || !out.TreeJobs {
		t.Fatalf("admin changed the tree's jobs: %+v %v %v", out.Limits, out.TreeJobs, err)
	}
	// The budget has none: off.
	tp.BatchJobs = &TreeJobs{}
	_, _ = s.UpsertFromTree(ctx, "p_kurs", tp)
	if p = mustGet(t, s, st.Slug); !p.TreeJobs || p.Limits.JobsPerPerson != 0 || p.Limits.JobMaxHours != 0 {
		t.Fatalf("switched off: %v %+v", p.TreeJobs, p.Limits)
	}
	if _, err := s.CheckJob(ctx, member, JobRequest{Project: st.Slug, GPUs: 1}); !strings.Contains(reason(err), "keine Batch-Jobs") {
		t.Errorf("off from the tree: %q", reason(err))
	}
	if qs, _ := s.JobQueues(ctx); len(qs) != 0 {
		t.Errorf("no queue without jobs: %+v", qs)
	}
}
