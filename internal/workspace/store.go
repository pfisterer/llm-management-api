package workspace

import (
	"context"
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
	if err := db.AutoMigrate(&Project{}); err != nil {
		return nil, err
	}
	return &GormStore{db: db}, nil
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
