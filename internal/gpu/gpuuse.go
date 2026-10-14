package gpu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// nodeInfo is the part of a node the GPU capacity needs.
type nodeInfo struct {
	Metadata struct {
		Name   string            `json:"name"`
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

// patchNodeLabels sets labels of a node; a nil value removes the label (JSON merge patch).
func (k *kube) patchNodeLabels(ctx context.Context, node string, labels map[string]any) error {
	body, err := json.Marshal(map[string]any{"metadata": map[string]any{"labels": labels}})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, k.base+"/api/v1/nodes/"+url.PathEscape(node), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+k.token)
	req.Header.Set("Content-Type", "application/merge-patch+json")
	resp, err := k.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("kubernetes PATCH node %s: %d %s", node, resp.StatusCode, msg)
	}
	return nil
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
