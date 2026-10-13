package usage

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// GPU use per hour (docs/bauplan.md, 6.10): how full the GPUs of a class were in each UTC hour, what for, and how long people waited for one. Sampled every minute like the daily rows; kept for GPUHourRetention, unlike the daily rows that are the accounting.

// GPUHourRetention is how long the hourly rows are kept: they name people with times, which the daily accounting does not need.
const GPUHourRetention = 365 * 24 * time.Hour

// GPUHour is one UTC hour of one GPU class. All amounts are GPU-hours summed over the minutes sampled: Capacity is what the class's GPUs could give in that time, so Workspace/Capacity is the share used by workspaces; WaitHours/SampledHours is the average number of pods waiting.
type GPUHour struct {
	Hour           time.Time `json:"hour" gorm:"primaryKey"`
	GPUClass       string    `json:"gpu_class" gorm:"primaryKey;column:gpu_class"`
	SampledHours   float64   `json:"sampled_hours" gorm:"column:sampled_hours"`     // time covered by samples (≤ 1)
	Capacity       float64   `json:"capacity" gorm:"column:capacity"`               // GPUs × hours available
	WorkspaceHours float64   `json:"workspace_hours" gorm:"column:workspace_hours"` // GPUs × hours held by workspaces (servers, GPU kernels)
	InferenceHours float64   `json:"inference_hours" gorm:"column:inference_hours"` // GPUs × hours held by the inference pool (vLLM)
	WaitHours      float64   `json:"wait_hours" gorm:"column:wait_hours"`           // waiting workspace pods × hours
	MaxWaiting     int       `json:"max_waiting" gorm:"column:max_waiting"`         // most pods waiting at one sample
	UpdatedAt      time.Time `json:"-"`
}

func (GPUHour) TableName() string { return "gpu_usage_hourly" }

// GPUHourPerson is one UTC hour of one person in one project, profile and GPU class: GPU-hours held and hours waited.
type GPUHourPerson struct {
	Hour      time.Time `json:"hour" gorm:"primaryKey"`
	GPUClass  string    `json:"gpu_class" gorm:"primaryKey;column:gpu_class"`
	Project   string    `json:"project" gorm:"primaryKey"`
	Person    string    `json:"person" gorm:"primaryKey"`
	Profile   string    `json:"profile" gorm:"primaryKey"` // "gpu-kernel" for GPU kernels
	GPUHours  float64   `json:"gpu_hours" gorm:"column:gpu_hours"`
	WaitHours float64   `json:"wait_hours" gorm:"column:wait_hours"`
	UpdatedAt time.Time `json:"-"`
}

func (GPUHourPerson) TableName() string { return "gpu_usage_hourly_person" }

// Hour is the UTC hour t falls in.
func Hour(t time.Time) time.Time { return t.UTC().Truncate(time.Hour) }

// GPUSample is the GPUs at one moment: the capacity per class, every workspace pod with or waiting for a GPU, and what the inference pool holds per class.
type GPUSample struct {
	Capacity  map[string]float64
	Pods      []GPUPod
	Inference map[string]float64
}

// GPUPod is a workspace pod with a GPU (Waiting = gated by Kueue or not yet on a node).
type GPUPod struct {
	GPUClass, Project, Person, Profile string
	GPUs                               float64
	Waiting                            bool
}

// GPUSource samples the GPUs.
type GPUSource func(ctx context.Context) (GPUSample, error)

// GPUStore keeps the hourly rows.
type GPUStore interface {
	AddGPUHours(ctx context.Context, hours []GPUHour, people []GPUHourPerson) error
	GPUHours(ctx context.Context, from, to time.Time) ([]GPUHour, []GPUHourPerson, error)
	PruneGPUHours(ctx context.Context, before time.Time) error
}

// GPUCollector books the GPU samples per hour.
type GPUCollector struct {
	store    GPUStore
	source   GPUSource
	project  ProjectOf
	interval time.Duration
	now      func() time.Time
	last     time.Time
	pruned   time.Time
	log      *zap.SugaredLogger
}

func NewGPUCollector(store GPUStore, source GPUSource, project ProjectOf, interval time.Duration, log *zap.SugaredLogger) *GPUCollector {
	return &GPUCollector{store: store, source: source, project: project, interval: interval, now: time.Now, log: log}
}

// Tick books the time since the previous tick (at most two intervals, as Collector.Tick) to the current hour.
func (c *GPUCollector) Tick(ctx context.Context) error {
	now := c.now()
	dt := c.interval
	if !c.last.IsZero() {
		if d := now.Sub(c.last); d < 2*c.interval {
			dt = d
		}
	}
	c.last = now
	s, err := c.source(ctx)
	if err != nil {
		return err
	}
	rows, people := BookGPUSample(ctx, s, Hour(now), dt.Hours(), c.project)
	for i := range rows {
		rows[i].UpdatedAt = now
	}
	for i := range people {
		people[i].UpdatedAt = now
	}
	if err := c.store.AddGPUHours(ctx, rows, people); err != nil {
		return err
	}
	if now.Sub(c.pruned) > 24*time.Hour {
		c.pruned = now
		if err := c.store.PruneGPUHours(ctx, now.Add(-GPUHourRetention)); err != nil {
			c.log.Warnw("gpu hours: pruning failed", "error", err)
		}
	}
	return nil
}

// BookGPUSample turns one sample into the rows of its hour, h hours long. A waiting pod whose class is not known yet (Kueue sets it on admission) counts for the only class there is, else for "unbekannt".
func BookGPUSample(ctx context.Context, s GPUSample, hour time.Time, h float64, project ProjectOf) ([]GPUHour, []GPUHourPerson) {
	byClass := map[string]*GPUHour{}
	row := func(class string) *GPUHour {
		r, ok := byClass[class]
		if !ok {
			r = &GPUHour{Hour: hour, GPUClass: class, SampledHours: h}
			byClass[class] = r
		}
		return r
	}
	for class, n := range s.Capacity {
		row(class).Capacity += n * h
	}
	for class, n := range s.Inference {
		row(class).InferenceHours += n * h
	}
	only := ""
	if len(s.Capacity) == 1 {
		for class := range s.Capacity {
			only = class
		}
	}
	waiting := map[string]int{}
	people := map[[4]string]*GPUHourPerson{}
	for _, p := range s.Pods {
		class := p.GPUClass
		if class == "" || class == "unbekannt" {
			class = only
			if class == "" {
				class = "unbekannt"
			}
		}
		proj := p.Project
		if proj == "" && project != nil {
			proj = project(ctx, p.Person)
		}
		k := [4]string{class, proj, p.Person, p.Profile}
		pr, ok := people[k]
		if !ok {
			pr = &GPUHourPerson{Hour: hour, GPUClass: class, Project: proj, Person: p.Person, Profile: p.Profile}
			people[k] = pr
		}
		r := row(class)
		if p.Waiting {
			waiting[class]++
			r.WaitHours += h
			pr.WaitHours += h
		} else {
			r.WorkspaceHours += p.GPUs * h
			pr.GPUHours += p.GPUs * h
		}
	}
	for class, n := range waiting {
		row(class).MaxWaiting = n
	}
	rows := make([]GPUHour, 0, len(byClass))
	for _, r := range byClass {
		rows = append(rows, *r)
	}
	out := make([]GPUHourPerson, 0, len(people))
	for _, p := range people {
		out = append(out, *p)
	}
	return rows, out
}

// Run ticks until ctx ends; errors are logged.
func (c *GPUCollector) Run(ctx context.Context) {
	t := time.NewTicker(c.interval)
	defer t.Stop()
	for {
		if err := c.Tick(ctx); err != nil {
			c.log.Warnw("gpu sample failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// GPUReport is the hourly GPU use of a period: per class and hour (admins only), and per person (for a project's managers only its rows).
type GPUReport struct {
	From   time.Time       `json:"from"`
	To     time.Time       `json:"to"`
	Hours  []GPUHour       `json:"hours"`
	People []GPUHourPerson `json:"people"`
}

// BuildGPUReport reads the hours of [from, to) sorted by hour and class; keep filters the person rows, classTotals=false leaves the hours out.
func BuildGPUReport(ctx context.Context, st GPUStore, from, to time.Time, keep func(GPUHourPerson) bool, classTotals bool) (GPUReport, error) {
	hours, people, err := st.GPUHours(ctx, from, to)
	if err != nil {
		return GPUReport{}, err
	}
	r := GPUReport{From: from, To: to, Hours: []GPUHour{}, People: []GPUHourPerson{}}
	if classTotals {
		r.Hours = hours
	}
	for _, p := range people {
		if keep == nil || keep(p) {
			r.People = append(r.People, p)
		}
	}
	sort.Slice(r.Hours, func(i, j int) bool {
		if !r.Hours[i].Hour.Equal(r.Hours[j].Hour) {
			return r.Hours[i].Hour.Before(r.Hours[j].Hour)
		}
		return r.Hours[i].GPUClass < r.Hours[j].GPUClass
	})
	sort.Slice(r.People, func(i, j int) bool {
		a, b := r.People[i], r.People[j]
		if !a.Hour.Equal(b.Hour) {
			return a.Hour.Before(b.Hour)
		}
		return strings.Compare(a.Person, b.Person) < 0
	})
	return r, nil
}

// --- stores ---

type gpuMem struct {
	mu     sync.Mutex
	hours  map[[2]string]GPUHour
	people map[[5]string]GPUHourPerson
}

func hourKey(h GPUHour) [2]string { return [2]string{h.Hour.Format(time.RFC3339), h.GPUClass} }
func personKey(p GPUHourPerson) [5]string {
	return [5]string{p.Hour.Format(time.RFC3339), p.GPUClass, p.Project, p.Person, p.Profile}
}

func (m *gpuMem) init() {
	if m.hours == nil {
		m.hours, m.people = map[[2]string]GPUHour{}, map[[5]string]GPUHourPerson{}
	}
}

func (s *MemoryStore) AddGPUHours(_ context.Context, hours []GPUHour, people []GPUHourPerson) error {
	m := &s.gpu
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	for _, h := range hours {
		k := hourKey(h)
		cur := m.hours[k]
		cur.Hour, cur.GPUClass = h.Hour, h.GPUClass
		cur.SampledHours += h.SampledHours
		cur.Capacity += h.Capacity
		cur.WorkspaceHours += h.WorkspaceHours
		cur.InferenceHours += h.InferenceHours
		cur.WaitHours += h.WaitHours
		cur.MaxWaiting = max(cur.MaxWaiting, h.MaxWaiting)
		m.hours[k] = cur
	}
	for _, p := range people {
		k := personKey(p)
		cur := m.people[k]
		cur.Hour, cur.GPUClass, cur.Project, cur.Person, cur.Profile = p.Hour, p.GPUClass, p.Project, p.Person, p.Profile
		cur.GPUHours += p.GPUHours
		cur.WaitHours += p.WaitHours
		m.people[k] = cur
	}
	return nil
}

func (s *MemoryStore) GPUHours(_ context.Context, from, to time.Time) ([]GPUHour, []GPUHourPerson, error) {
	m := &s.gpu
	m.mu.Lock()
	defer m.mu.Unlock()
	hours, people := []GPUHour{}, []GPUHourPerson{}
	for _, h := range m.hours {
		if !h.Hour.Before(from) && h.Hour.Before(to) {
			hours = append(hours, h)
		}
	}
	for _, p := range m.people {
		if !p.Hour.Before(from) && p.Hour.Before(to) {
			people = append(people, p)
		}
	}
	return hours, people, nil
}

func (s *MemoryStore) PruneGPUHours(_ context.Context, before time.Time) error {
	m := &s.gpu
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, h := range m.hours {
		if h.Hour.Before(before) {
			delete(m.hours, k)
		}
	}
	for k, p := range m.people {
		if p.Hour.Before(before) {
			delete(m.people, k)
		}
	}
	return nil
}

func migrateGPUHours(db *gorm.DB) error { return db.AutoMigrate(&GPUHour{}, &GPUHourPerson{}) }

// AddGPUHours increments in the database (two instances or a restart never overwrite each other); MaxWaiting keeps the larger value.
func (s *GormStore) AddGPUHours(ctx context.Context, hours []GPUHour, people []GPUHourPerson) error {
	inc := func(table, col string) clause.Expr { return gorm.Expr(table + "." + col + " + excluded." + col) }
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if len(hours) > 0 {
			t := "gpu_usage_hourly"
			if err := tx.Clauses(clause.OnConflict{
				Columns: []clause.Column{{Name: "hour"}, {Name: "gpu_class"}},
				DoUpdates: clause.Assignments(map[string]any{
					"sampled_hours":   inc(t, "sampled_hours"),
					"capacity":        inc(t, "capacity"),
					"workspace_hours": inc(t, "workspace_hours"),
					"inference_hours": inc(t, "inference_hours"),
					"wait_hours":      inc(t, "wait_hours"),
					"max_waiting":     gorm.Expr("GREATEST(" + t + ".max_waiting, excluded.max_waiting)"),
					"updated_at":      gorm.Expr("excluded.updated_at"),
				}),
			}).Create(&hours).Error; err != nil {
				return err
			}
		}
		if len(people) > 0 {
			t := "gpu_usage_hourly_person"
			if err := tx.Clauses(clause.OnConflict{
				Columns: []clause.Column{{Name: "hour"}, {Name: "gpu_class"}, {Name: "project"}, {Name: "person"}, {Name: "profile"}},
				DoUpdates: clause.Assignments(map[string]any{
					"gpu_hours":  inc(t, "gpu_hours"),
					"wait_hours": inc(t, "wait_hours"),
					"updated_at": gorm.Expr("excluded.updated_at"),
				}),
			}).Create(&people).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *GormStore) GPUHours(ctx context.Context, from, to time.Time) ([]GPUHour, []GPUHourPerson, error) {
	var hours []GPUHour
	var people []GPUHourPerson
	db := s.db.WithContext(ctx)
	if err := db.Where("hour >= ? AND hour < ?", from, to).Find(&hours).Error; err != nil {
		return nil, nil, err
	}
	if err := db.Where("hour >= ? AND hour < ?", from, to).Find(&people).Error; err != nil {
		return nil, nil, err
	}
	return hours, people, nil
}

func (s *GormStore) PruneGPUHours(ctx context.Context, before time.Time) error {
	db := s.db.WithContext(ctx)
	if err := db.Where("hour < ?", before).Delete(&GPUHour{}).Error; err != nil {
		return err
	}
	return db.Where("hour < ?", before).Delete(&GPUHourPerson{}).Error
}
