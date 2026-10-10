package gpu

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"gorm.io/gorm"
)

// Status of an environment's image.
type Status string

const (
	StatusBuilding Status = "building"
	StatusReady    Status = "ready"
	StatusFailed   Status = "failed"
)

// Environment is a Git repository at a commit, built into an image (Harbor
// envs/<slug>:<commit>). Images are shared: two people adding the same
// repository and commit get the same image, built once.
type Environment struct {
	ID        uint      `json:"id" gorm:"primaryKey"`
	Owner     string    `json:"owner" gorm:"index;not null"`
	Name      string    `json:"name" gorm:"not null"`
	GitURL    string    `json:"git_url" gorm:"not null"`
	Ref       string    `json:"ref"`
	Branch    string    `json:"branch"`
	Commit    string    `json:"commit" gorm:"not null"`
	Image     string    `json:"image" gorm:"not null"`
	Status    Status    `json:"status" gorm:"not null"`
	JobName   string    `json:"-"`
	Message   string    `json:"message,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName pins the table name.
func (Environment) TableName() string { return "gpu_environments" }

// Store keeps the environments.
type Store interface {
	List(ctx context.Context, owner string) ([]Environment, error) // owner "" = all
	Get(ctx context.Context, id uint) (Environment, error)
	Save(ctx context.Context, e Environment) (Environment, error)
	Delete(ctx context.Context, id uint) error
}

type MemoryStore struct {
	mu     sync.Mutex
	envs   map[uint]Environment
	nextID uint
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{envs: map[uint]Environment{}, nextID: 1} }

func (m *MemoryStore) List(_ context.Context, owner string) ([]Environment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Environment
	for _, e := range m.envs {
		if owner == "" || e.Owner == owner {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

func (m *MemoryStore) Get(_ context.Context, id uint) (Environment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.envs[id]
	if !ok {
		return Environment{}, ErrNotFound
	}
	return e, nil
}

func (m *MemoryStore) Save(_ context.Context, e Environment) (Environment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	if e.ID == 0 {
		e.ID = m.nextID
		m.nextID++
		e.CreatedAt = now
	}
	e.UpdatedAt = now
	m.envs[e.ID] = e
	return e, nil
}

func (m *MemoryStore) Delete(_ context.Context, id uint) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.envs[id]; !ok {
		return ErrNotFound
	}
	delete(m.envs, id)
	return nil
}

type GormStore struct{ db *gorm.DB }

func NewGormStore(db *gorm.DB) (*GormStore, error) {
	if err := db.AutoMigrate(&Environment{}); err != nil {
		return nil, err
	}
	return &GormStore{db: db}, nil
}

func (g *GormStore) List(ctx context.Context, owner string) ([]Environment, error) {
	q := g.db.WithContext(ctx).Order("id desc")
	if owner != "" {
		q = q.Where("owner = ?", owner)
	}
	var out []Environment
	return out, q.Find(&out).Error
}

func (g *GormStore) Get(ctx context.Context, id uint) (Environment, error) {
	var e Environment
	err := g.db.WithContext(ctx).First(&e, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Environment{}, ErrNotFound
	}
	return e, err
}

func (g *GormStore) Save(ctx context.Context, e Environment) (Environment, error) {
	return e, g.db.WithContext(ctx).Save(&e).Error
}

func (g *GormStore) Delete(ctx context.Context, id uint) error {
	res := g.db.WithContext(ctx).Delete(&Environment{}, id)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}
