package usage

import (
	"context"
	"math"
	"testing"
	"time"

	"go.uber.org/zap"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestCollectorBooksElapsedTime(t *testing.T) {
	st := NewMemoryStore()
	samples := []Sample{
		{Person: "a@dhbw.de", Profile: "cpu", CPU: 0.5, RAMGB: 2, CPUUsed: 0.25, RAMUsedGB: 1},
		{Person: "a@dhbw.de", Profile: "pytorch", GPUClass: "l4-3q", CPU: 1, RAMGB: 2, GPUs: 1},
		{Person: "b@dhbw.de", Profile: "cpu", CPU: 0.5, RAMGB: 2},
	}
	c := NewCollector(st, func(context.Context) ([]Sample, error) { return samples, nil },
		func(_ context.Context, p string) string { return map[string]string{"a@dhbw.de": "group:kurs"}[p] }, time.Minute, zap.NewNop().Sugar())
	now := time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	ctx := context.Background()
	// First tick: one interval. Then 30 s. Then a gap of an hour: counts as one interval, not as an hour.
	for _, step := range []time.Duration{0, 30 * time.Second, time.Hour} {
		now = now.Add(step)
		if err := c.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	booked := (2*time.Minute + 30*time.Second).Hours()
	rep, err := BuildReport(ctx, st, now, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Entries) != 3 {
		t.Fatalf("entries: %+v", rep.Entries)
	}
	byProfile := map[string]Total{}
	for _, e := range rep.Entries {
		byProfile[e.Person+"/"+e.Profile] = e
	}
	cpu := byProfile["a@dhbw.de/cpu"]
	if cpu.Project != "group:kurs" || !near(cpu.ServerHours, booked) || !near(cpu.VCPUHours, 0.5*booked) || !near(cpu.RAMGBHours, 2*booked) || !near(cpu.CPUUsedHours, 0.25*booked) {
		t.Fatalf("cpu row: %+v (want %v h)", cpu, booked)
	}
	if g := byProfile["a@dhbw.de/pytorch"]; g.GPUClass != "l4-3q" || !near(g.GPUHours, booked) {
		t.Fatalf("gpu row: %+v", g)
	}
	if b := byProfile["b@dhbw.de/cpu"]; b.Project != "" {
		t.Fatalf("person without project keeps the project the resolver gives: %+v", b)
	}
	if !near(rep.Total.ServerHours, 3*booked) || rep.Total.Days != 1 || len(rep.PerDay) != 1 {
		t.Fatalf("total: %+v per day %d", rep.Total, len(rep.PerDay))
	}
	// Own usage only.
	mine, _ := BuildReport(ctx, st, now, now, func(r Row) bool { return r.Person == "b@dhbw.de" })
	if len(mine.Entries) != 1 || !near(mine.Total.ServerHours, booked) {
		t.Fatalf("filtered: %+v", mine)
	}
}

func TestReportSpansDaysAndHome(t *testing.T) {
	st := NewMemoryStore()
	d1, d2 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	_ = st.Add(context.Background(), []Row{
		{Day: d1, Project: "p", Person: "a", Profile: "cpu", ServerHours: 2, VCPUHours: 1},
		{Day: d1, Project: "p", Person: "a", Profile: "cpu", ServerHours: 1, VCPUHours: 0.5, HomeGB: 3},
		{Day: d1, Project: "p", Person: "a", Profile: "cpu", HomeGB: 2},
		{Day: d2, Project: "p", Person: "a", Profile: "cpu", ServerHours: 1, VCPUHours: 0.5, HomeGB: 4},
	})
	rep, _ := BuildReport(context.Background(), st, d1, d2, nil)
	e := rep.Entries[0]
	if len(rep.Entries) != 1 || e.Days != 2 || !near(e.ServerHours, 4) || !near(e.VCPUHours, 2) || !near(e.HomeGBDays, 7) || !e.LastActive.Equal(d2) {
		t.Fatalf("entry: %+v", rep.Entries)
	}
	if len(rep.PerDay) != 2 || !near(rep.PerDay[0].ServerHours, 3) {
		t.Fatalf("per day: %+v", rep.PerDay)
	}
}

func TestHomes(t *testing.T) {
	list := `+--------------------------------------------+--------+---------+------+--------+-------+-------+
|                    Path                    |  Size  |   Used  | Use% | Inodes | IUsed | IUse% |
+--------------------------------------------+--------+---------+------+--------+-------+-------+
| /home-dennis-pfisterer-dhbw-de---c771a8ee  | 10 GiB | 1.5 GiB |  15% |      0 |   812 |    0% |
| /home-someone-dhbw-de---00000000           | 10 GiB | 512 MiB |   5% |      0 |    12 |    0% |
| /shared                                    | 50 GiB |  0 B    |   0% |      0 |     1 |    0% |
+--------------------------------------------+--------+---------+------+--------+-------+-------+`
	used, err := ParseQuotaList(list)
	if err != nil || len(used) != 3 || !near(used["home-dennis-pfisterer-dhbw-de---c771a8ee"], 1.5) || !near(used["home-someone-dhbw-de---00000000"], 0.5) {
		t.Fatalf("parsed: %v %v", used, err)
	}
	if _, err := ParseQuotaList("no table"); err == nil {
		t.Fatal("expected an error without a table")
	}
	st := NewMemoryStore()
	ctx := context.Background()
	now := time.Date(2026, 10, 11, 3, 0, 0, 0, time.UTC)
	_ = st.Add(ctx, []Row{{Day: Day(now), Project: "group:kurs", Person: "dennis.pfisterer@dhbw.de", Profile: "cpu", ServerHours: 1}})
	rows, err := HomeRows(ctx, st, used, func(context.Context, string) string { return "group:kurs" }, now)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows: %+v %v", rows, err)
	}
	by := map[string]Row{}
	for _, r := range rows {
		by[r.Person] = r
	}
	if r := by["dennis.pfisterer@dhbw.de"]; r.Profile != "home" || r.Project != "group:kurs" || !near(r.HomeGB, 1.5) {
		t.Fatalf("matched by hash: %+v", r)
	}
	if r := by["unbekannt: home-someone-dhbw-de---00000000"]; !near(r.HomeGB, 0.5) || r.Project != "" {
		t.Fatalf("unknown home still booked: %+v", by)
	}
	// Twice a day: the larger value stays, it is not added up.
	_ = st.Add(ctx, rows)
	_ = st.Add(ctx, rows)
	rep, _ := BuildReport(ctx, st, now, now, func(r Row) bool { return r.Profile == "home" && r.Person == "dennis.pfisterer@dhbw.de" })
	if !near(rep.Total.HomeGBDays, 1.5) {
		t.Fatalf("home GB-days: %+v", rep.Total)
	}
}
