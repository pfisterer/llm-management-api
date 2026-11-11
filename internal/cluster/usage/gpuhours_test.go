package usage

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"
)

// One minute with one GPU of one class: a server holds it, a kernel waits (class not known yet), the inference pool holds nothing; then an hour later only the pool.
func TestGPUCollector(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	samples := []GPUSample{
		{Capacity: map[string]float64{"l4-3q": 1}, Pods: []GPUPod{
			{GPUClass: "l4-3q", Project: "kurs", Person: "a@dhbw.de", Profile: "pytorch", GPUs: 1},
			{GPUClass: "unbekannt", Project: "kurs", Person: "b@dhbw.de", Profile: "gpu-kernel", GPUs: 1, Waiting: true},
		}},
		{Capacity: map[string]float64{"l4-3q": 1}, Inference: map[string]float64{"l4-3q": 1}},
	}
	i := 0
	c := NewGPUCollector(st, func(context.Context) (GPUSample, error) { s := samples[i]; return s, nil }, nil, time.Minute, zap.NewNop().Sugar())
	t0 := time.Date(2026, 10, 13, 16, 30, 0, 0, time.UTC)
	c.now = func() time.Time { return t0 }
	if err := c.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	c.now = func() time.Time { return t0.Add(time.Minute) }
	if err := c.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	i = 1
	c.now = func() time.Time { return t0.Add(time.Hour) }
	if err := c.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	r, err := BuildGPUReport(ctx, st, Day(t0), Day(t0).AddDate(0, 0, 1), nil, true)
	if err != nil || len(r.Hours) != 2 {
		t.Fatalf("report %+v %v", r, err)
	}
	h := r.Hours[0]
	if !h.Hour.Equal(Hour(t0)) || !near(h.SampledHours, 2.0/60) || !near(h.Capacity, 2.0/60) || !near(h.WorkspaceHours, 2.0/60) || !near(h.WaitHours, 2.0/60) || h.MaxWaiting != 1 {
		t.Errorf("16:00 = %+v", h)
	}
	// After a gap longer than two intervals only one interval is booked.
	if h := r.Hours[1]; !near(h.InferenceHours, 1.0/60) || h.WorkspaceHours != 0 {
		t.Errorf("17:00 = %+v", h)
	}
	by := map[string]GPUHourPerson{}
	for _, p := range r.People {
		by[p.Person] = p
	}
	if p := by["b@dhbw.de"]; p.GPUClass != "l4-3q" || !near(p.WaitHours, 2.0/60) || p.GPUHours != 0 {
		t.Errorf("waiting kernel booked as %+v", p)
	}
	// A project's view: its people only, no class totals.
	r, _ = BuildGPUReport(ctx, st, Day(t0), Day(t0).AddDate(0, 0, 1), func(p GPUHourPerson) bool { return p.Project == "anderes" }, false)
	if len(r.Hours) != 0 || len(r.People) != 0 {
		t.Errorf("project view %+v", r)
	}
	// Old hours go.
	_ = st.PruneGPUHours(ctx, t0.Add(30*time.Minute))
	if r, _ := BuildGPUReport(ctx, st, Day(t0), Day(t0).AddDate(0, 0, 1), nil, true); len(r.Hours) != 1 {
		t.Errorf("after pruning %+v", r.Hours)
	}
}
