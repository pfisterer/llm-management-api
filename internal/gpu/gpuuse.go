package gpu

import (
	"context"
	"net/http"
	"net/url"
)

// nodeInfo is the part of a node the GPU capacity needs.
type nodeInfo struct {
	Metadata struct {
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		Unschedulable bool `json:"unschedulable"`
	} `json:"spec"`
	Status struct {
		Allocatable map[string]string `json:"allocatable"`
		Conditions  []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
	} `json:"status"`
}

type nodeLister interface {
	listNodes(ctx context.Context, selector string) ([]nodeInfo, error)
}

func (k *kube) listNodes(ctx context.Context, selector string) ([]nodeInfo, error) {
	var list struct {
		Items []nodeInfo `json:"items"`
	}
	err := k.do(ctx, http.MethodGet, "/api/v1/nodes?labelSelector="+url.QueryEscape(selector), nil, &list)
	return list.Items, err
}

// GPUCapacity is how many GPUs (vGPU slices) each class has right now: the allocatable nvidia.com/gpu of the ready, schedulable nodes labelled dhbw.cloud/gpu-class. nil without a cluster.
func (s *Service) GPUCapacity(ctx context.Context) (map[string]float64, error) {
	k, ok := s.pods.(nodeLister)
	if !ok {
		return nil, nil
	}
	nodes, err := k.listNodes(ctx, "dhbw.cloud/gpu-class")
	if err != nil {
		return nil, err
	}
	out := map[string]float64{}
	for _, n := range nodes {
		ready := false
		for _, c := range n.Status.Conditions {
			if c.Type == "Ready" {
				ready = c.Status == "True"
			}
		}
		if !ready || n.Spec.Unschedulable {
			continue
		}
		out[n.Metadata.Labels["dhbw.cloud/gpu-class"]] += parseCPU(n.Status.Allocatable["nvidia.com/gpu"])
	}
	return out, nil
}

// InferenceGPUs is how many GPUs the inference pool holds per class: its pods that are on a node (starting, serving or ending), one GPU each.
func (s *Service) InferenceGPUs(ctx context.Context) (map[string]float64, error) {
	pool, err := s.InferencePool(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]float64{}
	for _, p := range pool.Pods {
		if p.State != InferenceWaiting && p.GPUClass != "" {
			out[p.GPUClass]++
		}
	}
	return out, nil
}
