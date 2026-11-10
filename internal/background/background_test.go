package background

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pfisterer/llm-management-api/internal/promtext"
	"go.uber.org/zap"
)

var log = zap.NewNop().Sugar()

func TestEveryGoesOnAfterPanic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	done := make(chan struct{})
	Every(ctx, log, "test-every", 5*time.Millisecond, true, func(context.Context) {
		n := calls.Add(1)
		if n == 1 {
			panic("boom")
		}
		if n == 3 {
			close(done)
		}
	})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("loop stopped after the panic: %d calls", calls.Load())
	}
	if Panics.Value("test-every") != 1 {
		t.Fatalf("panics = %v, want 1", Panics.Value("test-every"))
	}
}

func TestEveryNotNowWaits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int32
	Every(ctx, log, "test-wait", time.Hour, false, func(context.Context) { calls.Add(1) })
	time.Sleep(20 * time.Millisecond)
	cancel()
	if calls.Load() != 0 {
		t.Fatal("ran before its first interval")
	}
}

func TestSuperviseRestarts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var runs atomic.Int32
	done := make(chan struct{})
	Supervise(ctx, log, "test-supervise", time.Millisecond, func(ctx context.Context) {
		if runs.Add(1) == 1 {
			panic("boom")
		}
		close(done)
		<-ctx.Done()
	})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("not restarted after the panic")
	}
	if Panics.Value("test-supervise") != 1 {
		t.Fatal("panic not counted")
	}
}

func TestStepAndMetrics(t *testing.T) {
	want := errors.New("seed failed")
	if err := Step(log, "seed_models", want); err != want {
		t.Fatal("Step must return err")
	}
	_ = Step(log, "seed_fleet", nil)
	var w promtext.Writer
	WriteMetrics(&w)
	out := string(w.Bytes())
	for _, s := range []string{`aiaas_startup_step_failed{step="seed_models"} 1`, `aiaas_startup_step_failed{step="seed_fleet"} 0`, `aiaas_background_panics_total{loop="test-wait"} 0`} {
		if !strings.Contains(out, s) {
			t.Errorf("missing %s in\n%s", s, out)
		}
	}
}
