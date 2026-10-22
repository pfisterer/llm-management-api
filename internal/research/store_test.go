package research

import (
	"context"
	"os"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// The upsert in SQL merges like Merge: first values and the first end win, last_seen_at grows. Needs a Postgres (TEST_POSTGRES_DSN), skipped without one.
func TestGormStoreMerges(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	db.Exec("DROP TABLE IF EXISTS research_workloads")
	s, err := NewGormStore(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	prio := int32(-1)
	if err := s.Save(ctx, []Workload{{UID: "u", Kind: KindVLLM, Created: t0, AdmittedAt: at(time.Minute), Priority: &prio, LastSeenAt: t0.Add(time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	if open, err := s.Open(ctx); err != nil || len(open) != 1 {
		t.Fatalf("open: %+v %v", open, err)
	}
	for _, w := range []Workload{
		{UID: "u", Kind: KindGPUKernel, Model: "m", Created: t0, AdmittedAt: at(2 * time.Minute), ScheduledAt: at(3 * time.Minute), EndedAt: at(time.Hour), EndReason: EndPreempted, PreemptedBy: "p", QueuePosition: 3, LastSeenAt: t0.Add(30 * time.Minute)},
		{UID: "u", EndedAt: at(2 * time.Hour), EndReason: EndGone, QueuePosition: 1, LastSeenAt: t0},
	} {
		if err := s.Save(ctx, []Workload{w}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.Range(ctx, t0, t0.Add(time.Hour))
	if err != nil || len(rows) != 1 {
		t.Fatalf("range: %+v %v", rows, err)
	}
	w := rows[0]
	if w.Kind != KindVLLM || w.Model != "m" || !w.AdmittedAt.Equal(*at(time.Minute)) || !w.ScheduledAt.Equal(*at(3 * time.Minute)) || w.Priority == nil || *w.Priority != -1 {
		t.Fatalf("first values: %+v", w)
	}
	if w.EndReason != EndPreempted || w.PreemptedBy != "p" || !w.EndedAt.Equal(*at(time.Hour)) || w.QueuePosition != 3 || !w.LastSeenAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("first end, first queue, latest seen: %+v", w)
	}
	if err := s.Prune(ctx, t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if rows, _ := s.Range(ctx, t0, t0.Add(time.Hour)); len(rows) != 0 {
		t.Fatalf("pruned: %+v", rows)
	}
}
