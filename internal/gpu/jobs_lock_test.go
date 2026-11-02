package gpu

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Two replicas (two services on one database) submit to the same project at once: the advisory lock lets the second in only after the first's job is committed, another project does not wait. Needs a Postgres (TEST_POSTGRES_DSN), skipped without one.
func TestJobLockAcrossReplicas(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	open := func() *Service {
		db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard})
		if err != nil {
			t.Fatal(err)
		}
		st, err := NewGormJobStore(db)
		if err != nil {
			t.Fatal(err)
		}
		s := &Service{}
		s.SetJobStore(st)
		return s
	}
	a, b := open(), open()
	ctx := context.Background()
	project := "t-lock-" + time.Now().Format("150405.000000")
	inside := make(chan struct{})
	var mu sync.Mutex
	var order []string
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = a.LockProjectJobs(ctx, project, func(ctx context.Context) error {
			close(inside)
			time.Sleep(300 * time.Millisecond)
			mu.Lock()
			order = append(order, "a")
			mu.Unlock()
			return a.jobs.SaveJob(ctx, JobRecord{ID: "j-" + project, Project: project, State: JobQueued})
		})
	}()
	<-inside
	// Another project is not held up.
	start := time.Now()
	if err := b.LockProjectJobs(ctx, project+"-x", func(context.Context) error { return nil }); err != nil || time.Since(start) > 200*time.Millisecond {
		t.Errorf("other project waited %v: %v", time.Since(start), err)
	}
	if err := b.LockProjectJobs(ctx, project, func(ctx context.Context) error {
		live, err := b.jobs.Jobs(ctx, JobFilter{Project: project, Live: true})
		mu.Lock()
		order = append(order, "b")
		mu.Unlock()
		if err != nil || len(live) != 1 {
			t.Errorf("b does not see a's job: %d %v", len(live), err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if len(order) != 2 || order[0] != "a" {
		t.Errorf("order %v", order)
	}
}
