package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"

	"gorm.io/gorm"
)

// MemoryStore is for development and tests.
type MemoryStore struct {
	mu   sync.Mutex
	next uint
	m    map[string]Project
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{m: map[string]Project{}} }

func (s *MemoryStore) List(context.Context) ([]Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Project, 0, len(s.m))
	for _, p := range s.m {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out, nil
}

func (s *MemoryStore) Get(_ context.Context, slug string) (Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.m[slug]
	if !ok {
		return Project{}, ErrNotFound
	}
	return p, nil
}

func (s *MemoryStore) Create(_ context.Context, p *Project) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[p.Slug]; ok {
		return ErrExists
	}
	s.next++
	p.ID = s.next
	p.CreatedAt, p.UpdatedAt = time.Now().UTC(), time.Now().UTC()
	s.m[p.Slug] = *p
	return nil
}

func (s *MemoryStore) Update(_ context.Context, p *Project) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.m[p.Slug]
	if !ok {
		return ErrNotFound
	}
	p.ID, p.CreatedAt, p.CreatedBy, p.UpdatedAt = cur.ID, cur.CreatedAt, cur.CreatedBy, time.Now().UTC()
	s.m[p.Slug] = *p
	return nil
}

func (s *MemoryStore) Delete(_ context.Context, slug string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[slug]; !ok {
		return ErrNotFound
	}
	delete(s.m, slug)
	return nil
}

type GormStore struct{ db *gorm.DB }

func NewGormStore(db *gorm.DB) (*GormStore, error) {
	if err := db.AutoMigrate(&Project{}, &storageReport{}); err != nil {
		return nil, err
	}
	if err := migrateProjectGPUs(db); err != nil {
		return nil, err
	}
	if err := migrateMaterial(db); err != nil {
		return nil, err
	}
	return &GormStore{db: db}, nil
}

// migrateMaterial gives the projects built from Git their repository as course material (16.10.2026: until then the build's repository was checked out on its own, now the material is a field of its own). Once: the column is NULL only in rows from before it.
func migrateMaterial(db *gorm.DB) error {
	return db.Exec(`UPDATE workspace_projects SET material_git_url = COALESCE(image_git_url, ''), material_git_ref = COALESCE(image_git_ref, '') WHERE material_git_url IS NULL`).Error
}

// migrateProjectGPUs turns the one number of GPUs a project could use at the same time (Limits.GPUs, which GORM named limit_gp_us) into a number per GPU class: the same number for each class the project allows. A no-op once the column is gone.
func migrateProjectGPUs(db *gorm.DB) error {
	m := db.Migrator()
	if !m.HasColumn(&Project{}, "limit_gp_us") {
		return nil
	}
	type row struct {
		ID         uint
		LimitGPUs  int    `gorm:"column:limit_gp_us"`
		GPUClasses []byte `gorm:"column:gpu_classes"`
	}
	var rows []row
	if err := db.Table("workspace_projects").Select("id, limit_gp_us, gpu_classes").Where("limit_gp_us > 0").Scan(&rows).Error; err != nil {
		return err
	}
	for _, r := range rows {
		var classes []string
		_ = json.Unmarshal(r.GPUClasses, &classes)
		byClass := map[string]int{}
		for _, c := range classes {
			byClass[c] = r.LimitGPUs
		}
		raw, _ := json.Marshal(byClass)
		if err := db.Table("workspace_projects").Where("id = ?", r.ID).Update("gpus_by_class", string(raw)).Error; err != nil {
			return err
		}
	}
	return m.DropColumn(&Project{}, "limit_gp_us")
}

func (s *GormStore) List(ctx context.Context) ([]Project, error) {
	var out []Project
	err := s.db.WithContext(ctx).Order("slug").Find(&out).Error
	return out, err
}

func (s *GormStore) Get(ctx context.Context, slug string) (Project, error) {
	var p Project
	err := s.db.WithContext(ctx).First(&p, "slug = ?", slug).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Project{}, ErrNotFound
	}
	return p, err
}

func (s *GormStore) Create(ctx context.Context, p *Project) error {
	err := s.db.WithContext(ctx).Create(p).Error
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return ErrExists
	}
	return err
}

func (s *GormStore) Update(ctx context.Context, p *Project) error {
	cur, err := s.Get(ctx, p.Slug)
	if err != nil {
		return err
	}
	p.ID, p.CreatedAt, p.CreatedBy = cur.ID, cur.CreatedAt, cur.CreatedBy
	return s.db.WithContext(ctx).Save(p).Error
}

func (s *GormStore) Delete(ctx context.Context, slug string) error {
	res := s.db.WithContext(ctx).Where("slug = ?", slug).Delete(&Project{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// storageReport is the last storage report of one kind ("dirs", "valkey") as JSON, so a restart of the service does not lose what the hourly job measured.
type storageReport struct {
	Kind string `gorm:"primaryKey"`
	Body string `gorm:"type:text"`
	At   time.Time
}

func (storageReport) TableName() string { return "workspace_storage_reports" }

// ReportStore keeps the last storage reports; the GORM store has it, the memory store does not need it.
type ReportStore interface {
	SaveReport(ctx context.Context, kind, body string, at time.Time) error
	LoadReports(ctx context.Context) (map[string]storageReport, error)
}

func (s *GormStore) SaveReport(ctx context.Context, kind, body string, at time.Time) error {
	return s.db.WithContext(ctx).Save(&storageReport{Kind: kind, Body: body, At: at}).Error
}

func (s *GormStore) LoadReports(ctx context.Context) (map[string]storageReport, error) {
	var rows []storageReport
	if err := s.db.WithContext(ctx).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := map[string]storageReport{}
	for _, r := range rows {
		out[r.Kind] = r
	}
	return out, nil
}
