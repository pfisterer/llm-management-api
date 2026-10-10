package gpu

import (
	"context"
	"sort"
	"strings"
)

// InferenceReplica is one vLLM replica of the inference pool that is ready to serve. Ansible creates the replicas (one gap filler per GPU node, optionally a base load per GPU class); Kueue admits and evicts them. Only the running ones are listed, so an evicted gap filler drops out of LiteLLM at the next discovery run and comes back when it runs again.
type InferenceReplica struct {
	Name     string `json:"name"`      // label replica; also the path under InferenceURL
	APIBase  string `json:"api_base"`  // <InferenceURL>/<name>/v1
	GPUClass string `json:"gpu_class"` // label dhbw.cloud/gpu-class
	Role     string `json:"role"`      // label dhbw.cloud/inference-role: gap-filler or base-load
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
	pods, err := s.pods.listPods(ctx, ns, "app=vllm")
	if err != nil {
		return nil, err
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
			GPUClass: p.Metadata.Labels["dhbw.cloud/gpu-class"], Role: p.Metadata.Labels["dhbw.cloud/inference-role"]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func podReady(p podInfo) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == "Ready" {
			return c.Status == "True"
		}
	}
	return false
}
