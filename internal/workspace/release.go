package workspace

import "reflect"

// Release times (docs/todo.md idle_release): when a workspace or a GPU kernel goes back, per kind of use (CPU, GPU). Counted is idle time, not runtime: nothing computes (no running cell, no CPU or GPU load of its kernels and processes), with an open tab or without one; a training keeps its GPU as long as it runs, up to the optional upper bound. The budget sets the most a project may have (ReleaseMax, from the budget tree), the project chooses within it (Release); an empty field takes the budget's value, and where the budget sets none, today's value of the kind of workspace (Profile.IdleMinutes and MaxHours from the inventory, 5 min and 4 h for a GPU kernel).

// ReleaseTimes are the three times of one kind of use; nil = not set (inherited).
type ReleaseTimes struct {
	// IdleOpenMinutes: minutes without activity while a tab of the workspace is open.
	IdleOpenMinutes *int `json:"idle_open_minutes,omitempty" example:"30"`
	// IdleClosedMinutes: minutes without activity once no tab is open any more (closed tabs, a closed laptop).
	IdleClosedMinutes *int `json:"idle_closed_minutes,omitempty" example:"10"`
	// MaxHours: hours after its start a workspace or GPU kernel ends at the latest, whatever it does; 0 = no upper bound, only for GPUs of the project's own (gpus_by_class), never in the shared pool.
	MaxHours *int `json:"max_hours,omitempty" example:"24"`
}

// Release are the release times for CPU (CPU workspaces and the notebook of "CPU + GPU-Kernel") and GPU (GPU workspaces and GPU kernels).
type Release struct {
	CPU ReleaseTimes `json:"cpu"`
	GPU ReleaseTimes `json:"gpu"`
}

// ReleaseSeconds is what is enforced for one workspace or kernel, in seconds; Max 0 = no upper bound.
type ReleaseSeconds struct {
	IdleOpen   int `json:"idle_open" example:"1800"`
	IdleClosed int `json:"idle_closed" example:"600"`
	Max        int `json:"max" example:"86400"`
}

// EffectiveRelease is what the hub and the GPU kernels of a project enforce: per profile for its servers, and for the GPU kernels of "CPU + GPU-Kernel".
type EffectiveRelease struct {
	Servers map[string]ReleaseSeconds `json:"servers"`
	Kernel  ReleaseSeconds            `json:"kernel"`
}

// Bounds of the release times: idle times from 1 minute to a day, upper bounds up to 30 days.
const (
	MaxIdleMinutes = 24 * 60
	MaxMaxHours    = 30 * 24
)

// Today's times of a GPU kernel (before idle_release they were fixed in the hub and in the kernel pod): 5 minutes without execution, 4 hours at the latest.
const (
	KernelIdleMinutes = 5
	KernelMaxHours    = 4
)

// Fallbacks for a profile without times in the catalogue (development): the inventory's values of the CPU and GPU profiles.
const (
	fallbackCPUIdle, fallbackCPUMax = 120, 12
	fallbackGPUIdle, fallbackGPUMax = 30, 4
)

// releaseKind is "gpu" for a profile whose server holds a GPU (GPU workspace), "cpu" for the others (the notebook of "CPU + GPU-Kernel" runs on a CPU node).
func releaseKind(p Profile) string {
	if p.GPU && p.Slug != SplitProfile {
		return "gpu"
	}
	return "cpu"
}

// times are resolved release times in minutes and hours; MaxHours 0 = no upper bound.
type times struct{ idleOpen, idleClosed, maxHours int }

// profileDefault is today's times of a profile: its idle time with and without tab, its upper bound.
func profileDefault(p Profile) times {
	idle, maxH := p.IdleMinutes, p.MaxHours
	if releaseKind(p) == "gpu" {
		if idle <= 0 {
			idle = fallbackGPUIdle
		}
		if maxH <= 0 {
			maxH = fallbackGPUMax
		}
	} else {
		if idle <= 0 {
			idle = fallbackCPUIdle
		}
		if maxH <= 0 {
			maxH = fallbackCPUMax
		}
	}
	return times{idle, idle, maxH}
}

var kernelDefault = times{KernelIdleMinutes, KernelIdleMinutes, KernelMaxHours}

// kindDefaults is what a project may choose per kind where the budget sets nothing: the smallest of today's times over the uses of the kind (and the GPU kernel for GPU), so without a budget no use gets longer than today (the GPU kernel stays at 5 min), never without an upper bound.
func (s *Service) kindDefaults(kind string) times {
	out := times{}
	if kind == "gpu" {
		out = kernelDefault
	}
	for _, p := range s.cfg.Profiles {
		if releaseKind(p) != kind {
			continue
		}
		d := profileDefault(p)
		if out.idleOpen == 0 {
			out = d
			continue
		}
		out = times{min(out.idleOpen, d.idleOpen), min(out.idleClosed, d.idleClosed), min(out.maxHours, d.maxHours)}
	}
	if out.idleOpen == 0 {
		if kind == "gpu" {
			out = times{fallbackGPUIdle, fallbackGPUIdle, fallbackGPUMax}
		} else {
			out = times{fallbackCPUIdle, fallbackCPUIdle, fallbackCPUMax}
		}
	}
	return out
}

// ownGPUs tells whether every GPU class of the project has a number of GPUs of its own (gpus_by_class): only then may its GPU times have no upper bound. In the shared pool somebody else waits for the GPU.
func ownGPUs(p Project) bool { return ProjectGPUCap(p) > 0 }

func (r Release) kind(kind string) ReleaseTimes {
	if kind == "gpu" {
		return r.GPU
	}
	return r.CPU
}

// ceiling is the most the project may choose of a kind: the budget's times where it sets them, today's smallest of the kind where not; MaxHours 0 only where the budget allows no upper bound and the project has GPUs of its own.
func (s *Service) ceiling(p Project, kind string) times {
	d := s.kindDefaults(kind)
	b := p.ReleaseMax.kind(kind)
	out := d
	if b.IdleOpenMinutes != nil {
		out.idleOpen = *b.IdleOpenMinutes
	}
	if b.IdleClosedMinutes != nil {
		out.idleClosed = *b.IdleClosedMinutes
	}
	if b.MaxHours != nil {
		out.maxHours = *b.MaxHours
	}
	if out.maxHours == 0 && (kind != "gpu" || !ownGPUs(p)) {
		out.maxHours = d.maxHours
	}
	return out
}

// resolve is one use's times: the project's choice, else the budget's, else today's value of the use (def); never above the budget's times, and where the budget sets none never above today's value of the use; no upper bound only where the ceiling has none.
func (s *Service) resolve(p Project, kind string, def times) times {
	c := s.ceiling(p, kind)
	own, budget := p.Release.kind(kind), p.ReleaseMax.kind(kind)
	pick := func(own, budget *int, def, ceil int, hours bool) int {
		if budget == nil {
			ceil = def // without the budget's say, no use gets more than today
		}
		v := def
		if budget != nil {
			v = *budget
		}
		if own != nil {
			v = *own
		}
		if hours {
			// 0 = no upper bound: only below a ceiling without one.
			if ceil == 0 {
				return v
			}
			if v == 0 || v > ceil {
				return ceil
			}
			return v
		}
		return max(1, min(v, ceil))
	}
	return times{
		idleOpen:   pick(own.IdleOpenMinutes, budget.IdleOpenMinutes, def.idleOpen, c.idleOpen, false),
		idleClosed: pick(own.IdleClosedMinutes, budget.IdleClosedMinutes, def.idleClosed, c.idleClosed, false),
		maxHours:   pick(own.MaxHours, budget.MaxHours, def.maxHours, c.maxHours, true),
	}
}

func (t times) seconds() ReleaseSeconds {
	return ReleaseSeconds{IdleOpen: t.idleOpen * 60, IdleClosed: t.idleClosed * 60, Max: t.maxHours * 3600}
}

// EffectiveRelease is what the project's servers (per profile it allows) and GPU kernels enforce.
func (s *Service) EffectiveRelease(p Project) EffectiveRelease {
	out := EffectiveRelease{Servers: map[string]ReleaseSeconds{}, Kernel: s.resolve(p, "gpu", kernelDefault).seconds()}
	for _, pr := range s.cfg.Profiles {
		out.Servers[pr.Slug] = s.resolve(p, releaseKind(pr), profileDefault(pr)).seconds()
	}
	return out
}

// ReleaseCeiling is the most the project may choose, per kind, for its settings: every time set; MaxHours 0 = no upper bound allowed.
func (s *Service) ReleaseCeiling(p Project) Release {
	conv := func(t times) ReleaseTimes {
		return ReleaseTimes{IdleOpenMinutes: &t.idleOpen, IdleClosedMinutes: &t.idleClosed, MaxHours: &t.maxHours}
	}
	return Release{CPU: conv(s.ceiling(p, "cpu")), GPU: conv(s.ceiling(p, "gpu"))}
}

var kindNames = map[string]string{"cpu": "CPU", "gpu": "GPU"}

// validRange refuses times outside the bounds: idle times 1 minute to a day, upper bounds 0 (none) to 30 days.
func validRange(r Release, what string) error {
	for _, kind := range []string{"cpu", "gpu"} {
		t := r.kind(kind)
		for _, v := range []*int{t.IdleOpenMinutes, t.IdleClosedMinutes} {
			if v != nil && (*v < 1 || *v > MaxIdleMinutes) {
				return refuse("Rückgabezeiten %s (%s): Leerlauf zwischen 1 und %d Minuten.", what, kindNames[kind], MaxIdleMinutes)
			}
		}
		if t.MaxHours != nil && (*t.MaxHours < 0 || *t.MaxHours > MaxMaxHours) {
			return refuse("Rückgabezeiten %s (%s): Obergrenze zwischen 1 und %d Stunden, 0 = keine.", what, kindNames[kind], MaxMaxHours)
		}
	}
	return nil
}

// checkReleaseMax refuses a budget's times that are out of range or without an upper bound where none is allowed: for CPU never, for GPU only with GPUs of the project's own.
func checkReleaseMax(p Project) error {
	if err := validRange(p.ReleaseMax, "des Budgets"); err != nil {
		return err
	}
	if m := p.ReleaseMax.CPU.MaxHours; m != nil && *m == 0 {
		return refuse("Rückgabezeiten (CPU): ohne Obergrenze geht es nur bei eigenen GPUs; CPUs teilen sich alle.")
	}
	if m := p.ReleaseMax.GPU.MaxHours; m != nil && *m == 0 && !ownGPUs(p) {
		return refuse("Rückgabezeiten (GPU): ohne Obergrenze geht es nur mit eigenen GPUs des Projekts (GPUs je Klasse); im gemeinsamen Pool gibt es immer eine.")
	}
	return nil
}

// checkRelease refuses the project's times when one is out of range or above what the budget allows (ceiling).
func (s *Service) checkRelease(p Project) error {
	if err := validRange(p.Release, "des Projekts"); err != nil {
		return err
	}
	for _, kind := range []string{"cpu", "gpu"} {
		t, c := p.Release.kind(kind), s.ceiling(p, kind)
		if v := t.IdleOpenMinutes; v != nil && *v > c.idleOpen {
			return refuse("Rückgabezeiten (%s): höchstens %d Minuten Leerlauf mit offenem Tab, mehr erlaubt das Budget nicht.", kindNames[kind], c.idleOpen)
		}
		if v := t.IdleClosedMinutes; v != nil && *v > c.idleClosed {
			return refuse("Rückgabezeiten (%s): höchstens %d Minuten Leerlauf ohne Tab, mehr erlaubt das Budget nicht.", kindNames[kind], c.idleClosed)
		}
		if v := t.MaxHours; v != nil && c.maxHours > 0 && (*v == 0 || *v > c.maxHours) {
			return refuse("Rückgabezeiten (%s): Obergrenze höchstens %d Stunden, mehr erlaubt das Budget nicht%s.", kindNames[kind], c.maxHours, unlimitedHint(kind))
		}
	}
	return nil
}

func unlimitedHint(kind string) string {
	if kind == "gpu" {
		return " (ohne Obergrenze nur mit eigenen GPUs, wenn das Budget es erlaubt)"
	}
	return ""
}

// applyRelease checks the release times of an update against the stored project: only what changed is checked, so a project whose budget shrank stays editable (its stored times are capped when they are used, resolve).
func (s *Service) applyRelease(p *Project, cur Project) error {
	if !reflect.DeepEqual(p.ReleaseMax, cur.ReleaseMax) {
		if err := checkReleaseMax(*p); err != nil {
			return err
		}
	}
	if !reflect.DeepEqual(p.Release, cur.Release) {
		return s.checkRelease(*p)
	}
	return nil
}

// releaseFromTree takes the budget's times the tree sends: out-of-range values and a missing upper bound where none is allowed are dropped (the default applies), so a tree sync never fails over them.
func releaseFromTree(r Release, p Project) Release {
	clean := func(t ReleaseTimes, kind string) ReleaseTimes {
		ok := func(v *int, lo, hi int) *int {
			if v == nil || *v < lo || *v > hi {
				return nil
			}
			n := *v
			return &n
		}
		out := ReleaseTimes{IdleOpenMinutes: ok(t.IdleOpenMinutes, 1, MaxIdleMinutes), IdleClosedMinutes: ok(t.IdleClosedMinutes, 1, MaxIdleMinutes), MaxHours: ok(t.MaxHours, 0, MaxMaxHours)}
		if out.MaxHours != nil && *out.MaxHours == 0 && (kind != "gpu" || !ownGPUs(p)) {
			out.MaxHours = nil
		}
		return out
	}
	return Release{CPU: clean(r.CPU, "cpu"), GPU: clean(r.GPU, "gpu")}
}
