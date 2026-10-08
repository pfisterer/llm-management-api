package fleet

import (
	"context"
	"errors"
	"sort"
	"sync"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrPeerNotFound = errors.New("machine not found")

// Store keeps the registry.
//
// Update runs a read-modify-write over the whole registry under one lock (an
// exclusive table lock in Postgres). Enrolment needs that: the next free
// address and the name de-duplication look at all peers, and two machines
// enrolling at once must not both get the same address. The fleet is ~100
// machines enrolling hourly, so a coarse lock costs nothing measurable.
type Store interface {
	List(ctx context.Context) ([]Peer, error)
	// Update loads all peers, calls fn, and persists the peers fn returns as
	// changed (upsert) and the serials it returns as removed.
	Update(ctx context.Context, fn func(peers []Peer) (changed []Peer, removed []string, err error)) error
}

// ---------------------------------------------------------------- memory

type MemoryStore struct {
	mu    sync.Mutex
	peers map[string]Peer
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{peers: map[string]Peer{}} }

func (m *MemoryStore) List(_ context.Context) ([]Peer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshot(), nil
}

func (m *MemoryStore) snapshot() []Peer {
	out := make([]Peer, 0, len(m.peers))
	for _, p := range m.peers {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Serial < out[j].Serial })
	return out
}

func (m *MemoryStore) Update(_ context.Context, fn func([]Peer) ([]Peer, []string, error)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	changed, removed, err := fn(m.snapshot())
	if err != nil {
		return err
	}
	for _, p := range changed {
		m.peers[p.Serial] = p
	}
	for _, s := range removed {
		delete(m.peers, s)
	}
	return nil
}

// ---------------------------------------------------------------- postgres

type GormStore struct{ db *gorm.DB }

func NewGormStore(db *gorm.DB) (*GormStore, error) {
	if err := db.AutoMigrate(&Peer{}); err != nil {
		return nil, err
	}
	return &GormStore{db: db}, nil
}

func (g *GormStore) List(ctx context.Context) ([]Peer, error) {
	var out []Peer
	err := g.db.WithContext(ctx).Order("serial").Find(&out).Error
	return out, err
}

func (g *GormStore) Update(ctx context.Context, fn func([]Peer) ([]Peer, []string, error)) error {
	return g.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Serialises concurrent enrolments; readers (List) are not blocked.
		if err := tx.Exec("LOCK TABLE fleet_peers IN SHARE ROW EXCLUSIVE MODE").Error; err != nil {
			return err
		}
		var peers []Peer
		if err := tx.Order("serial").Find(&peers).Error; err != nil {
			return err
		}
		changed, removed, err := fn(peers)
		if err != nil {
			return err
		}
		if len(changed) > 0 {
			if err := tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&changed).Error; err != nil {
				return err
			}
		}
		if len(removed) > 0 {
			if err := tx.Where("serial IN ?", removed).Delete(&Peer{}).Error; err != nil {
				return err
			}
		}
		return nil
	})
}
