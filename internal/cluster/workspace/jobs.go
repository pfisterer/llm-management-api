package workspace

import (
	"context"
	"errors"
	"math"
	"strings"
)

// Batch jobs (docs/architektur.md "Batch-Jobs"): GPU jobs without a workspace, in the project's volume and environment. What a project allows (JobsPerPerson, JobMaxHours, BatchPriority) is checked here; the GPU part builds the job and keeps it. In a tree project whose budgets set batch jobs (TreeProject.BatchJobs) the first two are the tree's (Project.TreeJobs), else the admins'.

// BatchGuaranteed is a project whose jobs are never evicted (BatchPriority).
const BatchGuaranteed = "guaranteed"

// JobProfile is the profile of a batch job's pod (label dhbw.cloud/profile), in usage and the running workspaces.
const JobProfile = "job"

// DefaultJobMaxHours is how long a job runs at the latest when the project sets nothing; MaxJobHours the most a project may set.
const (
	DefaultJobMaxHours = 4.0
	MaxJobHours        = 24.0
)

// batchPriorityFromTree takes the tree's value; anything but "guaranteed" is preemptible.
func batchPriorityFromTree(v string) string {
	if v == BatchGuaranteed {
		return BatchGuaranteed
	}
	return ""
}

// jobsFromTree are the job limits for what the tree allows: no negative count, and without jobs no run time; a run time within (0, MaxJobHours], 0 = DefaultJobMaxHours.
func jobsFromTree(j TreeJobs) (int, float64) {
	if j.JobsPerPerson <= 0 {
		return 0, 0
	}
	if j.MaxHours <= 0 || math.IsNaN(j.MaxHours) {
		return j.JobsPerPerson, 0
	}
	return j.JobsPerPerson, min(j.MaxHours, MaxJobHours)
}

// JobMaxHours is how long a job of the project may run at the latest.
func JobMaxHours(p Project) float64 {
	if p.Limits.JobMaxHours <= 0 {
		return DefaultJobMaxHours
	}
	return min(p.Limits.JobMaxHours, MaxJobHours)
}

// Reservation is what a running or waiting job still holds of the budget: its GPUs times the hours it may still run.
type Reservation struct {
	Person   string
	GPUClass string
	GPUHours float64
}

// Reservations lists the reservations of a project's jobs (the GPU part's live jobs).
type Reservations func(ctx context.Context, project string) ([]Reservation, error)

// SetJobReservations makes reservations count as used in the budgets (BudgetStatus): for a new job, for starts and for Enforce, so jobs never overbook a budget.
func (s *Service) SetJobReservations(r Reservations) { s.reservations = r }

// reserved is what the reservations hold of a budget: GPU hours only, of its class, of the person when it is one per person.
func reserved(rs []Reservation, person string, b Budget) float64 {
	if b.Unit != "gpu_hours" {
		return 0
	}
	var sum float64
	for _, r := range rs {
		if !b.coversClass(r.GPUClass) || (b.PerPerson && !strings.EqualFold(r.Person, person)) {
			continue
		}
		sum += r.GPUHours
	}
	return sum
}

// JobRequest is a job to check: who, in which project and GPU class, how many GPUs, how long at the latest (0 = the project's JobMaxHours), and how many jobs the person has running and waiting in the project.
type JobRequest struct {
	Person   string
	Project  string
	GPUClass string
	GPUs     int
	MaxHours float64
	Jobs     int
}

// JobGrant is a job the project allows, with what decides how it runs.
type JobGrant struct {
	Project    Project
	GPUClass   string
	MaxHours   float64
	Guaranteed bool
}

// CheckJob decides whether the person may submit the job: a member of the active project with workspaces and a built environment, batch jobs and the GPU class allowed, below the jobs per person, and the budgets: used plus reserved plus GPUs × hours of the new job must fit into every GPU-hours budget, and no other budget may be used up.
func (s *Service) CheckJob(ctx context.Context, tokens []string, r JobRequest) (JobGrant, error) {
	p, err := s.store.Get(ctx, r.Project)
	if errors.Is(err, ErrNotFound) {
		return JobGrant{}, refuse(noProjectAccess)
	}
	if err != nil {
		return JobGrant{}, err
	}
	switch {
	case p.RoleOf(tokens) == "":
		return JobGrant{}, refuse(noProjectAccess)
	case !p.Active(s.now()):
		return JobGrant{}, refuse("Projekt „%s“ ist am %s abgelaufen.", p.Name, p.EndsAt.Format("02.01.2006"))
	case p.NoWorkspaces:
		return JobGrant{}, refuse("Projekt „%s“ hat keine Workspaces, nur LLM-Tokens.", p.Name)
	case p.StorageGB <= 0:
		return JobGrant{}, refuse("Projekt „%s“ hat keinen Workspace-Speicher; Jobs brauchen ihn für Daten und Ausgabe.", p.Name)
	case p.ImagePending():
		return JobGrant{}, refuse("Die Umgebung von Projekt „%s“ ist noch nicht gebaut.", p.Name)
	case p.Limits.JobsPerPerson <= 0 || len(p.GPUClasses) == 0:
		return JobGrant{}, refuse("Im Projekt „%s“ sind keine Batch-Jobs freigegeben.", p.Name)
	case r.Jobs >= p.Limits.JobsPerPerson:
		return JobGrant{}, refuse("Im Projekt „%s“ %s gleichzeitig möglich (laufend und wartend); schon %d. Einen zuerst abwarten oder abbrechen.", p.Name, plural(p.Limits.JobsPerPerson, "ist 1 Job", "sind %d Jobs"), r.Jobs)
	}
	g := JobGrant{Project: p, GPUClass: r.GPUClass, MaxHours: JobMaxHours(p), Guaranteed: p.BatchPriority == BatchGuaranteed}
	if g.GPUClass == "" && len(p.GPUClasses) == 1 {
		g.GPUClass = p.GPUClasses[0]
	}
	if g.GPUClass == "" {
		return JobGrant{}, invalid("Bitte eine GPU-Klasse wählen.")
	}
	if !p.AllowsGPUClass(g.GPUClass) {
		return JobGrant{}, refuse("GPU-Klasse %s ist im Projekt „%s“ nicht freigegeben.", s.ClassName(ctx, g.GPUClass), p.Name)
	}
	if n := p.GPUsByClass[g.GPUClass]; n > 0 && r.GPUs > n {
		return JobGrant{}, refuse("Das Projekt „%s“ hat %s der Klasse %s.", p.Name, plural(n, "1 GPU", "%d GPUs"), s.ClassName(ctx, g.GPUClass))
	}
	if r.MaxHours < 0 || r.MaxHours > g.MaxHours {
		return JobGrant{}, refuse("Höchstlaufzeit im Projekt „%s“: %s Stunden.", p.Name, num(g.MaxHours))
	}
	if r.MaxHours > 0 {
		g.MaxHours = r.MaxHours
	}
	st, err := s.BudgetStatus(ctx, p, r.Person)
	if err != nil {
		return JobGrant{}, err
	}
	need := float64(max(r.GPUs, 1)) * g.MaxHours
	for _, b := range st {
		if b.Unit == "gpu_hours" && b.coversClass(g.GPUClass) && b.Amount > 0 && b.Used+b.Reserved+need > b.Amount+1e-9 {
			return JobGrant{}, refuse("Budget im Projekt „%s“ reicht nicht: %s. Genutzt %s, für laufende und wartende Jobs vorgemerkt %s, dieser Job bräuchte bis zu %s.", p.Name, describe(b.Budget), num(b.Used), num(b.Reserved), num(need))
		}
		if b.Exhausted {
			return JobGrant{}, refuse("Budget im Projekt „%s“ aufgebraucht: %s. Genutzt: %s.", p.Name, describe(b.Budget), num(b.Used+b.Reserved))
		}
	}
	return g, nil
}

// JobQueue is a project's batch queue in one of its GPU classes, for the fleet (Kueue ClusterQueue job-<slug>--<class>).
type JobQueue struct {
	Project    string
	GPUClass   string
	GPUs       int // the project's GPUs of the class, what its jobs use at the same time at most; 0 = no bound of its own
	Guaranteed bool
}

// JobQueues are the queues of the active projects with batch jobs, one per GPU class.
func (s *Service) JobQueues(ctx context.Context) ([]JobQueue, error) {
	list, err := s.store.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []JobQueue
	for _, p := range list {
		if p.Limits.JobsPerPerson <= 0 || p.NoWorkspaces || !p.Active(s.now()) {
			continue
		}
		for _, c := range p.GPUClasses {
			out = append(out, JobQueue{Project: p.Slug, GPUClass: c, GPUs: p.GPUsByClass[c], Guaranteed: p.BatchPriority == BatchGuaranteed})
		}
	}
	return out, nil
}

// checkJobLimits refuses job limits outside their range.
func checkJobLimits(l Limits) error {
	if l.JobsPerPerson < 0 || l.JobMaxHours < 0 || l.JobMaxHours > MaxJobHours || math.IsNaN(l.JobMaxHours) {
		return invalid("Batch-Jobs: Jobs je Person nicht negativ, Höchstlaufzeit zwischen 0 (Standard %s Stunden) und %s Stunden.", num(DefaultJobMaxHours), num(MaxJobHours))
	}
	return nil
}
