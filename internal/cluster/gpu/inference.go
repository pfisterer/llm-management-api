package gpu

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"
)

// InferenceSitePrefix is put in front of a replica's name for its site in the discovery job and in LiteLLM (gpu-gap-<node>), so it cannot clash with a fleet machine.
const InferenceSitePrefix = "gpu-"

// InferenceReplica is one replica of the inference pool (vLLM or Strata) that is ready to serve. Ansible creates the replicas (one gap filler per GPU node, optionally a base load per GPU class); Kueue admits and evicts them. Only the running ones are listed, so an evicted gap filler drops out of LiteLLM at the next discovery run and comes back when it runs again.
type InferenceReplica struct {
	Name     string `json:"name"`      // label replica; also the path under InferenceURL
	APIBase  string `json:"api_base"`  // <InferenceURL>/<name>/v1
	GPUClass string `json:"gpu_class"` // label dhbw.cloud/gpu-class
	Role     string `json:"role"`      // label dhbw.cloud/inference-role: gap-filler or base-load
	Weight   int    `json:"weight"`    // the node's NodeAssignment.Weight, 1 without one
	Slots    int    `json:"slots"`     // requests the replica works on at the same time (replicaSlots), 0 = unknown
}

// podLister is the part of the Kubernetes client the inference listing needs.
type podLister interface {
	listPods(ctx context.Context, ns, selector string) ([]podInfo, error)
}

// InferenceEnabled tells whether the inference pool is configured.
func (s *Service) InferenceEnabled() bool { return s.cfg.InferenceURL != "" && s.pods != nil }

// InferenceAPIKey is the key the replicas expect.
func (s *Service) InferenceAPIKey() string { return s.cfg.InferenceAPIKey }

// InferenceReplicas lists the ready replicas, sorted by name. A pod that is terminating counts as gone: vLLM may still answer for up to a minute, but the GPU is already promised to someone else.
func (s *Service) InferenceReplicas(ctx context.Context) ([]InferenceReplica, error) {
	ns := s.cfg.InferenceNamespace
	if ns == "" {
		ns = "inference"
	}
	pods, err := s.pods.listPods(ctx, ns, "app="+inferenceApp)
	if err != nil {
		return nil, err
	}
	// The weight is the node's: how fast a replica answers depends on the machine it runs on.
	weights := map[string]int{}
	if s.fleet != nil {
		as, err := s.fleet.Assignments(ctx)
		if err != nil {
			return nil, err
		}
		for _, a := range as {
			weights[a.Node] = replicaWeight(&a)
		}
	}
	base := strings.TrimRight(s.cfg.InferenceURL, "/")
	seen := map[string]bool{}
	out := []InferenceReplica{}
	for _, p := range pods {
		name := p.Metadata.Labels["replica"]
		if name == "" || seen[name] || p.Metadata.DeletionTimestamp != nil || !podReady(p) {
			continue
		}
		seen[name] = true
		out = append(out, InferenceReplica{Name: name, APIBase: base + "/" + name + "/v1",
			GPUClass: p.Metadata.Labels["dhbw.cloud/gpu-class"], Role: p.Metadata.Labels["dhbw.cloud/inference-role"], Weight: max(weights[p.Spec.NodeName], 1), Slots: replicaSlots(p)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// strataDefaultParallel is the slots of a Strata replica without STRATA_PARALLEL (images/strata/entrypoint.sh).
const strataDefaultParallel = 4

// replicaSlots is how many requests a replica works on at the same time, read from its pod as it runs: a Strata replica (label dhbw.cloud/inference-engine strata) from STRATA_PARALLEL (the catalogue's --parallel), default 4; a vLLM replica from --max-num-seqs in its arguments (the catalogue's or, for the base load, the inventory's inference.models), else 0 = unknown, since vLLM's own default (256 or more sequences) says nothing about how many requests it answers at a useful speed. The discovery job turns it into LiteLLM's max_parallel_requests where the replica has overflow members behind it (fleet.Site.Slots).
func replicaSlots(p podInfo) int {
	strata := p.Metadata.Labels[labelEngine] == EngineStrata
	for _, c := range p.Spec.Containers {
		if strata {
			for _, e := range c.Env {
				if e.Name == "STRATA_PARALLEL" {
					if n, err := strconv.Atoi(e.Value); err == nil && n > 0 {
						return n
					}
				}
			}
			continue
		}
		var fields []string
		for _, a := range c.Args {
			fields = append(fields, strings.Fields(a)...)
		}
		for i, f := range fields {
			val, ok := strings.CutPrefix(f, "--max-num-seqs=")
			if !ok && f == "--max-num-seqs" && i+1 < len(fields) {
				val, ok = fields[i+1], true
			}
			if n, err := strconv.Atoi(val); ok && err == nil && n > 0 {
				return n
			}
		}
	}
	if strata {
		return strataDefaultParallel
	}
	return 0
}

// Replica states as the admin page shows them. German words like the fleet's states; the UI maps them to translations.
const (
	InferenceReady       = "bereit"         // Ready: serves and is registered in LiteLLM
	InferenceStarting    = "startet"        // on its GPU, loading the model
	InferenceWaiting     = "wartet auf GPU" // gated by Kueue or not yet scheduled: the GPU is busy with something of higher priority
	InferenceTerminating = "wird beendet"   // evicted or replaced, gives the GPU back
)

// InferencePod is one pod of the inference pool for the admin page, in whatever state.
type InferencePod struct {
	Replica  string `json:"replica"`
	Pod      string `json:"pod"`
	Site     string `json:"site"` // name in the discovery job and in LiteLLM
	Node     string `json:"node,omitempty"`
	GPUClass string `json:"gpu_class"`
	Role     string `json:"role" enums:"gap-filler,base-load"`
	Model    string `json:"model"`
	State    string `json:"state" enums:"bereit,startet,wartet auf GPU,wird beendet"`
	// ModelKey is the catalogue model of a gap filler (label dhbw.cloud/inference-model); empty for the base load.
	ModelKey string `json:"model_key,omitempty"`
	Restarts int    `json:"restarts"`
	// Problem is why the container is not running or last ended, e.g. "CrashLoopBackOff" or "OOMKilled".
	Problem string     `json:"problem,omitempty"`
	Since   *time.Time `json:"since"` // start on the node, or creation while it still waits
}

// InferencePool is the inference pool for the admin page.
type InferencePool struct {
	Enabled bool           `json:"enabled"`
	URL     string         `json:"url,omitempty"`
	Pods    []InferencePod `json:"pods"`
}

// InferencePool lists every replica's pod with its state, sorted by replica.
func (s *Service) InferencePool(ctx context.Context) (InferencePool, error) {
	out := InferencePool{Pods: []InferencePod{}}
	if !s.InferenceEnabled() {
		return out, nil
	}
	out.Enabled, out.URL = true, s.cfg.InferenceURL
	ns := s.cfg.InferenceNamespace
	if ns == "" {
		ns = "inference"
	}
	pods, err := s.pods.listPods(ctx, ns, "app="+inferenceApp)
	if err != nil {
		return out, err
	}
	for _, p := range pods {
		name := p.Metadata.Labels["replica"]
		// Pods that ended (evicted, completed) stay as records until garbage collected; their deployment already runs a successor.
		if name == "" || p.Status.Phase == "Failed" || p.Status.Phase == "Succeeded" {
			continue
		}
		ip := InferencePod{Replica: name, Pod: p.Metadata.Name, Site: InferenceSitePrefix + name, Node: p.Spec.NodeName,
			GPUClass: p.Metadata.Labels["dhbw.cloud/gpu-class"], Role: p.Metadata.Labels["dhbw.cloud/inference-role"], Model: servedModel(p), ModelKey: p.Metadata.Labels["dhbw.cloud/inference-model"]}
		for _, cs := range p.Status.ContainerStatuses {
			ip.Restarts += cs.RestartCount
			if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" && cs.State.Waiting.Reason != "ContainerCreating" && cs.State.Waiting.Reason != "PodInitializing" {
				ip.Problem = cs.State.Waiting.Reason
			} else if cs.LastState.Terminated != nil && cs.RestartCount > 0 {
				ip.Problem = cs.LastState.Terminated.Reason
			}
		}
		switch {
		case p.Metadata.DeletionTimestamp != nil:
			ip.State = InferenceTerminating
		case podReady(p):
			ip.State = InferenceReady
		case len(p.Spec.SchedulingGates) > 0 || p.Spec.NodeName == "":
			ip.State = InferenceWaiting
		default:
			ip.State = InferenceStarting
		}
		if p.Status.StartTime != nil {
			ip.Since = p.Status.StartTime
		} else if !p.Metadata.CreationTimestamp.IsZero() {
			t := p.Metadata.CreationTimestamp
			ip.Since = &t
		}
		out.Pods = append(out.Pods, ip)
	}
	sort.SliceStable(out.Pods, func(i, j int) bool { return out.Pods[i].Replica < out.Pods[j].Replica })
	return out, nil
}

// servedModel is the model name a replica serves: its annotation, else vLLM's arguments (--served-model-name, else --model; base load from Ansible).
func servedModel(p podInfo) string {
	if m := p.Metadata.Annotations[annotationServed]; m != "" {
		return m
	}
	var model string
	for _, c := range p.Spec.Containers {
		for i := 0; i+1 < len(c.Args); i++ {
			switch c.Args[i] {
			case "--served-model-name":
				return c.Args[i+1]
			case "--model":
				model = c.Args[i+1]
			}
		}
	}
	return model
}

func podReady(p podInfo) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == "Ready" {
			return c.Status == "True"
		}
	}
	return false
}
