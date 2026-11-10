// Package background runs the service's periodic work so that one failing part does not take the rest with it, and reports what went wrong as metrics.
//
// Decided on 10.11.2026 (docs/refactoring-audit.md): a panic in a background loop is logged with its stack, counted (aiaas_background_panics_total, alert AiServiceProblems) and the loop goes on at its next tick; a startup step other than the database schema warns instead of stopping the service, and stays visible as aiaas_startup_step_failed until the next start.
package background

import (
	"context"
	"fmt"
	"maps"
	"runtime/debug"
	"slices"
	"sync"
	"time"

	"github.com/pfisterer/llm-management-api/internal/promtext"
	"go.uber.org/zap"
)

// Panics counts the panics recovered in background loops, per loop.
var Panics = promtext.NewCounterVec("aiaas_background_panics_total", "Panics recovered in a background loop of the KI-Dienst, per loop; the loop went on at its next tick.", "loop")

var (
	stepsMu sync.Mutex
	steps   = map[string]bool{} // startup step -> failed
)

// Step records the outcome of a startup step that does not stop the service: a failure is logged as a warning and shown as aiaas_startup_step_failed{step} 1 until the next start, a success as 0. It returns err.
func Step(log *zap.SugaredLogger, name string, err error) error {
	stepsMu.Lock()
	steps[name] = err != nil
	stepsMu.Unlock()
	if err != nil {
		log.Warnw("startup step failed, the service runs without it", "step", name, "error", err)
	}
	return err
}

// WriteMetrics writes aiaas_background_panics_total and aiaas_startup_step_failed.
func WriteMetrics(w *promtext.Writer) {
	Panics.WriteTo(w)
	stepsMu.Lock()
	defer stepsMu.Unlock()
	w.Header("aiaas_startup_step_failed", "gauge", "1 when a startup step of the KI-Dienst failed at its last start and the service runs without it (seed, rename, data migration); the log line \"startup step failed\" says why.")
	for _, name := range slices.Sorted(maps.Keys(steps)) {
		v := 0.0
		if steps[name] {
			v = 1
		}
		w.Sample("aiaas_startup_step_failed", v, "step", name)
	}
}

// guard runs fn and turns a panic into a log line and a count for loop.
func guard(log *zap.SugaredLogger, loop string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			Panics.Inc(loop)
			log.Errorw("background loop panicked, going on at its next tick", "loop", loop, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
		}
	}()
	fn()
}

// Schedule says when a loop ticks.
type Schedule struct {
	// Interval is the time between two ticks.
	Interval time.Duration
	// First is the pause before the first tick; 0 = right away.
	First time.Duration
	// Nudge, if set, makes the loop tick at once whenever it receives; the regular ticks go on as before.
	Nudge <-chan struct{}
}

// Run runs fn by s until ctx ends. A panic in fn is recovered, logged and counted, and the next tick runs as usual.
func Run(ctx context.Context, log *zap.SugaredLogger, loop string, s Schedule, fn func(ctx context.Context)) {
	Panics.Init(loop)
	go func() {
		t := time.NewTicker(s.Interval)
		defer t.Stop()
		if s.First > 0 {
			first := time.NewTimer(s.First)
			select {
			case <-ctx.Done():
				first.Stop()
				return
			case <-first.C:
			}
			t.Reset(s.Interval)
		}
		for {
			guard(log, loop, func() { fn(ctx) })
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			case <-s.Nudge:
			}
		}
	}()
}

// Every runs fn every interval until ctx ends: right away first if now is true, else first after one interval.
func Every(ctx context.Context, log *zap.SugaredLogger, loop string, interval time.Duration, now bool, fn func(ctx context.Context)) {
	s := Schedule{Interval: interval}
	if !now {
		s.First = interval
	}
	Run(ctx, log, loop, s, fn)
}
