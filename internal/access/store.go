package access

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"gorm.io/gorm"
)

var (
	ErrNotFound  = errors.New("access rule not found")
	ErrDuplicate = errors.New("an access rule for this token already exists")
)

// Store keeps the access list. Bootstrap rules are NOT stored here; the
// Service adds them on top.
type Store interface {
	List(ctx context.Context) ([]Rule, error)
	Get(ctx context.Context, id uint) (Rule, error)
	Create(ctx context.Context, r Rule) (Rule, error)
	Update(ctx context.Context, r Rule) (Rule, error)
	Delete(ctx context.Context, id uint) error
}

// ---------------------------------------------------------------- memory

// MemoryStore is for local development and tests; it forgets on restart.
type MemoryStore struct {
	mu     sync.Mutex
	rules  map[uint]Rule
	nextID uint
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{rules: map[uint]Rule{}, nextID: 1} }

func (m *MemoryStore) List(_ context.Context) ([]Rule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Rule, 0, len(m.rules))
	for _, r := range m.rules {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Token < out[j].Token })
	return out, nil
}

func (m *MemoryStore) Get(_ context.Context, id uint) (Rule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rules[id]
	if !ok {
		return Rule{}, ErrNotFound
	}
	return r, nil
}

func (m *MemoryStore) Create(_ context.Context, r Rule) (Rule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.rules {
		if existing.Token == r.Token {
			return Rule{}, ErrDuplicate
		}
	}
	r.ID = m.nextID
	m.nextID++
	r.UpdatedAt = time.Now().UTC()
	m.rules[r.ID] = r
	return r, nil
}

func (m *MemoryStore) Update(_ context.Context, r Rule) (Rule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rules[r.ID]; !ok {
		return Rule{}, ErrNotFound
	}
	for id, existing := range m.rules {
		if id != r.ID && existing.Token == r.Token {
			return Rule{}, ErrDuplicate
		}
	}
	r.UpdatedAt = time.Now().UTC()
	m.rules[r.ID] = r
	return r, nil
}

func (m *MemoryStore) Delete(_ context.Context, id uint) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rules[id]; !ok {
		return ErrNotFound
	}
	delete(m.rules, id)
	return nil
}

// ---------------------------------------------------------------- postgres

// GormStore keeps the access list in Postgres.
type GormStore struct{ db *gorm.DB }

// NewGormStore migrates the table and returns the store.
func NewGormStore(db *gorm.DB) (*GormStore, error) {
	if err := db.AutoMigrate(&Rule{}); err != nil {
		return nil, err
	}
	return &GormStore{db: db}, nil
}

func (g *GormStore) List(ctx context.Context) ([]Rule, error) {
	var out []Rule
	err := g.db.WithContext(ctx).Order("token").Find(&out).Error
	return out, err
}

func (g *GormStore) Get(ctx context.Context, id uint) (Rule, error) {
	var r Rule
	err := g.db.WithContext(ctx).First(&r, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Rule{}, ErrNotFound
	}
	return r, err
}

func (g *GormStore) Create(ctx context.Context, r Rule) (Rule, error) {
	r.ID = 0
	err := g.db.WithContext(ctx).Create(&r).Error
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return Rule{}, ErrDuplicate
	}
	return r, err
}

func (g *GormStore) Update(ctx context.Context, r Rule) (Rule, error) {
	res := g.db.WithContext(ctx).Model(&Rule{}).Where("id = ?", r.ID).Updates(map[string]any{
		"token": r.Token, "role": r.Role, "tier": r.Tier, "comment": r.Comment,
		"updated_by": r.UpdatedBy, "updated_at": time.Now().UTC(),
	})
	if errors.Is(res.Error, gorm.ErrDuplicatedKey) {
		return Rule{}, ErrDuplicate
	}
	if res.Error != nil {
		return Rule{}, res.Error
	}
	if res.RowsAffected == 0 {
		return Rule{}, ErrNotFound
	}
	return g.Get(ctx, r.ID)
}

func (g *GormStore) Delete(ctx context.Context, id uint) error {
	res := g.db.WithContext(ctx).Delete(&Rule{}, id)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}
