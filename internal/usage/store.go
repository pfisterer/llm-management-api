package usage

import (
	"context"
	"sort"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type key struct {
	day                              time.Time
	project, person, profile, gpuCls string
}

func keyOf(r Row) key { return key{r.Day, r.Project, r.Person, r.Profile, r.GPUClass} }

func add(dst *Row, r Row) {
	dst.ServerHours += r.ServerHours
	dst.VCPUHours += r.VCPUHours
	dst.RAMGBHours += r.RAMGBHours
	dst.GPUHours += r.GPUHours
	dst.CPUUsedHours += r.CPUUsedHours
	dst.RAMUsedGBHours += r.RAMUsedGBHours
	dst.HomeGB = max(dst.HomeGB, r.HomeGB)
}

// MemoryStore is for development and tests.
type MemoryStore struct {
	mu  sync.Mutex
	m   map[key]Row
	gpu gpuMem
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{m: map[key]Row{}} }

func (s *MemoryStore) Add(_ context.Context, rows []Row) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range rows {
		k := keyOf(r)
		cur, ok := s.m[k]
		if !ok {
			cur = Row{Day: r.Day, Project: r.Project, Person: r.Person, Profile: r.Profile, GPUClass: r.GPUClass}
		}
		add(&cur, r)
		s.m[k] = cur
	}
	return nil
}

func (s *MemoryStore) Range(_ context.Context, from, to time.Time) ([]Row, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Row{}
	for _, r := range s.m {
		if !r.Day.Before(from) && !r.Day.After(to) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day.Before(out[j].Day) })
	return out, nil
}

func (s *MemoryStore) Persons(context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	out := []string{}
	for k := range s.m {
		if !seen[k.person] {
			seen[k.person] = true
			out = append(out, k.person)
		}
	}
	return out, nil
}

type GormStore struct{ db *gorm.DB }

func NewGormStore(db *gorm.DB) (*GormStore, error) {
	if err := db.AutoMigrate(&Row{}); err != nil {
		return nil, err
	}
	if err := migrateGPUHours(db); err != nil {
		return nil, err
	}
	return &GormStore{db: db}, nil
}

// Add increments in the database, so a restart loses at most the minute in flight and two instances never overwrite each other.
func (s *GormStore) Add(ctx context.Context, rows []Row) error {
	if len(rows) == 0 {
		return nil
	}
	inc := func(col string) clause.Expr {
		return gorm.Expr("workspace_usage_daily." + col + " + excluded." + col)
	}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "day"}, {Name: "project"}, {Name: "person"}, {Name: "profile"}, {Name: "gpu_class"}},
		DoUpdates: clause.Assignments(map[string]any{
			"server_hours":      inc("server_hours"),
			"vcpu_hours":        inc("vcpu_hours"),
			"ram_gb_hours":      inc("ram_gb_hours"),
			"gpu_hours":         inc("gpu_hours"),
			"cpu_used_hours":    inc("cpu_used_hours"),
			"ram_used_gb_hours": inc("ram_used_gb_hours"),
			"home_gb":           gorm.Expr("GREATEST(workspace_usage_daily.home_gb, excluded.home_gb)"),
			"updated_at":        gorm.Expr("excluded.updated_at"),
		}),
	}).Create(&rows).Error
}

func (s *GormStore) Range(ctx context.Context, from, to time.Time) ([]Row, error) {
	var out []Row
	err := s.db.WithContext(ctx).Where("day BETWEEN ? AND ?", from, to).Order("day").Find(&out).Error
	return out, err
}

func (s *GormStore) Persons(ctx context.Context) ([]string, error) {
	var out []string
	err := s.db.WithContext(ctx).Model(&Row{}).Distinct().Pluck("person", &out).Error
	return out, err
}
