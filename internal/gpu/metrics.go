package gpu

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/pfisterer/llm-management-api/internal/promtext"
)

// WorkspaceMetrics renders what the cluster says about the workspaces for the service's /metrics: the running and waiting servers and GPU kernels by profile and GPU class, the GPUs they hold, how long starts took, the project image builds and the prepull. Rendered when asked (the caller caches it briefly); one listing of the workspace pods, one read of the builds.
//
// The start duration is the time from a workspace pod's creation (the hub's start, or a GPU kernel's) to its Ready condition: it includes waiting in Kueue for a GPU, pulling the image and the init containers (course material). It is observed when a listing first sees a pod ready, so a pod that becomes ready and ends between two listings is missed, and pods that were ready already at the first listing after a restart are not counted (their start happened before). A pod that never becomes ready (refused by Kueue for good, image not pullable) is not observed; the waiting gauges show it.
//
// No person or project in a label: profile (the catalogue's, gpu-kernel or default; anything else = other), the GPU class (the GPU administration's ids) and the state.
type WorkspaceMetrics struct {
	svc      *Service
	profiles []string
	duration *promtext.HistogramVec

	mu     sync.Mutex
	seen   map[string]bool // pods (name and creation) of the last listing, true once observed ready
	seeded bool
}

// StartBuckets are the upper bounds of the start duration in seconds: a CPU server with its image on the node starts in seconds, a GPU server may wait minutes for its GPU.
var StartBuckets = []float64{5, 10, 20, 30, 45, 60, 90, 120, 180, 300, 600, 1200, 1800, 3600}

// NewWorkspaceMetrics makes the renderer; profiles are the known profile labels (workspace.Service.MetricsProfiles).
func NewWorkspaceMetrics(s *Service, profiles []string) *WorkspaceMetrics {
	return &WorkspaceMetrics{svc: s, profiles: profiles, seen: map[string]bool{},
		duration: promtext.NewHistogramVec("aiaas_workspace_start_duration_seconds", "Time from a workspace pod's creation to its readiness (Kueue wait, image pull, init containers), by profile. Observed since the service started.", StartBuckets, "profile")}
}

func (m *WorkspaceMetrics) profile(p string) string {
	if slices.Contains(m.profiles, p) {
		return p
	}
	return "other"
}

// Render writes the families; an error listing the pods fails it (the caller reports the part as failed rather than showing no servers).
func (m *WorkspaceMetrics) Render(ctx context.Context) ([]byte, error) {
	pods, err := m.svc.WorkspacePods(ctx)
	if err != nil {
		return nil, err
	}
	m.observe(pods)

	var w promtext.Writer
	type key struct{ profile, class, state string }
	servers, gpus := map[key]float64{}, map[[2]string]float64{}
	for _, p := range m.profiles {
		for _, st := range []string{"running", "starting", "queued"} {
			servers[key{p, "", st}] += 0
		}
	}
	for _, p := range pods {
		st := "running"
		switch {
		case p.Queued:
			st = "queued"
		case !p.Running:
			st = "starting"
		}
		servers[key{m.profile(p.Profile), p.GPUClass, st}]++
		if p.GPUs > 0 {
			gpus[[2]string{p.GPUClass, st}] += p.GPUs
		}
	}
	w.Header("aiaas_workspace_servers", "gauge", "Workspace pods (Jupyter servers and GPU kernels) by profile, GPU class (empty without GPU) and state: running, starting (on a node, not running yet: image pull, init containers) or queued (waiting for its place: gated by Kueue or on no node yet).")
	for _, k := range slices.SortedFunc(maps.Keys(servers), func(a, b key) int {
		return cmp.Or(strings.Compare(a.profile, b.profile), strings.Compare(a.class, b.class), strings.Compare(a.state, b.state))
	}) {
		w.Sample("aiaas_workspace_servers", servers[k], "profile", k.profile, "gpu_class", k.class, "state", k.state)
	}
	w.Header("aiaas_workspace_gpus", "gauge", "GPUs of workspace pods by GPU class and state (running, starting, queued); a starting pod holds its GPU already.")
	for _, k := range slices.SortedFunc(maps.Keys(gpus), func(a, b [2]string) int {
		return cmp.Or(strings.Compare(a[0], b[0]), strings.Compare(a[1], b[1]))
	}) {
		w.Sample("aiaas_workspace_gpus", gpus[k], "gpu_class", k[0], "state", k[1])
	}
	m.duration.WriteTo(&w)

	builds := map[Status]int{StatusBuilding: 0, StatusReady: 0, StatusFailed: 0}
	if m.svc.store != nil {
		envs, err := m.svc.store.List(ctx, "")
		if err != nil {
			return nil, err
		}
		for _, e := range envs {
			if _, ok := ProjectOf(e); ok {
				builds[e.Status]++
			}
		}
	}
	w.Header("aiaas_env_builds", "gauge", "Project images built from Git (one per project, the current build) by status: building, ready or failed; followed every five minutes.")
	for _, st := range []Status{StatusBuilding, StatusFailed, StatusReady} {
		w.Sample("aiaas_env_builds", float64(builds[st]), "status", string(st))
	}
	w.Header("aiaas_prepull_images", "gauge", "Images the prepull DaemonSet of an architecture holds on the nodes (bases and the active projects' images), as of its last update; missing before the first.")
	for _, arch := range prepullArchs {
		if n, ok := m.svc.prepulled.Load(arch); ok {
			w.Sample("aiaas_prepull_images", float64(n.(int)), "arch", arch)
		}
	}
	m.svc.writeCleanupMetrics(&w)
	return w.Bytes(), nil
}

// observe records the start duration of every pod that is ready for the first time in a listing; the first listing only notes the pods ready already.
func (m *WorkspaceMetrics) observe(pods []WorkspacePod) {
	m.mu.Lock()
	defer m.mu.Unlock()
	present := map[string]bool{}
	for _, p := range pods {
		if p.Created.IsZero() {
			continue
		}
		// A pod that was ready, became unready and ready again keeps its key and is not observed twice.
		k := p.Pod + "@" + p.Created.UTC().Format(time.RFC3339)
		present[k] = true
		if _, ok := m.seen[k]; !ok {
			m.seen[k] = false
		}
		if p.ReadyAt == nil || m.seen[k] {
			continue
		}
		m.seen[k] = true
		if m.seeded {
			m.duration.Observe(max(0, p.ReadyAt.Sub(p.Created).Seconds()), m.profile(p.Profile))
		}
	}
	for k := range m.seen {
		if !present[k] {
			delete(m.seen, k)
		}
	}
	m.seeded = true
}
