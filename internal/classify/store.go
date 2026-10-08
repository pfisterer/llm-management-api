package classify

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// MemoryStore is for development and tests.
type MemoryStore struct {
	mu sync.Mutex
	m  map[string]Result
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{m: map[string]Result{}} }

func (s *MemoryStore) Get(_ context.Context, model string) (Result, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.m[model]
	return r, ok, nil
}

func (s *MemoryStore) Put(_ context.Context, model string, r Result) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[model] = r
	return nil
}

// row is one stored result; the result itself as JSON, it is only ever read whole.
type row struct {
	Model     string `gorm:"primaryKey"`
	Result    string `gorm:"type:text;not null"`
	UpdatedAt time.Time
}

func (row) TableName() string { return "model_classifications" }

type GormStore struct{ db *gorm.DB }

func NewGormStore(db *gorm.DB) (*GormStore, error) {
	if err := db.AutoMigrate(&row{}); err != nil {
		return nil, err
	}
	return &GormStore{db: db}, nil
}

func (s *GormStore) Get(ctx context.Context, model string) (Result, bool, error) {
	var r row
	err := s.db.WithContext(ctx).First(&r, "model = ?", model).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, err
	}
	var res Result
	return res, true, json.Unmarshal([]byte(r.Result), &res)
}

func (s *GormStore) Put(ctx context.Context, model string, res Result) error {
	b, err := json.Marshal(res)
	if err != nil {
		return err
	}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).
		Create(&row{Model: model, Result: string(b), UpdatedAt: time.Now()}).Error
}

// Import reads the former Node broker's cache (classified.json: model -> result).
func Import(ctx context.Context, store Store, path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var m map[string]Result
	if err := json.Unmarshal(raw, &m); err != nil {
		return 0, err
	}
	for model, r := range m {
		if err := store.Put(ctx, model, r); err != nil {
			return 0, err
		}
	}
	return len(m), nil
}
