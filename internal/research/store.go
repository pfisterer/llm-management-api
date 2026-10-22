package research

import (
	"context"
	"sort"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// MemoryStore is for development and tests.
type MemoryStore struct {
	mu   sync.Mutex
	rows map[string]Workload
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{rows: map[string]Workload{}} }

func (s *MemoryStore) Save(_ context.Context, rows []Workload) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, w := range rows {
		cur := s.rows[w.UID]
		Merge(&cur, w)
		s.rows[w.UID] = cur
	}
	return nil
}

func (s *MemoryStore) Open(context.Context) ([]Workload, error) {
	return s.filter(func(w Workload) bool { return w.EndedAt == nil }), nil
}

func (s *MemoryStore) Range(_ context.Context, from, to time.Time) ([]Workload, error) {
	return s.filter(func(w Workload) bool { return !w.Created.Before(from) && w.Created.Before(to) }), nil
}

func (s *MemoryStore) Prune(_ context.Context, before time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, w := range s.rows {
		if w.Created.Before(before) {
			delete(s.rows, k)
		}
	}
	return nil
}

func (s *MemoryStore) filter(keep func(Workload) bool) []Workload {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Workload{}
	for _, w := range s.rows {
		if keep(w) {
			out = append(out, w)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Created.Equal(out[j].Created) {
			return out[i].Created.Before(out[j].Created)
		}
		return out[i].UID < out[j].UID
	})
	return out
}

type GormStore struct{ db *gorm.DB }

func NewGormStore(db *gorm.DB) (*GormStore, error) {
	if err := db.AutoMigrate(&Workload{}); err != nil {
		return nil, err
	}
	return &GormStore{db: db}, nil
}

// Save merges in the database as Merge does, so two instances or a restart never overwrite what the other knew first.
func (s *GormStore) Save(ctx context.Context, rows []Workload) error {
	if len(rows) == 0 {
		return nil
	}
	const t = "research_workloads."
	first := func(col string) clause.Expr { return gorm.Expr("COALESCE(" + t + col + ", excluded." + col + ")") }
	firstStr := func(col string) clause.Expr {
		return gorm.Expr("COALESCE(NULLIF(" + t + col + ", ''), excluded." + col + ")")
	}
	firstNonZero := func(col, cond string) clause.Expr {
		return gorm.Expr("CASE WHEN " + cond + " THEN excluded." + col + " ELSE " + t + col + " END")
	}
	set := map[string]any{
		"last_seen_at": gorm.Expr("GREATEST(" + t + "last_seen_at, excluded.last_seen_at)"),
		"updated_at":   gorm.Expr("excluded.updated_at"),
		"gpus":         firstNonZero("gpus", t+"gpus = 0"),
		"priority":     first("priority"),
	}
	for _, c := range []string{"workload_uid", "kind", "gpu_class", "flavor", "gpu_product", "node", "priority_class", "project", "person", "profile", "model"} {
		set[c] = firstStr(c)
	}
	for _, c := range []string{"admitted_at", "scheduled_at", "running_at", "ready_at"} {
		set[c] = first(c)
	}
	// The end is taken once, with its reason; the queue as first shown.
	for _, c := range []string{"ended_at", "end_reason", "end_detail", "end_estimated", "preempted_by"} {
		set[c] = firstNonZero(c, t+"ended_at IS NULL")
	}
	for _, c := range []string{"queue_position", "estimated_start", "queue_shown_at"} {
		set[c] = firstNonZero(c, t+"queue_position = 0")
	}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "uid"}}, DoUpdates: clause.Assignments(set)}).Create(&rows).Error
}

func (s *GormStore) Open(ctx context.Context) ([]Workload, error) {
	var out []Workload
	err := s.db.WithContext(ctx).Where("ended_at IS NULL").Find(&out).Error
	return out, err
}

func (s *GormStore) Range(ctx context.Context, from, to time.Time) ([]Workload, error) {
	var out []Workload
	err := s.db.WithContext(ctx).Where("created_at >= ? AND created_at < ?", from, to).Order("created_at, uid").Find(&out).Error
	return out, err
}

func (s *GormStore) Prune(ctx context.Context, before time.Time) error {
	return s.db.WithContext(ctx).Where("created_at < ?", before).Delete(&Workload{}).Error
}
