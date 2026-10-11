package usage

import (
	"context"
	"time"

	"go.uber.org/zap"
)

// Sample is one running workspace pod at the moment of sampling.
type Sample struct {
	Person    string
	Profile   string
	GPUClass  string  // empty without GPU
	CPU       float64 // requested cores
	RAMGB     float64 // requested GiB
	GPUs      float64
	CPUUsed   float64 // measured cores, 0 when unknown
	RAMUsedGB float64 // measured GiB
}

// Source lists the running workspace pods.
type Source func(ctx context.Context) ([]Sample, error)

// ProjectOf names the project a person's usage is booked to.
type ProjectOf func(ctx context.Context, person string) string

// Collector samples the workspaces and books the elapsed time.
type Collector struct {
	store    Store
	source   Source
	project  ProjectOf
	interval time.Duration
	now      func() time.Time
	last     time.Time
	log      *zap.SugaredLogger
}

func NewCollector(store Store, source Source, project ProjectOf, interval time.Duration, log *zap.SugaredLogger) *Collector {
	return &Collector{store: store, source: source, project: project, interval: interval, now: time.Now, log: log}
}

// Tick books the time since the previous tick for every running pod. A gap longer than two intervals (the service was down, a tick hung) counts as one interval: what ran in between is unknown, and booking all of it to whoever runs now would be wrong.
func (c *Collector) Tick(ctx context.Context) error {
	now := c.now()
	dt := c.interval
	if !c.last.IsZero() {
		if d := now.Sub(c.last); d < 2*c.interval {
			dt = d
		}
	}
	c.last = now
	samples, err := c.source(ctx)
	if err != nil {
		return err
	}
	h := dt.Hours()
	day := Day(now)
	rows := map[key]Row{}
	for _, s := range samples {
		r := Row{Day: day, Project: c.project(ctx, s.Person), Person: s.Person, Profile: s.Profile, GPUClass: s.GPUClass,
			ServerHours: h, VCPUHours: s.CPU * h, RAMGBHours: s.RAMGB * h, GPUHours: s.GPUs * h,
			CPUUsedHours: s.CPUUsed * h, RAMUsedGBHours: s.RAMUsedGB * h, UpdatedAt: now}
		k := keyOf(r)
		if cur, ok := rows[k]; ok {
			add(&cur, r)
			rows[k] = cur
		} else {
			rows[k] = r
		}
	}
	out := make([]Row, 0, len(rows))
	for _, r := range rows {
		out = append(out, r)
	}
	return c.store.Add(ctx, out)
}

// Run ticks until ctx ends. Errors are logged, not fatal: a minute without a sample costs a minute of accounting, not the service.
func (c *Collector) Run(ctx context.Context) {
	t := time.NewTicker(c.interval)
	defer t.Stop()
	for {
		if err := c.Tick(ctx); err != nil {
			c.log.Warnw("usage sample failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
