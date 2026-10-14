package workspace

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pfisterer/llm-management-api/internal/usage"
	"go.uber.org/zap"
)

func TestBudgets(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	day := func(back int) time.Time { return usage.Day(now).AddDate(0, 0, -back) }
	u := usage.NewMemoryStore()
	_ = u.Add(ctx, []usage.Row{
		{Day: day(0), Project: "kurs", Person: "a@dhbw.de", Profile: "cpu", VCPUHours: 3, ServerHours: 6},
		{Day: day(5), Project: "kurs", Person: "a@dhbw.de", Profile: "cpu", VCPUHours: 10},
		{Day: day(20), Project: "kurs", Person: "a@dhbw.de", Profile: "cpu", VCPUHours: 30},
		{Day: day(40), Project: "kurs", Person: "a@dhbw.de", Profile: "cpu", VCPUHours: 100},
		{Day: day(1), Project: "kurs", Person: "b@dhbw.de", Profile: "gpu", GPUClass: "l4-3q", GPUHours: 4},
		{Day: day(1), Project: "anders", Person: "a@dhbw.de", Profile: "cpu", VCPUHours: 500},
	})
	s := service()
	s.now = func() time.Time { return now }
	s.SetUsage(u)
	created := now.AddDate(0, -2, 0)
	p := Project{Slug: "kurs", Name: "Kurs", Members: []Member{{Token: "group:k"}}, Profiles: []string{"cpu", "gpu"}, GPUClasses: []string{"l4-3q"},
		Limits: Limits{GPUsPerPerson: 1}, CreatedAt: created, Budgets: []Budget{
			{Unit: "vcpu_hours", Window: "24h", Amount: 5, PerPerson: true},
			{Unit: "vcpu_hours", Window: "7d", Amount: 20, PerPerson: true},
			{Unit: "vcpu_hours", Window: "30d", Amount: 50, PerPerson: true},
			{Unit: "vcpu_hours", Window: "total", Amount: 1000, PerPerson: true},
			{Unit: "gpu_hours", GPUClass: "l4-3q", Window: "30d", Amount: 4},
		}}
	st, err := s.BudgetStatus(ctx, p, "a@dhbw.de")
	if err != nil {
		t.Fatal(err)
	}
	want := []float64{3, 13, 43, 143, 4}
	for i, w := range want {
		if !near(st[i].Used, w) {
			t.Errorf("budget %d (%s %s): used %v, want %v", i, st[i].Unit, st[i].Window, st[i].Used, w)
		}
	}
	if st[0].Exhausted || st[2].Exhausted || !st[4].Exhausted {
		t.Fatalf("exhausted flags: %+v", st)
	}
	// The project-wide GPU budget is used up: every start in the project is refused, with the budget named.
	_ = s.store.Create(ctx, &p)
	got := reason(s.CheckStart(ctx, []string{"group:k"}, StartRequest{Person: "a@dhbw.de", Project: "kurs", Profile: "cpu"}))
	if !strings.Contains(got, "Budget") || !strings.Contains(got, "GPU-Stunden (l4-3q) in 30 Tagen für das ganze Projekt") {
		t.Fatalf("refusal: %q", got)
	}
	// ForPerson carries the status.
	mine, _ := s.ForPerson(ctx, []string{"group:k"}, "a@dhbw.de", false)
	if len(mine) != 1 || len(mine[0].BudgetStatus) != 5 {
		t.Fatalf("mine: %+v", mine)
	}
	// Enforce stops a's and b's servers in kurs (project budget), not the kernel and not other projects.
	var stopped []string
	s.Enforce(ctx, []RunningServer{
		{Running: Running{Person: "a@dhbw.de", Project: "kurs", Profile: "cpu"}, ServerName: ""},
		{Running: Running{Person: "a@dhbw.de", Project: "kurs", Profile: "gpu-kernel"}, ServerName: "k"},
		{Running: Running{Person: "b@dhbw.de", Project: "kurs", Profile: "gpu"}, ServerName: "gpu"},
		{Running: Running{Person: "a@dhbw.de", Project: "anders", Profile: "cpu"}, ServerName: "x"},
	}, func(_ context.Context, person, server string) error {
		stopped = append(stopped, person+"/"+server)
		return nil
	}, zap.NewNop().Sugar())
	if strings.Join(stopped, ",") != "a@dhbw.de/,b@dhbw.de/gpu" {
		t.Fatalf("stopped: %v", stopped)
	}
}

func near(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }
