package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Two replicas (two services on one database): people taking the last place of a group at once get it once (Modify's row lock), the version a save answers with is the one read back (microseconds), and a stale version is refused. Needs a Postgres (TEST_POSTGRES_DSN), skipped without one.
func TestGormModify(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	open := func() *Service {
		db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard})
		if err != nil {
			t.Fatal(err)
		}
		st, err := NewGormStore(db)
		if err != nil {
			t.Fatal(err)
		}
		return NewService(st, Config{}, nil)
	}
	a := open()
	b := open()
	ctx := context.Background()
	_ = a.store.Delete(ctx, "t-modify")
	var members []Member
	for i := range 10 {
		members = append(members, Member{Token: fmt.Sprintf("user:p%d@dhbw.de", i)})
	}
	members = append(members, Member{Token: "user:m@dhbw.de", Role: RoleManager})
	if _, err := a.create(ctx, Project{Slug: "t-modify", Members: members, StorageGB: 10, GroupChoice: true, GroupMaxMembers: 1, Groups: []Group{{ID: "0000aaaa", Name: "g1"}}}, "test"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.store.Delete(ctx, "t-modify") })
	var wg sync.WaitGroup
	var mu sync.Mutex
	joined := 0
	for i := range 10 {
		s := a
		if i%2 == 1 {
			s = b
		}
		wg.Go(func() {
			if _, err := s.JoinGroup(ctx, nil, fmt.Sprintf("p%d@dhbw.de", i), "t-modify", "0000aaaa"); err == nil {
				mu.Lock()
				joined++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	p, err := a.Get(ctx, "t-modify")
	if err != nil || joined != 1 || len(p.Groups[0].Members) != 1 {
		t.Fatalf("%d joined, group %v, %v", joined, p.Groups[0].Members, err)
	}
	p.Description = "eins"
	saved, err := b.Update(ctx, p, "m@dhbw.de", false)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := a.Get(ctx, "t-modify"); !got.UpdatedAt.Equal(saved.UpdatedAt) {
		t.Fatalf("answered version %v, stored %v", saved.UpdatedAt, got.UpdatedAt)
	}
	saved.Description = "zwei"
	if _, err := a.Update(ctx, saved, "m@dhbw.de", false); err != nil {
		t.Fatalf("save with the answered version: %v", err)
	}
	if _, err := a.Update(ctx, p, "m@dhbw.de", false); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale version: %v", err)
	}
}
