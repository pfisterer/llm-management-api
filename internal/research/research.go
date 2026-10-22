// Package research keeps a pseudonymous log of the GPU cluster's workloads for publications about the platform: one row per pod (GPU kernel, workspace server, vLLM replica, image build) with when it was requested, admitted by Kueue, put on a node, running and ready, when and why it ended, and for a waiting GPU kernel the queue position and start estimate JupyterLab showed. Sampled every minute like the usage; the times are the cluster's own (pod, Kueue Workload, events), so they are exact except for pods that vanish between two samples without a trace (EndEstimated).
//
// Never stored: the person's identity (only an HMAC pseudonym with a key from the configuration, none without a key), pod names (a server's carries the e-mail), prompts, code, file names, addresses.
package research

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Kinds of workload.
const (
	KindGPUKernel = "gpu-kernel" // GPU kernel pod of "CPU + GPU-Kernel"
	KindGPUServer = "gpu-server" // workspace server with a GPU
	KindCPUServer = "cpu-server" // workspace server without a GPU
	KindVLLM      = "vllm"       // vLLM replica of the inference pool (gap filler or base load)
	KindBuild     = "build"      // build of a project image
)

// Why a workload ended (EndReason); EndDetail keeps the cluster's own reason where there is one.
const (
	EndCompleted      = "completed"       // the pod succeeded: a build finished, a server or kernel process ended itself (for a server: its idle release by shutdown_no_activity, or File → Shut Down)
	EndFailed         = "failed"          // a container failed
	EndOOM            = "oom"             // a container ran out of memory
	EndMaxRuntime     = "max_runtime"     // the upper bound of the release times (activeDeadlineSeconds)
	EndEvictedNode    = "evicted_node"    // the kubelet evicted it (local storage over its limit, memory pressure)
	EndNodeFailure    = "node_failure"    // its node went away (taint manager)
	EndEvictedAPI     = "evicted_api"     // evicted through the eviction API (node drain)
	EndPreempted      = "preempted"       // Kueue preempted it for another workload (PreemptedBy)
	EndDeactivated    = "deactivated"     // Kueue deactivated its workload (InactiveWorkload)
	EndEvictedKueue   = "evicted_kueue"   // Kueue evicted it for another reason (EndDetail)
	EndIdleRelease    = "idle_release"    // a GPU kernel its server ended after the idle time (release times)
	EndStoppedPerson  = "stopped_person"  // a GPU kernel the person shut down
	EndStoppedServer  = "stopped_server"  // a GPU kernel its notebook server ended without saying why (the person, or the idle release)
	EndCancelled      = "cancelled"       // a waiting GPU kernel the person cancelled
	EndLaunchTimeout  = "launch_timeout"  // a waiting GPU kernel its server gave up on (waited too long)
	EndStoppedPortal  = "stopped_portal"  // a server stopped in the self-service portal or through the API
	EndBudget         = "budget"          // a server ended because the budget in its project was used up
	EndProjectDeleted = "project_deleted" // a server ended because its project is gone
	EndServerStopped  = "server_stopped"  // a GPU kernel whose server ended
	EndHubStopped     = "hub_stopped"     // a server pod the hub deleted: its control panel, its idle culler, or after the server ended itself
	EndDeleted        = "deleted"         // deleted, the cause unknown (a gap filler moved or held back, by hand)
	EndGone           = "gone"            // vanished while nothing was known about it (e.g. ended while the service was down)
)

// Workload is one pod of the GPU cluster from its request to its end. Times are UTC; nil = not (yet) reached or not known.
type Workload struct {
	UID         string  `json:"uid" gorm:"primaryKey"`                       // the pod's UID
	WorkloadUID string  `json:"workload_uid,omitempty"`                      // its Kueue Workload's UID
	Kind        string  `json:"kind" gorm:"index"`                           // KindGPUKernel …
	GPUClass    string  `json:"gpu_class,omitempty" gorm:"column:gpu_class"` // the class chosen (label) or of its node selector
	Flavor      string  `json:"flavor,omitempty"`                            // the Kueue flavor it was admitted to (another pool of the tier when borrowed)
	GPUProduct  string  `json:"gpu_product,omitempty" gorm:"column:gpu_product"`
	GPUs        float64 `json:"gpus" gorm:"column:gpus"`
	Node        string  `json:"node,omitempty"` // the node it ran on
	// PriorityClass is Kueue's (workload priority class, e.g. gap-filler, base-load) or the pod's; Priority the pod's priority value.
	PriorityClass string `json:"priority_class,omitempty"`
	Priority      *int32 `json:"priority,omitempty"`
	Project       string `json:"project,omitempty"` // project slug
	Person        string `json:"person,omitempty"`  // HMAC-SHA256 pseudonym (hex); empty without a key or without a person
	Profile       string `json:"profile,omitempty"`
	Model         string `json:"model,omitempty"` // the model a vLLM replica serves
	// Created is when the pod was made (the request); AdmittedAt when Kueue admitted its workload (quota reserved); ScheduledAt when it was bound to a node (after a preemption: when the victim had freed the GPU); RunningAt when its first container started; ReadyAt when it first became ready.
	Created     time.Time  `json:"created_at" gorm:"column:created_at;index"`
	AdmittedAt  *time.Time `json:"admitted_at,omitempty"`
	ScheduledAt *time.Time `json:"scheduled_at,omitempty"`
	RunningAt   *time.Time `json:"running_at,omitempty"`
	ReadyAt     *time.Time `json:"ready_at,omitempty"`
	// EndedAt is when it ended (deletion asked for, containers finished, Kueue's stop); with EndEstimated it is when the sampler found it gone, and it ended between LastSeenAt and EndedAt.
	EndedAt      *time.Time `json:"ended_at,omitempty" gorm:"index"`
	EndReason    string     `json:"end_reason,omitempty"`
	EndDetail    string     `json:"end_detail,omitempty"`
	EndEstimated bool       `json:"end_estimated,omitempty"`
	// PreemptedBy is the UID of the pod (or Kueue workload) a preempted workload made room for.
	PreemptedBy string `json:"preempted_by,omitempty"`
	// QueuePosition and EstimatedStart are what JupyterLab first showed for a waiting GPU kernel (1 = next; "spätestens frei um"), at QueueShownAt.
	QueuePosition  int        `json:"queue_position,omitempty"`
	EstimatedStart *time.Time `json:"estimated_start,omitempty"`
	QueueShownAt   *time.Time `json:"queue_shown_at,omitempty"`
	LastSeenAt     time.Time  `json:"last_seen_at"`
	UpdatedAt      time.Time  `json:"-"`
}

func (Workload) TableName() string { return "research_workloads" }

// Merge takes into dst what src knows and dst does not: the first value of a field wins, an end is taken once with its reason; LastSeenAt is the later one. Both stores and the recorder merge this way, so samples of two instances or after a restart never overwrite each other.
func Merge(dst *Workload, src Workload) {
	str := func(d *string, s string) {
		if *d == "" {
			*d = s
		}
	}
	at := func(d **time.Time, s *time.Time) {
		if *d == nil && s != nil {
			v := s.UTC()
			*d = &v
		}
	}
	str(&dst.UID, src.UID)
	str(&dst.WorkloadUID, src.WorkloadUID)
	str(&dst.Kind, src.Kind)
	str(&dst.GPUClass, src.GPUClass)
	str(&dst.Flavor, src.Flavor)
	str(&dst.GPUProduct, src.GPUProduct)
	str(&dst.Node, src.Node)
	str(&dst.PriorityClass, src.PriorityClass)
	str(&dst.Project, src.Project)
	str(&dst.Person, src.Person)
	str(&dst.Profile, src.Profile)
	str(&dst.Model, src.Model)
	if dst.GPUs == 0 {
		dst.GPUs = src.GPUs
	}
	if dst.Priority == nil && src.Priority != nil {
		v := *src.Priority
		dst.Priority = &v
	}
	if dst.Created.IsZero() {
		dst.Created = src.Created.UTC()
	}
	at(&dst.AdmittedAt, src.AdmittedAt)
	at(&dst.ScheduledAt, src.ScheduledAt)
	at(&dst.RunningAt, src.RunningAt)
	at(&dst.ReadyAt, src.ReadyAt)
	if dst.EndedAt == nil && src.EndedAt != nil {
		at(&dst.EndedAt, src.EndedAt)
		dst.EndReason, dst.EndDetail, dst.EndEstimated, dst.PreemptedBy = src.EndReason, src.EndDetail, src.EndEstimated, src.PreemptedBy
	}
	if dst.QueuePosition == 0 && src.QueuePosition != 0 {
		dst.QueuePosition = src.QueuePosition
		at(&dst.EstimatedStart, src.EstimatedStart)
		at(&dst.QueueShownAt, src.QueueShownAt)
	}
	if src.LastSeenAt.After(dst.LastSeenAt) {
		dst.LastSeenAt = src.LastSeenAt.UTC()
	}
}

// Pod is one pod as the cluster lists it at a sample. Person, Server and OwnerUID stay in memory: they connect the pod with what the service itself ends; only the pseudonym of Person is stored.
type Pod struct {
	UID, WorkloadUID, Kind string
	Person                 string // JupyterHub user name (e-mail)
	Server                 string // JupyterHub server name of a workspace server
	OwnerUID               string // a GPU kernel's server pod
	Project, Profile       string
	GPUClass, Flavor       string
	GPUProduct, Node       string
	PriorityClass, Model   string
	Priority               *int32
	GPUs                   float64
	Created                time.Time
	AdmittedAt             *time.Time
	ScheduledAt            *time.Time
	RunningAt              *time.Time
	ReadyAt                *time.Time
	Phase                  string     // Pending, Running, Succeeded, Failed
	FinishedAt             *time.Time // the last container's end of a finished pod
	DeletingAt             *time.Time // when its deletion was asked for (deletionTimestamp less the grace period)
	StatusReason           string     // status.reason (DeadlineExceeded, Evicted)
	ContainerReason        string     // why a container ended other than Completed (OOMKilled, Error)
	Disruption             string     // reason of its DisruptionTarget condition
	// KueueEvicted is the reason of its Workload's Evicted (or Preempted) condition, KueueMessage its message (the preemptor's UID), KueueEvictedAt its time.
	KueueEvicted   string
	KueueMessage   string
	KueueEvictedAt *time.Time
}

// Event is a Kubernetes event that tells why Kueue stopped a pod: "Stopped" on the pod, "Preempted" on its Workload. Pods that vanish between two samples are explained by them.
type Event struct {
	UID     string // the pod's or the Workload's UID
	Reason  string
	Message string
	At      time.Time
}

// Snapshot is the cluster at one sample.
type Snapshot struct {
	Pods   []Pod
	Events []Event
}

// Source lists the cluster; an error leaves everything as it is (nothing is ended for a failed listing).
type Source func(ctx context.Context) (Snapshot, error)

// Store keeps the rows.
type Store interface {
	// Save merges the rows into the stored ones (Merge).
	Save(ctx context.Context, rows []Workload) error
	// Open is every row that has not ended.
	Open(ctx context.Context) ([]Workload, error)
	// Range is the rows created in [from, to), oldest first.
	Range(ctx context.Context, from, to time.Time) ([]Workload, error)
	// Prune deletes the rows created before.
	Prune(ctx context.Context, before time.Time) error
}

// Pseudonym is the HMAC-SHA256 of the identity (lower case) under key, hex; empty without key or identity. Stable as long as the key is: a new key gives everybody new pseudonyms.
func Pseudonym(key []byte, identity string) string {
	identity = strings.ToLower(strings.TrimSpace(identity))
	if len(key) == 0 || identity == "" {
		return ""
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte(identity))
	return hex.EncodeToString(m.Sum(nil))
}

// tracked is a row that has not ended, with what only the memory keeps.
type tracked struct {
	row            Workload
	person, server string
	owner          string
	restored       bool // loaded from the store after a restart: nothing in memory about it
}

type hint struct {
	reason string
	at     time.Time
}

type shown struct {
	position int
	estimate *time.Time
	at       time.Time
}

// hintTTL is how long a reason the service gave for an end it caused waits for the pod to end.
const hintTTL = 15 * time.Minute

// Recorder samples the cluster and keeps the rows. Its methods for the hooks are safe on a nil Recorder (no log).
type Recorder struct {
	store     Store
	source    Source
	key       []byte
	retention time.Duration
	interval  time.Duration
	now       func() time.Time
	log       *zap.SugaredLogger

	mu      sync.Mutex
	loaded  bool
	open    map[string]*tracked
	done    map[string]time.Time // ended pods still listed (finished pods stay until deleted), and when they ended
	unsaved map[string]Workload
	hints   map[string]hint // by pod UID
	servers map[string]hint // by person and server name
	shown   map[string]shown
	pruned  time.Time
}

// NewRecorder samples source every interval into store; key pseudonymises the people (empty = no person stored), retention is how long rows are kept.
func NewRecorder(store Store, source Source, key []byte, retention, interval time.Duration, log *zap.SugaredLogger) *Recorder {
	return &Recorder{store: store, source: source, key: key, retention: retention, interval: interval, now: time.Now, log: log,
		open: map[string]*tracked{}, done: map[string]time.Time{}, unsaved: map[string]Workload{}, hints: map[string]hint{}, servers: map[string]hint{}, shown: map[string]shown{}}
}

// Ending notes why the service is about to end a pod (before it deletes it).
func (r *Recorder) Ending(uid, reason string) {
	if r == nil || uid == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hints[uid] = hint{reason, r.now()}
}

// ServerEnding notes why the service is about to stop a person's workspace server (through the hub, which deletes its pod).
func (r *Recorder) ServerEnding(person, server, reason string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.servers[serverKey(person, server)] = hint{reason, r.now()}
}

// QueueShown notes what JupyterLab shows a waiting GPU kernel: its place in the queue and when a GPU is free at the latest (nil = not known); the first one shown is kept.
func (r *Recorder) QueueShown(uid string, position int, estimate *time.Time) {
	if r == nil || uid == "" || position <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.open[uid]; ok && t.row.QueuePosition != 0 {
		return
	}
	if _, ok := r.shown[uid]; !ok {
		r.shown[uid] = shown{position, estimate, r.now()}
	}
}

func serverKey(person, server string) string { return strings.ToLower(person) + "\x00" + server }

// Tick takes one sample and saves what changed; rows not saved (database away) are tried again at the next tick.
func (r *Recorder) Tick(ctx context.Context) error {
	if !r.loaded {
		rows, err := r.store.Open(ctx)
		if err != nil {
			return err
		}
		r.mu.Lock()
		for _, w := range rows {
			r.open[w.UID] = &tracked{row: w, restored: true}
		}
		r.loaded = true
		r.mu.Unlock()
	}
	snap, err := r.source(ctx)
	if err != nil {
		return err
	}
	now := r.now()
	r.mu.Lock()
	r.apply(snap, now)
	rows := make([]Workload, 0, len(r.unsaved))
	for _, w := range r.unsaved {
		rows = append(rows, w)
	}
	r.mu.Unlock()
	sort.Slice(rows, func(i, j int) bool { return rows[i].UID < rows[j].UID })
	if err := r.store.Save(ctx, rows); err != nil {
		return err
	}
	// Only Tick changes them, so what was saved is all there is.
	r.mu.Lock()
	clear(r.unsaved)
	r.mu.Unlock()
	if r.retention > 0 && now.Sub(r.pruned) > 24*time.Hour {
		r.pruned = now
		if err := r.store.Prune(ctx, now.Add(-r.retention)); err != nil {
			r.log.Warnw("research log: pruning failed", "error", err)
		}
	}
	return nil
}

// Run ticks until ctx ends; errors are logged.
func (r *Recorder) Run(ctx context.Context) {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		if err := r.Tick(ctx); err != nil {
			r.log.Warnw("research log: sample failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// apply brings the rows to the snapshot; r.mu is held.
func (r *Recorder) apply(s Snapshot, now time.Time) {
	podOf := map[string]string{} // Workload UID → pod UID
	for uid, t := range r.open {
		if t.row.WorkloadUID != "" {
			podOf[t.row.WorkloadUID] = uid
		}
	}
	present := map[string]*Pod{}
	for i := range s.Pods {
		p := &s.Pods[i]
		present[p.UID] = p
		if p.WorkloadUID != "" {
			podOf[p.WorkloadUID] = p.UID
		}
	}
	stops := map[string]Event{} // pod UID → Kueue's earliest stop
	for _, e := range s.Events {
		uid := e.UID
		if p, ok := podOf[uid]; ok {
			uid = p
		}
		if cur, ok := stops[uid]; !ok || e.At.Before(cur.At) {
			stops[uid] = e
		}
	}
	for i := range s.Pods {
		p := &s.Pods[i]
		if _, ended := r.done[p.UID]; ended {
			continue
		}
		t, ok := r.open[p.UID]
		if !ok {
			t = &tracked{row: Workload{UID: p.UID}}
			r.open[p.UID] = t
		}
		t.person, t.server, t.owner, t.restored = p.Person, p.Server, p.OwnerUID, false
		Merge(&t.row, Workload{WorkloadUID: p.WorkloadUID, Kind: p.Kind, GPUClass: p.GPUClass, Flavor: p.Flavor, GPUProduct: p.GPUProduct, GPUs: p.GPUs, Node: p.Node,
			PriorityClass: p.PriorityClass, Priority: p.Priority, Project: p.Project, Person: Pseudonym(r.key, p.Person), Profile: p.Profile, Model: p.Model,
			Created: p.Created, AdmittedAt: p.AdmittedAt, ScheduledAt: p.ScheduledAt, RunningAt: p.RunningAt, ReadyAt: p.ReadyAt, LastSeenAt: now})
		if q, ok := r.shown[p.UID]; ok {
			Merge(&t.row, Workload{QueuePosition: q.position, EstimatedStart: q.estimate, QueueShownAt: &q.at})
			delete(r.shown, p.UID)
		}
		if e, ok := r.endOfListed(t, p, present, stops, now); ok {
			Merge(&t.row, e)
		}
		r.unsaved[p.UID] = t.row
		if t.row.EndedAt != nil {
			delete(r.open, p.UID)
			r.done[p.UID] = *t.row.EndedAt
		}
	}
	// Servers before kernels: a kernel that vanished with its server ended when the server did.
	var gone []string
	for uid := range r.open {
		if present[uid] == nil {
			gone = append(gone, uid)
		}
	}
	sort.Slice(gone, func(i, j int) bool {
		ki, kj := r.open[gone[i]].row.Kind == KindGPUKernel, r.open[gone[j]].row.Kind == KindGPUKernel
		if ki != kj {
			return kj
		}
		return gone[i] < gone[j]
	})
	for _, uid := range gone {
		t := r.open[uid]
		Merge(&t.row, r.endOfVanished(t, present, stops, now))
		r.unsaved[uid] = t.row
		delete(r.open, uid)
		r.done[uid] = *t.row.EndedAt
	}
	for uid, at := range r.done {
		if present[uid] == nil && now.Sub(at) > time.Hour {
			delete(r.done, uid)
		}
	}
	for k, h := range r.hints {
		if now.Sub(h.at) > hintTTL {
			delete(r.hints, k)
		}
	}
	for k, h := range r.servers {
		if now.Sub(h.at) > hintTTL {
			delete(r.servers, k)
		}
	}
	for k, q := range r.shown {
		if now.Sub(q.at) > time.Hour {
			delete(r.shown, k)
		}
	}
}

// endOfListed is the end of a pod still listed: finished, or being deleted.
func (r *Recorder) endOfListed(t *tracked, p *Pod, present map[string]*Pod, stops map[string]Event, now time.Time) (Workload, bool) {
	if p.DeletingAt != nil {
		e := r.cause(t, p, present, stops)
		at := *p.DeletingAt
		e.EndedAt = &at
		return e, true
	}
	if p.Phase != "Succeeded" && p.Phase != "Failed" {
		return Workload{}, false
	}
	at := now
	if p.FinishedAt != nil {
		at = *p.FinishedAt
	}
	e := Workload{EndedAt: &at, EndReason: EndCompleted}
	if p.Phase == "Failed" {
		switch {
		case p.StatusReason == "DeadlineExceeded":
			e.EndReason, e.EndDetail = EndMaxRuntime, p.StatusReason
		case p.StatusReason == "Evicted":
			e.EndReason, e.EndDetail = EndEvictedNode, p.StatusReason
		case p.ContainerReason == "OOMKilled":
			e.EndReason, e.EndDetail = EndOOM, p.ContainerReason
		case p.KueueEvicted != "":
			k := kueueEnd(p.KueueEvicted, p.KueueMessage)
			k.EndedAt = &at
			return k, true
		default:
			e.EndReason, e.EndDetail = EndFailed, firstOf(p.StatusReason, p.ContainerReason)
		}
	}
	return e, true
}

// endOfVanished is the end of a pod no longer listed: Kueue's stop event or the time the service ended it give the exact time, else it ended since it was last seen.
func (r *Recorder) endOfVanished(t *tracked, present map[string]*Pod, stops map[string]Event, now time.Time) Workload {
	e := r.cause(t, nil, present, stops)
	if ev, ok := stops[t.row.UID]; ok && !ev.At.IsZero() {
		at := ev.At
		e.EndedAt = &at
		return e
	}
	if h, ok := r.hints[t.row.UID]; ok {
		e.EndedAt = &h.at
		return e
	}
	if h, ok := r.servers[serverKey(t.person, t.server)]; ok && t.person != "" && isServer(t.row.Kind) {
		e.EndedAt = &h.at
		return e
	}
	if e.EndReason == EndServerStopped {
		if at, ok := r.done[t.owner]; ok {
			e.EndedAt = &at
			return e
		}
	}
	at := now
	e.EndedAt, e.EndEstimated = &at, true
	return e
}

// cause is why a pod is being deleted (p) or vanished (p nil).
func (r *Recorder) cause(t *tracked, p *Pod, present map[string]*Pod, stops map[string]Event) Workload {
	if ev, ok := stops[t.row.UID]; ok {
		return kueueEnd(ev.Reason, ev.Message)
	}
	if p != nil && p.KueueEvicted != "" {
		return kueueEnd(p.KueueEvicted, p.KueueMessage)
	}
	if h, ok := r.hints[t.row.UID]; ok {
		return Workload{EndReason: h.reason}
	}
	if h, ok := r.servers[serverKey(t.person, t.server)]; ok && t.person != "" && isServer(t.row.Kind) {
		return Workload{EndReason: h.reason}
	}
	if p != nil && p.Disruption != "" {
		reason := map[string]string{"PreemptionByScheduler": EndPreempted, "EvictionByEvictionAPI": EndEvictedAPI, "DeletionByTaintManager": EndNodeFailure, "TerminationByKubelet": EndEvictedNode}[p.Disruption]
		return Workload{EndReason: firstOf(reason, EndDeleted), EndDetail: p.Disruption}
	}
	switch {
	case t.restored:
		return Workload{EndReason: EndGone}
	case t.row.Kind == KindGPUKernel:
		if o := present[t.owner]; t.owner != "" && (o == nil || o.DeletingAt != nil || o.Phase == "Succeeded" || o.Phase == "Failed") {
			return Workload{EndReason: EndServerStopped}
		}
		return Workload{EndReason: EndDeleted}
	case isServer(t.row.Kind):
		return Workload{EndReason: EndHubStopped}
	}
	return Workload{EndReason: EndDeleted}
}

func isServer(kind string) bool { return kind == KindGPUServer || kind == KindCPUServer }

var (
	jobUIDRE = regexp.MustCompile(`JobUID: ([0-9a-fA-F-]{36})`)
	uidRE    = regexp.MustCompile(`\bUID: ([0-9a-fA-F-]{36})`)
)

// kueueEnd reads why Kueue stopped a workload, from an event (Stopped, Preempted) or the Evicted condition (Preempted, InactiveWorkload, …); a preemption's message names the preemptor ("Preempted to accommodate a workload (UID: …, JobUID: …) due to …"), whose pod UID (JobUID) is kept.
func kueueEnd(reason, message string) Workload {
	e := Workload{EndReason: EndEvictedKueue, EndDetail: reason}
	switch {
	case reason == "Preempted" || strings.Contains(message, "Preempted"):
		e.EndReason = EndPreempted
		if m := jobUIDRE.FindStringSubmatch(message); m != nil {
			e.PreemptedBy = strings.ToLower(m[1])
		} else if m := uidRE.FindStringSubmatch(message); m != nil {
			e.PreemptedBy = strings.ToLower(m[1])
		}
	case reason == "InactiveWorkload" || strings.Contains(strings.ToLower(message), "deactivated"):
		e.EndReason = EndDeactivated
	}
	return e
}

func firstOf(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}
