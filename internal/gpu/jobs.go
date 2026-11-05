package gpu

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Batch jobs (docs/architektur.md "Batch-Jobs"): GPU jobs without a workspace. A job is built like a GPU kernel pod (kernels.go) without its server: the project's environment, the person's home and the shared folder from the project's volume, in the project's queue job-<slug>--<class>, which borrows the class's GPUs up to the project's (fleet.go). Kueue admits it, evicts a preemptible one for a workspace and admits it again (it starts from the beginning); Kubernetes deletes the finished Job after a day. What stays is the row in gpu_jobs: state, exit code, times, evictions, GPU hours and the end of its output.

// JobConfig is how batch jobs are built.
type JobConfig struct {
	// InitImage creates the person's home in the project's volume before the job (the hub's init image, a base pinned by digest). Empty = no batch jobs.
	InitImage string
	// StorageClass and StorageAccess are how the project's volume ws-<slug> is made when a job comes before any workspace with storage, as the hub makes it (juicefs-projects and ReadWriteMany; local-path and ReadWriteOnce without JuiceFS).
	StorageClass  string
	StorageAccess string
	// GraceSeconds is how long a job has after TERM (eviction, its deadline, cancel) to save a checkpoint before Kubernetes kills it: the pod's terminationGracePeriodSeconds. 0 = DefaultJobGraceSeconds; at most MaxJobGraceSeconds. The GPU stays the pod's until it has ended, so this is also how long a workspace that evicts the job may wait for it.
	GraceSeconds int
}

const (
	jobApp         = "dhbw-job"
	jobLabelUser   = "dhbw.cloud/job-user"
	jobAnnotPrefix = "dhbw.cloud/job-"
	// JobTTL is how long Kubernetes keeps a finished Job and its pod (ttlSecondsAfterFinished): the output can be read live until then.
	JobTTL = 24 * time.Hour
	// JobLogTail is how much of a job's output the row keeps, and JobLogKeep for how long.
	JobLogTail = 64 << 10
	JobLogKeep = 30 * 24 * time.Hour
	// WorkloadPriorityClasses of the jobs (tasks/kueue.yaml): preemptible above the gap fillers and below the workspaces, guaranteed as a workspace.
	JobPriority           = "batch"
	JobPriorityGuaranteed = "batch-guaranteed"
)

// States of a job.
const (
	JobQueued    = "queued"
	JobRunning   = "running"
	JobSucceeded = "succeeded"
	JobFailed    = "failed"
	JobCancelled = "cancelled"
)

// JobQueue is a project's batch queue in one GPU class.
type JobQueue struct {
	Project    string
	Class      string
	GPUs       int  // the project's GPUs of the class: what its jobs use at the same time at most; 0 = no bound of its own
	Guaranteed bool // batch-guaranteed: also evicts preemptible jobs of other projects, is not evicted by workspaces
}

// JobQueueName is the ClusterQueue and LocalQueue of a project's jobs in a class. Slugs and classes have no "--", so the name is unambiguous.
func JobQueueName(project, class string) string { return "job-" + project + "--" + class }

// SetJobQueues sets where the fleet learns the projects' batch queues (workspace.Service.JobQueues).
func (s *Service) SetJobQueues(f func(ctx context.Context) ([]JobQueue, error)) { s.jobQueues = f }

// jobClusterQueueFor is a project's job queue: no quota of its own, it borrows the class's GPUs up to the project's (borrowingLimit) and evicts what is below its threshold in the class's other queues — preemptible jobs only gap fillers (−100), guaranteed ones also preemptible jobs (−50).
func jobClusterQueueFor(q JobQueue) map[string]any {
	gpu := map[string]any{"name": "nvidia.com/gpu", "nominalQuota": "0"}
	if q.GPUs > 0 {
		gpu["borrowingLimit"] = fmt.Sprint(q.GPUs)
	}
	threshold := -51
	if q.Guaranteed {
		threshold = -1
	}
	return map[string]any{
		"apiVersion": "kueue.x-k8s.io/v1beta2", "kind": "ClusterQueue",
		"metadata": map[string]any{"name": JobQueueName(q.Project, q.Class), "labels": map[string]any{
			"app.kubernetes.io/managed-by": "llm-management-api", "dhbw.cloud/queue-kind": "job", "dhbw.cloud/project": q.Project, labelClass: q.Class}},
		"spec": map[string]any{
			"namespaceSelector": map[string]any{},
			"cohortName":        "class-" + q.Class,
			"preemption": map[string]any{"withinClusterQueue": "LowerPriority", "reclaimWithinCohort": "LowerPriority",
				"borrowWithinCohort": map[string]any{"policy": "LowerPriority", "maxPriorityThreshold": threshold}},
			"resourceGroups": []any{map[string]any{
				"coveredResources": []any{"cpu", "memory", "nvidia.com/gpu"},
				"flavors": []any{map[string]any{"name": q.Class, "resources": []any{
					map[string]any{"name": "cpu", "nominalQuota": "0"}, map[string]any{"name": "memory", "nominalQuota": "0"}, gpu,
				}}},
			}},
		},
	}
}

func jobLocalQueueFor(ns string, q JobQueue) map[string]any {
	name := JobQueueName(q.Project, q.Class)
	return map[string]any{
		"apiVersion": "kueue.x-k8s.io/v1beta2", "kind": "LocalQueue",
		"metadata": map[string]any{"name": name, "namespace": ns, "labels": map[string]any{"app.kubernetes.io/managed-by": "llm-management-api", "dhbw.cloud/queue-kind": "job"}},
		"spec":     map[string]any{"clusterQueue": name},
	}
}

// reconcileJobQueues keeps a queue per project with batch jobs and class (of the classes there are) and deletes the job queues nobody wants any more; Kueue keeps a queue with admitted workloads until they end.
func (s *Service) reconcileJobQueues(ctx context.Context, k kubeDoer, classes []GPUClass) error {
	if s.jobQueues == nil || s.cfg.Kernels.NoGPU {
		return nil
	}
	want, err := s.jobQueues(ctx)
	if err != nil {
		return fmt.Errorf("job queues: %w", err)
	}
	ns := s.kernelNamespace()
	keep := map[string]bool{}
	var errs []error
	for _, q := range want {
		if !slices.ContainsFunc(classes, func(c GPUClass) bool { return c.ID == q.Class }) {
			continue
		}
		keep[JobQueueName(q.Project, q.Class)] = true
		for _, o := range []map[string]any{jobClusterQueueFor(q), jobLocalQueueFor(ns, q)} {
			if err := ensureObject(ctx, k, o); err != nil {
				errs = append(errs, fmt.Errorf("job queue %s: %w", JobQueueName(q.Project, q.Class), err))
			}
		}
	}
	var cqs struct {
		Items []map[string]any `json:"items"`
	}
	if err := k.do(ctx, http.MethodGet, "/apis/kueue.x-k8s.io/v1beta2/clusterqueues?labelSelector="+url.QueryEscape("dhbw.cloud/queue-kind=job"), nil, &cqs); err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, cq := range cqs.Items {
		meta := obj(cq, "metadata")
		name := str(meta, "name")
		if keep[name] || str(obj(meta, "labels"), "dhbw.cloud/queue-kind") != "job" {
			continue
		}
		for _, p := range []string{"/apis/kueue.x-k8s.io/v1beta2/namespaces/" + ns + "/localqueues/" + name, "/apis/kueue.x-k8s.io/v1beta2/clusterqueues/" + name} {
			if err := k.do(ctx, http.MethodDelete, p, nil, nil); err != nil && !errors.Is(err, ErrNotFound) {
				errs = append(errs, err)
			}
		}
		s.log.Infow("job queue removed", "queue", name)
	}
	return errors.Join(errs...)
}

// JobSpec is a job the project allows (workspace.CheckJob), with what the service needs of the project to build it.
type JobSpec struct {
	Name       string // the person's name for it, also its output folder ~/jobs/<name>; empty = its id
	Person     string // JupyterHub user name (e-mail)
	Project    string
	Class      string
	GPUs       int
	MaxHours   float64
	Guaranteed bool
	// The project's environment, as the hub starts it (HubProject): image, architectures, pull secret; the project's volume ws-<slug> and whether the person only reads its shared folder.
	Image          string
	Archs          []string
	PullSecret     string
	SharedReadOnly bool
	ScratchGB      int
	StorageGB      int // the project's volume ws-<slug>, made or grown to this size before the job
	// LLMKey is the person's LLM key for the job (keys.WorkspaceKey, as their workspaces in the project get it), drawn from LLMProject; it goes into a Secret <id>-llm owned by the Job, never into the Job itself or the job's row. LLMBaseURL is the LLM API as people reach it. Empty = no key.
	LLMKey     string
	LLMProject string
	LLMBaseURL string
	// LLMAgentEnv are Claude Code's ANTHROPIC_* and CLAUDE_CODE_* variables for the key, as a workspace gets them, without the key itself (ANTHROPIC_AUTH_TOKEN comes from the Secret too); nil = none.
	LLMAgentEnv map[string]string
	// What runs: Command in a shell, or Notebook (a path in the home) through papermill; WorkingDir is relative to the home (empty = the home, for a notebook its folder).
	Command    string
	Notebook   string
	WorkingDir string
	// requests and limits of the container (jobResources), set by SubmitJob.
	requests, limits map[string]any
}

// Job is a job as people see it: its row in gpu_jobs with what Kueue says while it lives.
type Job struct {
	ID         string  `json:"id" example:"j-4f3a9c0d2e"`
	Name       string  `json:"name" example:"training-1"`
	Person     string  `json:"person"`
	Project    string  `json:"project"`
	GPUClass   string  `json:"gpu_class"`
	GPUs       int     `json:"gpus"`
	MaxHours   float64 `json:"max_hours"`
	Guaranteed bool    `json:"guaranteed"`
	Command    string  `json:"command,omitempty"`
	Notebook   string  `json:"notebook,omitempty"`
	WorkingDir string  `json:"working_dir,omitempty"`
	// LLMProject is the project whose LLM tokens the job's key (OPENAI_API_KEY) draws from; empty = the job has none.
	LLMProject string     `json:"llm_project,omitempty"`
	Image      string     `json:"image"`
	State      string     `json:"state" enums:"queued,running,succeeded,failed,cancelled"`
	ExitCode   *int       `json:"exit_code,omitempty"`
	Reason     string     `json:"reason,omitempty" example:"DeadlineExceeded"`
	Submitted  time.Time  `json:"submitted"`
	Admitted   *time.Time `json:"admitted,omitempty"`
	Started    *time.Time `json:"started,omitempty"`
	Ended      *time.Time `json:"ended,omitempty"`
	Evictions  int        `json:"evictions"`
	GPUHours   float64    `json:"gpu_hours"`
	// Position is the job's place in its queue while it waits (1 = next); 0 otherwise.
	Position int `json:"position,omitempty"`
}

// JobRecord is a job's row in gpu_jobs: kept as long as its project, the end of its output (LogTail) JobLogKeep after it ended.
type JobRecord struct {
	ID         string `gorm:"primaryKey"`
	Name       string
	Person     string `gorm:"index"`
	Project    string `gorm:"index"`
	GPUClass   string
	GPUs       int
	MaxHours   float64
	Guaranteed bool
	Command    string
	Notebook   string
	WorkingDir string
	LLMProject string
	Image      string
	State      string `gorm:"index"`
	ExitCode   *int
	Reason     string
	Submitted  time.Time
	Admitted   *time.Time
	Started    *time.Time
	Ended      *time.Time
	Evictions  int
	GPUHours   float64
	// SeenRunning is when the sync last saw it running: the GPU hours count from there. AttemptStarted is the start of its current attempt, from which its deadline counts.
	SeenRunning    *time.Time
	AttemptStarted *time.Time
	LogTail        string
}

func (JobRecord) TableName() string { return "gpu_jobs" }

func (r JobRecord) job() Job {
	return Job{ID: r.ID, Name: r.Name, Person: r.Person, Project: r.Project, GPUClass: r.GPUClass, GPUs: r.GPUs, MaxHours: r.MaxHours, Guaranteed: r.Guaranteed,
		Command: r.Command, Notebook: r.Notebook, WorkingDir: r.WorkingDir, LLMProject: r.LLMProject, Image: r.Image, State: r.State, ExitCode: r.ExitCode, Reason: r.Reason,
		Submitted: r.Submitted, Admitted: r.Admitted, Started: r.Started, Ended: r.Ended, Evictions: r.Evictions, GPUHours: r.GPUHours}
}

func (r JobRecord) live() bool { return r.State == JobQueued || r.State == JobRunning }

// JobFilter selects jobs: of a project, of a person, only the live ones (queued, running); empty fields select all.
type JobFilter struct {
	Project string
	Person  string
	Live    bool
}

// JobStore keeps the rows of gpu_jobs.
type JobStore interface {
	Jobs(ctx context.Context, f JobFilter) ([]JobRecord, error)
	Job(ctx context.Context, id string) (JobRecord, error)
	SaveJob(ctx context.Context, r JobRecord) error
}

// MemoryJobStore is a JobStore in memory (tests, development without a database).
type MemoryJobStore struct {
	mu   sync.Mutex
	rows map[string]JobRecord
}

func NewMemoryJobStore() *MemoryJobStore { return &MemoryJobStore{rows: map[string]JobRecord{}} }

func (m *MemoryJobStore) Jobs(_ context.Context, f JobFilter) ([]JobRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []JobRecord
	for _, r := range m.rows {
		if f.match(r) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Submitted.After(out[j].Submitted) })
	return out, nil
}

func (m *MemoryJobStore) Job(_ context.Context, id string) (JobRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[id]
	if !ok {
		return JobRecord{}, ErrNotFound
	}
	return r, nil
}

func (m *MemoryJobStore) SaveJob(_ context.Context, r JobRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[r.ID] = r
	return nil
}

func (f JobFilter) match(r JobRecord) bool {
	return (f.Project == "" || r.Project == f.Project) && (f.Person == "" || strings.EqualFold(r.Person, f.Person)) && (!f.Live || r.live())
}

// GormJobStore is a JobStore in the database.
type GormJobStore struct{ db *gorm.DB }

func NewGormJobStore(db *gorm.DB) (*GormJobStore, error) {
	if err := db.AutoMigrate(&JobRecord{}); err != nil {
		return nil, err
	}
	return &GormJobStore{db: db}, nil
}

func (g *GormJobStore) Jobs(ctx context.Context, f JobFilter) ([]JobRecord, error) {
	q := g.db.WithContext(ctx).Order("submitted desc")
	if f.Project != "" {
		q = q.Where("project = ?", f.Project)
	}
	if f.Person != "" {
		q = q.Where("lower(person) = lower(?)", f.Person)
	}
	if f.Live {
		q = q.Where("state IN ?", []string{JobQueued, JobRunning})
	}
	var out []JobRecord
	return out, q.Find(&out).Error
}

func (g *GormJobStore) Job(ctx context.Context, id string) (JobRecord, error) {
	var r JobRecord
	err := g.db.WithContext(ctx).First(&r, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return r, ErrNotFound
	}
	return r, err
}

func (g *GormJobStore) SaveJob(ctx context.Context, r JobRecord) error {
	return g.db.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).Create(&r).Error
}

// jobProjectLocker is a JobStore that serialises check and submit of a project's jobs across the replicas of the service (GormJobStore: a Postgres advisory lock).
type jobProjectLocker interface {
	lockProject(ctx context.Context, project string, fn func(context.Context) error) error
}

// jobLockClass is the first key of the advisory locks of jobs ("jobs"), the second the hash of the project's slug.
const jobLockClass = 0x6a6f6273

// lockProject runs fn in a transaction that holds the project's advisory lock until it ends, after what fn saved through the store (on other connections) is committed: the next one sees it.
func (g *GormJobStore) lockProject(ctx context.Context, project string, fn func(context.Context) error) error {
	return g.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT pg_advisory_xact_lock(?::integer, hashtext(?::text))", jobLockClass, project).Error; err != nil {
			return fmt.Errorf("job lock: %w", err)
		}
		return fn(ctx)
	})
}

// LockProjectJobs runs fn alone among the submits to the project: what fn counts (the person's live jobs, the budget with the reservations of live jobs) holds until the job it submits is saved. In the process a lock per project, across replicas (a rolling update runs two) the store's advisory lock; the process lock first, so a burst of requests waits in memory and holds no database connection.
func (s *Service) LockProjectJobs(ctx context.Context, project string, fn func(context.Context) error) error {
	l, _ := s.jobLocks.LoadOrStore(project, make(chan struct{}, 1))
	lock := l.(chan struct{})
	select {
	case lock <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-lock }()
	if pl, ok := s.jobs.(jobProjectLocker); ok {
		return pl.lockProject(ctx, project, fn)
	}
	return fn(ctx)
}

// SetJobStore turns batch jobs on (with JobConfig.InitImage).
func (s *Service) SetJobStore(st JobStore) { s.jobs = st }

// JobsEnabled says whether the service runs batch jobs.
func (s *Service) JobsEnabled() bool {
	return s.jobs != nil && s.k8s != nil && s.cfg.Jobs.InitImage != ""
}

// JobRefused is a job the request does not allow; Reason is shown to the person.
type JobRefused struct{ Reason string }

func (e *JobRefused) Error() string { return e.Reason }

var (
	jobNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]{0,38}[a-z0-9])?$`)
	jobIDPattern   = regexp.MustCompile(`^j-[0-9a-f]{10}$`)
)

// JobID says whether id has the form of a job id.
func JobID(id string) bool { return jobIDPattern.MatchString(id) }

// homeRel is a path in the home, relative to it: an absolute one must lie in /home/jovyan, a relative one counts from there; "" is the home itself, false a path that leaves it.
func homeRel(p string) (string, bool) {
	const home = "/home/jovyan"
	p = strings.TrimSpace(p)
	if !strings.HasPrefix(p, "/") {
		p = home + "/" + p
	}
	c := path.Clean(p)
	if c == home {
		return "", true
	}
	if !strings.HasPrefix(c, home+"/") {
		return "", false
	}
	return strings.TrimPrefix(c, home+"/"), true
}

// SubmitJob builds the job, records it and creates it; the project's queue is made first, so a project does not wait for the next reconcile of the fleet.
func (s *Service) SubmitJob(ctx context.Context, sp JobSpec) (Job, error) {
	if !s.JobsEnabled() {
		return Job{}, &JobRefused{"Batch-Jobs sind in diesem Cluster nicht eingerichtet."}
	}
	if sp.Name != "" && !jobNamePattern.MatchString(sp.Name) {
		return Job{}, &JobRefused{"Name: bis 40 Zeichen, Kleinbuchstaben, Ziffern, Punkt, Bindestrich und Unterstrich, am Anfang und Ende Buchstabe oder Ziffer."}
	}
	if (strings.TrimSpace(sp.Command) == "") == (strings.TrimSpace(sp.Notebook) == "") {
		return Job{}, &JobRefused{"Entweder einen Befehl oder ein Notebook angeben."}
	}
	if len(sp.Command) > 8192 {
		return Job{}, &JobRefused{"Der Befehl ist länger als 8192 Zeichen; längeres in ein Skript im Home schreiben."}
	}
	wd, ok := homeRel(sp.WorkingDir)
	if !ok {
		return Job{}, &JobRefused{"Arbeitsverzeichnis außerhalb des Homes."}
	}
	sp.WorkingDir = wd
	if sp.Notebook != "" {
		nb, ok := homeRel(sp.Notebook)
		if !ok || nb == "" || !strings.HasSuffix(nb, ".ipynb") {
			return Job{}, &JobRefused{"Notebook: ein Pfad im Home, der auf .ipynb endet."}
		}
		sp.Notebook = nb
	}
	if sp.GPUs != 1 {
		return Job{}, &JobRefused{"Ein Job hat genau eine GPU."}
	}
	if err := s.ensureProjectVolume(ctx, sp.Project, sp.StorageGB); err != nil {
		return Job{}, fmt.Errorf("project volume: %w", err)
	}
	id := make([]byte, 5)
	_, _ = rand.Read(id)
	r := JobRecord{ID: "j-" + hex.EncodeToString(id), Name: sp.Name, Person: sp.Person, Project: sp.Project, GPUClass: sp.Class, GPUs: sp.GPUs, MaxHours: sp.MaxHours,
		Guaranteed: sp.Guaranteed, Command: sp.Command, Notebook: sp.Notebook, WorkingDir: sp.WorkingDir, Image: sp.Image, State: JobQueued, Submitted: time.Now().UTC()}
	if sp.LLMKey != "" {
		r.LLMProject = sp.LLMProject
	}
	if r.Name == "" {
		r.Name = r.ID
	}
	q := JobQueue{Project: sp.Project, Class: sp.Class, Guaranteed: sp.Guaranteed}
	if !s.cfg.Kernels.NoGPU {
		// The queue as the fleet keeps it (the project's GPUs of the class); a project new since the last reconcile gets it now.
		if s.jobQueues != nil {
			if qs, err := s.jobQueues(ctx); err == nil {
				for _, x := range qs {
					if x.Project == q.Project && x.Class == q.Class {
						q = x
					}
				}
			}
		}
		for _, o := range []map[string]any{jobClusterQueueFor(q), jobLocalQueueFor(s.kernelNamespace(), q)} {
			if err := ensureObject(ctx, s.k8s, o); err != nil {
				return Job{}, fmt.Errorf("job queue: %w", err)
			}
		}
	}
	sp.requests, sp.limits = s.jobResources(ctx, sp.Class)
	if err := s.jobs.SaveJob(ctx, r); err != nil {
		return Job{}, err
	}
	var created struct {
		Metadata struct {
			UID string `json:"uid"`
		} `json:"metadata"`
	}
	failed := func(err error) (Job, error) {
		end := time.Now().UTC()
		r.State, r.Reason, r.Ended = JobFailed, "CreateFailed", &end
		_ = s.jobs.SaveJob(ctx, r)
		return Job{}, err
	}
	if err := s.k8s.do(ctx, http.MethodPost, "/apis/batch/v1/namespaces/"+s.kernelNamespace()+"/jobs", s.jobObject(r, sp), &created); err != nil {
		return failed(err)
	}
	// The key after the Job, which owns it (deleted with it); the Job is suspended until Kueue admits it, and a pod that came first would wait for the Secret.
	if sp.LLMKey != "" {
		if err := s.k8s.do(ctx, http.MethodPost, "/api/v1/namespaces/"+s.kernelNamespace()+"/secrets", jobLLMSecret(r, created.Metadata.UID, sp.LLMKey), nil); err != nil {
			_ = s.k8s.do(ctx, http.MethodDelete, "/apis/batch/v1/namespaces/"+s.kernelNamespace()+"/jobs/"+r.ID+"?propagationPolicy=Background", nil, nil)
			return failed(fmt.Errorf("llm key secret: %w", err))
		}
	}
	s.log.Infow("batch job submitted", "job", r.ID, "person", r.Person, "project", r.Project, "class", r.GPUClass, "max_hours", r.MaxHours, "guaranteed", r.Guaranteed, "llm_project", r.LLMProject)
	return r.job(), nil
}

// ensureProjectVolume makes the project's volume ws-<slug> or grows it to gb, as the hub does before a start (10-dhbw-storage): a job may come before any workspace of the project, and a volume never shrinks.
func (s *Service) ensureProjectVolume(ctx context.Context, project string, gb int) error {
	if gb <= 0 {
		return &JobRefused{"Das Projekt hat keinen Workspace-Speicher; Jobs brauchen ihn für Daten und Ausgabe."}
	}
	path := "/api/v1/namespaces/" + s.kernelNamespace() + "/persistentvolumeclaims"
	size := fmt.Sprintf("%dGi", gb)
	var pvc map[string]any
	err := s.k8s.do(ctx, http.MethodGet, path+"/ws-"+project, nil, &pvc)
	if errors.Is(err, ErrNotFound) {
		class, access := s.cfg.Jobs.StorageClass, s.cfg.Jobs.StorageAccess
		if class == "" {
			class = "juicefs-projects"
		}
		if access == "" {
			access = "ReadWriteMany"
		}
		s.log.Infow("project volume created for a job", "project", project, "size", size)
		err = s.k8s.do(ctx, http.MethodPost, path, map[string]any{
			"apiVersion": "v1", "kind": "PersistentVolumeClaim",
			"metadata": map[string]any{"name": "ws-" + project, "labels": map[string]any{"dhbw.cloud/workspace-storage": "true", "dhbw.cloud/project": project}},
			"spec":     map[string]any{"accessModes": []any{access}, "storageClassName": class, "resources": map[string]any{"requests": map[string]any{"storage": size}}},
		}, nil)
		if errors.Is(err, errAlreadyExists) {
			return nil
		}
		return err
	}
	if err != nil {
		return err
	}
	if parseGiB(str(obj(obj(obj(pvc, "spec"), "resources"), "requests"), "storage")) < float64(gb) {
		s.log.Infow("project volume grown for a job", "project", project, "size", size)
		return s.k8s.do(ctx, http.MethodPatch, path+"/ws-"+project, map[string]any{"spec": map[string]any{"resources": map[string]any{"requests": map[string]any{"storage": size}}}}, nil)
	}
	return nil
}

// jobScript runs the job's command in the working directory, its output also into ~/jobs/<name>/output-<n>.log (one file per start, numbered in the folder), and ends with the command's exit code. bash where the image has it, else sh; only POSIX sh is assumed, no tini (the projects' own images need not have it).
//
// Signals: the shell is PID 1, which the kernel spares every signal it has no handler for, and a pipeline would hide the command from it. So the command runs in the background in its own process group (setsid where the image has it) with its output through a FIFO into tee; the trap passes TERM and INT on to the whole group (a script's children too), the shell waits until the command has really ended (a trapped signal ends the first wait early) and tee, which ignores the signals, writes the last lines. Kubernetes sends TERM when Kueue evicts the job, at its deadline and when it is cancelled, and kills after terminationGracePeriodSeconds (jobGraceSeconds): the time the command has to save a checkpoint. A command that ends with 0 after the signal leaves with 143: the pod of an evicted job must not count as the job's success.
//
// DHBW_JOB_ATTEMPT is how often this job (its id, not its name, which another job may reuse) has started, counted in ~/jobs/<name>/.attempt-<id> on the project's volume, which outlives the pods: 1 at the first start, one more after each eviction. The service cannot hand it in: the pod's environment is fixed when the Job is created, the pod has no token to read its Workload, and Kueue's eviction count also counts evictions before the pod ever ran.
const jobScript = `p= stop=
fwd() { stop=1; [ -n "$p" ] && { kill -TERM -"$p" 2>/dev/null || kill -TERM "$p" 2>/dev/null; }; }
trap fwd TERM INT
d="$HOME/jobs/$DHBW_JOB_NAME"
mkdir -p "$d" || exit 1
n=1; while [ -e "$d/output-$n.log" ]; do n=$((n+1)); done
log="$d/output-$n.log"
a=$(cat "$d/.attempt-$DHBW_JOB_ID" 2>/dev/null); case "$a" in ''|*[!0-9]*) a=0;; esac
a=$((a+1)); echo "$a" > "$d/.attempt-$DHBW_JOB_ID" || exit 1
export DHBW_JOB_ATTEMPT="$a"
cd "$HOME/$DHBW_JOB_WORKDIR" || exit 1
sh=sh; command -v bash >/dev/null 2>&1 && sh=bash
s=; command -v setsid >/dev/null 2>&1 && s=setsid
f="${TMPDIR:-/tmp}/dhbw-job-$$.out"; rm -f "$f"; mkfifo "$f" || exit 1
echo "== $(date -Iseconds) Job $DHBW_JOB_NAME ($DHBW_JOB_ID), Versuch $a" | tee "$log"
(trap '' TERM INT; exec tee -a "$log") < "$f" &
t=$!
$s "$sh" -c "$DHBW_JOB_COMMAND" > "$f" 2>&1 &
p=$!
[ -n "$stop" ] && fwd
wait "$p"; rc=$?
while [ -n "$stop" ] && kill -0 "$p" 2>/dev/null; do wait "$p"; rc=$?; done
while kill -0 "$t" 2>/dev/null; do wait "$t"; done
rm -f "$f"
if [ -n "$stop" ]; then
  echo "== $(date -Iseconds) Signal erhalten (Verdrängung, Abbruch oder Höchstlaufzeit), Exit-Code des Befehls $rc" | tee -a "$log"
  [ "$rc" = 0 ] && rc=143
fi
echo "== $(date -Iseconds) Ende, Exit-Code $rc" | tee -a "$log"
exit "$rc"`

// DefaultJobGraceSeconds is the pod's terminationGracePeriodSeconds without JobConfig.GraceSeconds: how long a job has after TERM to save its state before Kubernetes kills it. MaxJobGraceSeconds caps the setting: a workspace that evicts a job waits that long for the GPU.
const (
	DefaultJobGraceSeconds = 120
	MaxJobGraceSeconds     = 600
)

// jobGraceSeconds is the grace period of a job's pod (JobConfig.GraceSeconds, DefaultJobGraceSeconds, at most MaxJobGraceSeconds).
func (s *Service) jobGraceSeconds() int {
	g := s.cfg.Jobs.GraceSeconds
	if g <= 0 {
		return DefaultJobGraceSeconds
	}
	return min(g, MaxJobGraceSeconds)
}

// jobLLMSecretName is the Secret with a job's LLM key.
func jobLLMSecretName(id string) string { return id + "-llm" }

// jobLLMSecret holds a job's LLM key (OPENAI_API_KEY), owned by its Job (uid; empty only in tests) so that Kubernetes deletes it with the Job.
func jobLLMSecret(r JobRecord, jobUID, key string) map[string]any {
	meta := map[string]any{"name": jobLLMSecretName(r.ID), "labels": map[string]any{"app": jobApp, "dhbw.cloud/project": r.Project, "dhbw.cloud/job-id": r.ID}}
	if jobUID != "" {
		meta["ownerReferences"] = []any{map[string]any{"apiVersion": "batch/v1", "kind": "Job", "name": r.ID, "uid": jobUID}}
	}
	return map[string]any{"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "metadata": meta, "stringData": map[string]any{"OPENAI_API_KEY": key}}
}

// shellQuote quotes s for a POSIX shell.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// notebookCommand runs a notebook with papermill; the executed notebook lands next to it as <stem>.<job name>.ipynb.
func notebookCommand(nb, name string) string {
	out := strings.TrimSuffix(nb, ".ipynb") + "." + name + ".ipynb"
	return "papermill --log-output " + shellQuote("$HOME/"+nb) + " " + shellQuote("$HOME/"+out)
}

// Resources of a job's container when its class has no ready nodes to measure (jobResources): cores (request, limit) and memory (request = limit, guaranteed).
const (
	jobCPURequest = 2.0
	jobCPULimit   = 4.0
	jobMemGi      = 8.0
)

// jobResources are a job's container resources in its class: its whole share per GPU, what the class's ready nodes have after the fleet's reserve divided by their GPUs (the class's quota, which Kueue checks), the smallest share of its nodes. The share leaves every other GPU of a node its own, so no cap is needed on top. Without fleet or nodes the fallbacks above.
func (s *Service) jobResources(ctx context.Context, class string) (requests, limits map[string]any) {
	cpuReq, cpuLim, mem := jobCPURequest, jobCPULimit, jobMemGi
	if s.fleet != nil {
		if nodes, err := s.GPUNodes(ctx); err == nil {
			found := false
			for _, n := range nodes {
				if n.Class != class || !n.Ready || n.GPUs <= 0 {
					continue
				}
				cpu := max(n.CPU-s.fleetCfg.ReserveCPU, 0) / n.GPUs
				m := max(n.MemGi-s.fleetCfg.ReserveMemoryGi, 0) / n.GPUs
				if !found {
					cpuReq, mem, found = cpu, m, true
				}
				cpuReq, mem = min(cpuReq, cpu), min(mem, m)
			}
			if found {
				cpuLim = cpuReq
			}
		}
	}
	// Whole Mi and milli-cores, a little below the share so that rounding never exceeds the quota.
	memMi := int(mem*1024) - 64
	return map[string]any{"cpu": fmt.Sprintf("%dm", int(cpuReq*1000)), "memory": fmt.Sprintf("%dMi", memMi)},
		map[string]any{"cpu": fmt.Sprintf("%dm", int(cpuLim*1000)), "memory": fmt.Sprintf("%dMi", memMi)}
}

// jobObject is the batch/v1 Job: suspended until Kueue admits it, no retries (an eviction is no failure; it is admitted again), the project's maximum as its deadline (which Kubernetes restarts after an eviction; limitJobHours cuts it to what is left over all attempts), deleted a day after its end.
func (s *Service) jobObject(r JobRecord, sp JobSpec) map[string]any {
	queue, priority := JobQueueName(r.Project, r.GPUClass), JobPriority
	if r.Guaranteed {
		priority = JobPriorityGuaranteed
	}
	if s.cfg.Kernels.NoGPU {
		queue = s.cfg.Kernels.Queue
	}
	command := r.Command
	workdir := r.WorkingDir
	if r.Notebook != "" {
		command = notebookCommand(r.Notebook, r.Name)
		if workdir == "" {
			workdir = path.Dir(r.Notebook)
			if workdir == "." {
				workdir = ""
			}
		}
		// papermill expands no variables: the paths are given absolute.
		command = strings.ReplaceAll(command, "'$HOME/", "'/home/jovyan/")
	}
	user := HubUserSlug(r.Person)
	labels := map[string]any{"app": jobApp, "component": "job", "dhbw.cloud/profile": "job", "dhbw.cloud/project": r.Project, labelClass: r.GPUClass,
		jobLabelUser: hubLabel(r.Person), "dhbw.cloud/job-id": r.ID}
	requests, limits := sp.requests, sp.limits
	kernelScratchResources(sp.ScratchGB, requests, limits)
	spec := map[string]any{
		"restartPolicy":                 "Never",
		"terminationGracePeriodSeconds": s.jobGraceSeconds(),
		"automountServiceAccountToken":  false,
		"enableServiceLinks":            false,
		"securityContext":               map[string]any{"runAsUser": 1000, "runAsGroup": 100, "fsGroup": 100},
		"volumes":                       []any{map[string]any{"name": "home", "persistentVolumeClaim": map[string]any{"claimName": "ws-" + r.Project}}},
	}
	if !s.cfg.Kernels.NoGPU {
		requests["nvidia.com/gpu"], limits["nvidia.com/gpu"] = fmt.Sprint(r.GPUs), fmt.Sprint(r.GPUs)
		spec["tolerations"] = []any{gpuToleration}
	}
	if len(sp.Archs) == 1 {
		spec["nodeSelector"] = map[string]any{"kubernetes.io/arch": sp.Archs[0]}
	}
	if sp.PullSecret != "" {
		spec["imagePullSecrets"] = []any{map[string]any{"name": sp.PullSecret}}
	}
	sec := map[string]any{"allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []any{"ALL"}}}
	home := "homes/" + user
	// As the hub's init container dhbw-dirs: the home and the shared folder, created as the person in the init image, which alone sees the whole volume.
	spec["initContainers"] = []any{map[string]any{
		"name": "dhbw-dirs", "image": s.cfg.Jobs.InitImage,
		"command":         []any{"sh", "-c", `mkdir -p "/ws/$DHBW_HOME_DIR" /ws/project-shared`},
		"env":             []any{envVar("DHBW_HOME_DIR", home)},
		"securityContext": map[string]any{"runAsUser": 1000, "runAsGroup": 100, "allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []any{"ALL"}}},
		"resources":       map[string]any{"requests": map[string]any{"cpu": "10m", "memory": "32Mi"}, "limits": map[string]any{"memory": "128Mi"}},
		"volumeMounts":    []any{map[string]any{"name": "home", "mountPath": "/ws"}},
	}}
	shared := map[string]any{"name": "home", "mountPath": "/home/jovyan/project-shared", "subPath": "project-shared"}
	if sp.SharedReadOnly {
		shared["readOnly"] = true
	}
	spec["containers"] = []any{map[string]any{
		"name":    "job",
		"image":   r.Image,
		"command": []any{"sh", "-c", jobScript},
		"env": []any{envVar("HOME", "/home/jovyan"), envVar("DHBW_JOB_ID", r.ID), envVar("DHBW_JOB_NAME", r.Name), envVar("DHBW_JOB_DIR", "/home/jovyan/jobs/"+r.Name),
			envVar("DHBW_JOB_WORKDIR", workdir), envVar("DHBW_JOB_COMMAND", command), envVar("DHBW_PROJECT", r.Project), envVar("DHBW_JOB_GRACE_SECONDS", fmt.Sprint(s.jobGraceSeconds())), envVar("PYTHONUNBUFFERED", "1")},
		"workingDir":      "/home/jovyan",
		"securityContext": sec,
		"resources":       map[string]any{"requests": requests, "limits": limits},
		"volumeMounts":    []any{map[string]any{"name": "home", "mountPath": "/home/jovyan", "subPath": home}, shared},
	}}
	if sp.LLMKey != "" {
		// The key from its Secret, so that it is in neither the Job nor the pod spec; the address as it is, like OPENAI_BASE_URL in a workspace.
		c := spec["containers"].([]any)[0].(map[string]any)
		c["env"] = append(c["env"].([]any),
			map[string]any{"name": "OPENAI_API_KEY", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": jobLLMSecretName(r.ID), "key": "OPENAI_API_KEY"}}},
			envVar("OPENAI_BASE_URL", sp.LLMBaseURL))
		if len(sp.LLMAgentEnv) > 0 {
			c["env"] = append(c["env"].([]any), map[string]any{"name": "ANTHROPIC_AUTH_TOKEN", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": jobLLMSecretName(r.ID), "key": "OPENAI_API_KEY"}}})
			names := make([]string, 0, len(sp.LLMAgentEnv))
			for k := range sp.LLMAgentEnv {
				names = append(names, k)
			}
			sort.Strings(names)
			for _, k := range names {
				c["env"] = append(c["env"].([]any), envVar(k, sp.LLMAgentEnv[k]))
			}
		}
	}
	annotations := map[string]any{jobAnnotPrefix + "name": r.Name, "hub.jupyter.org/username": r.Person}
	return map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{
			"name":        r.ID,
			"labels":      map[string]any{"app": jobApp, "dhbw.cloud/project": r.Project, jobLabelUser: hubLabel(r.Person), "kueue.x-k8s.io/queue-name": queue, "kueue.x-k8s.io/priority-class": priority},
			"annotations": annotations,
		},
		"spec": map[string]any{
			"suspend":                 true,
			"backoffLimit":            0,
			"activeDeadlineSeconds":   int64(r.MaxHours * 3600),
			"ttlSecondsAfterFinished": int64(JobTTL / time.Second),
			"template": map[string]any{
				"metadata": map[string]any{"labels": labels, "annotations": annotations},
				"spec":     spec,
			},
		},
	}
}

// HubUserSlug is KubeSpawner's safe slug of a hub user name (slug_scheme "safe", 48 characters, as in {username}): the person's folder homes/<slug> in the project's volume. A name that is no valid object name, as every e-mail address, becomes its safe part, "---" and the start of its SHA-256.
func HubUserSlug(name string) string {
	const maxLen = 48
	if !strings.Contains(name, "--") && validObjectName(name) && len(name) <= maxLen {
		return name
	}
	return stripAndHash(name, maxLen)
}

// hubLabel is KubeSpawner's label value of a hub user name (hub.jupyter.org/username): the name when it is a valid label value, else its safe slug of 32 characters.
func hubLabel(name string) string {
	if !strings.Contains(name, "--") && validLabelValue(name) {
		return name
	}
	return stripAndHash(name, 32)
}

var (
	objectNamePattern = regexp.MustCompile(`^[a-z0-9-]+$`)
	labelValuePattern = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)
	nonAlnumPattern   = regexp.MustCompile(`[^a-z0-9]+`)
)

func validObjectName(s string) bool {
	return len(s) >= 1 && len(s) <= 63 && objectNamePattern.MatchString(s) && s[0] >= 'a' && s[0] <= 'z' && isAlnumLower(s[len(s)-1])
}

func validLabelValue(s string) bool {
	if s == "" {
		return true
	}
	return len(s) <= 63 && labelValuePattern.MatchString(s) && isAlnum(s[0]) && isAlnum(s[len(s)-1])
}

func isAlnumLower(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') }
func isAlnum(c byte) bool      { return isAlnumLower(c) || (c >= 'A' && c <= 'Z') }

func stripAndHash(name string, maxLen int) string {
	sum := sha256.Sum256([]byte(name))
	n := maxLen - 11
	safe := nonAlnumPattern.ReplaceAllString(strings.ToLower(name), "-")
	safe = strings.TrimLeft(safe, "-")
	if len(safe) > n {
		safe = safe[:n]
	}
	safe = strings.TrimRight(safe, "-")
	if safe != "" && !(safe[0] >= 'a' && safe[0] <= 'z') {
		if len(safe) > n-2 {
			safe = safe[:n-2]
		}
		safe = "x-" + safe
	}
	if safe == "" {
		safe = "x"
	}
	return safe + "---" + hex.EncodeToString(sum[:])[:8]
}
