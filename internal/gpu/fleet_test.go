package gpu

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// fakeFleetKube is a cluster with GPU nodes and a store of Kueue objects by path.
type fakeFleetKube struct {
	nodes   []nodeInfo
	patches map[string]map[string]any
	objects map[string]map[string]any
	writes  int
}

func (f *fakeFleetKube) listPods(context.Context, string, string) ([]podInfo, error) { return nil, nil }

func (f *fakeFleetKube) listNodes(context.Context, string) ([]nodeInfo, error) { return f.nodes, nil }

func (f *fakeFleetKube) patchNodeLabels(_ context.Context, node string, labels map[string]any) error {
	f.patches[node] = labels
	for i := range f.nodes {
		if f.nodes[i].Metadata.Name == node {
			for k, v := range labels {
				if v == nil {
					delete(f.nodes[i].Metadata.Labels, k)
				} else {
					f.nodes[i].Metadata.Labels[k] = v.(string)
				}
			}
		}
	}
	return nil
}

func (f *fakeFleetKube) do(_ context.Context, method, path string, body, out any) error {
	switch method {
	case "GET":
		o, ok := f.objects[path]
		if !ok {
			return ErrNotFound
		}
		b, _ := json.Marshal(o)
		return json.Unmarshal(b, out)
	case "POST":
		obj := body.(map[string]any)
		f.objects[path+"/"+obj["metadata"].(map[string]any)["name"].(string)] = obj
		f.writes++
	case "PUT":
		f.objects[path] = body.(map[string]any)
		f.writes++
	}
	return nil
}

func gpuNode(name, class, product, memMiB, gpus string) nodeInfo {
	var n nodeInfo
	n.Metadata.Name = name
	n.Metadata.Labels = map[string]string{"dhbw.cloud/node-type": "gpu", "nvidia.com/gpu.product": product, "nvidia.com/gpu.memory": memMiB}
	if class != "" {
		n.Metadata.Labels[labelClass] = class
	}
	n.Status.Allocatable = map[string]string{"nvidia.com/gpu": gpus, "cpu": "8", "memory": "24Gi"}
	n.Status.Conditions = []struct {
		Type   string `json:"type"`
		Status string `json:"status"`
	}{{Type: "Ready", Status: "True"}}
	return n
}

func fleetTestService() (*Service, *fakeFleetKube) {
	s, _, _, _ := testService()
	k := &fakeFleetKube{patches: map[string]map[string]any{}, objects: map[string]map[string]any{},
		nodes: []nodeInfo{gpuNode("gpu-1", "l4-3q", "NVIDIA-L4-3Q", "3072", "1"), gpuNode("gpu-2", "l40s-12q", "NVIDIA-L40S-12Q", "12288", "1"), gpuNode("gpu-3", "", "NVIDIA-H100", "81920", "1")}}
	s.pods = k
	s.SetFleet(NewMemoryFleetStore(), FleetConfig{ReserveCPU: 1, ReserveMemoryGi: 2, Legacy: map[string]LegacyClass{"l4-3q": {Pool: "allgemein", Tier: "klein"}}})
	return s, k
}

func TestReconcileFleet(t *testing.T) {
	ctx := context.Background()
	s, k := fleetTestService()
	if err := s.SeedFleet(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileFleet(ctx); err != nil {
		t.Fatal(err)
	}
	nodes, _ := s.GPUNodes(ctx)
	if nodes[0].Class != "l4-3q" || nodes[0].Assignment == nil || nodes[0].Assignment.UpdatedBy != "adopted" {
		t.Fatalf("legacy node adopted: %+v", nodes[0])
	}
	// gpu-2 carries a class nobody knows: it loses the label until an admin assigns it; gpu-3 is new.
	if _, has := k.nodes[1].Metadata.Labels[labelClass]; has || nodes[1].Assignment != nil || nodes[2].SuggestedTier != "gross" || nodes[1].SuggestedTier != "mittel" {
		t.Fatalf("unknown and new nodes: %+v %+v %v", nodes[1], nodes[2], k.nodes[1].Metadata.Labels)
	}
	cq := k.objects["/apis/kueue.x-k8s.io/v1beta2/clusterqueues/gpu-l4-3q"]
	b, _ := json.Marshal(cq)
	if !strings.Contains(string(b), `"nominalQuota":"7"`) || !strings.Contains(string(b), `"nominalQuota":"22Gi"`) || !strings.Contains(string(b), `{"name":"nvidia.com/gpu","nominalQuota":"1"}`) {
		t.Errorf("queue of l4-3q: %s", b)
	}
	if k.objects["/apis/kueue.x-k8s.io/v1beta2/namespaces/jupyterhub/localqueues/gpu-l4-3q"] == nil || k.objects["/apis/kueue.x-k8s.io/v1beta2/resourceflavors/l4-3q"] == nil {
		t.Errorf("flavor and local queue: %v", k.objects)
	}

	// An admin assigns gpu-2 to allgemein/mittel: a new class with its own queue.
	if err := s.AssignNode(ctx, NodeAssignment{Node: "gpu-2", Pool: "allgemein", Tier: "mittel"}); err != nil {
		t.Fatal(err)
	}
	writes := k.writes
	if err := s.ReconcileFleet(ctx); err != nil {
		t.Fatal(err)
	}
	if k.nodes[1].Metadata.Labels[labelClass] != "allgemein-mittel" || k.nodes[1].Metadata.Labels[labelPool] != "allgemein" || k.objects["/apis/kueue.x-k8s.io/v1beta2/clusterqueues/gpu-allgemein-mittel"] == nil {
		t.Errorf("assigned node: %v", k.nodes[1].Metadata.Labels)
	}
	classes, _ := s.GPUClasses(ctx)
	if len(classes) != 2 || classes[0].ID != "allgemein-mittel" || classes[0].Name != "Allgemein · mittel" || classes[0].GPUs != 1 || classes[0].Products != "NVIDIA-L40S-12Q" {
		t.Errorf("classes: %+v", classes)
	}
	// Nothing changed: nothing is written.
	writes = k.writes
	_ = s.ReconcileFleet(ctx)
	if k.writes != writes {
		t.Errorf("unchanged objects rewritten: %d writes", k.writes-writes)
	}

	// Taken out: the labels go, the class stays (with quota 0).
	if err := s.AssignNode(ctx, NodeAssignment{Node: "gpu-2"}); err != nil {
		t.Fatal(err)
	}
	_ = s.ReconcileFleet(ctx)
	if _, has := k.nodes[1].Metadata.Labels[labelClass]; has {
		t.Errorf("unassigned node keeps its class: %v", k.nodes[1].Metadata.Labels)
	}
	b, _ = json.Marshal(k.objects["/apis/kueue.x-k8s.io/v1beta2/clusterqueues/gpu-allgemein-mittel"])
	if !strings.Contains(string(b), `{"name":"nvidia.com/gpu","nominalQuota":"0"}`) {
		t.Errorf("empty class keeps its queue with quota 0: %s", b)
	}
}

func TestFleetRefusals(t *testing.T) {
	ctx := context.Background()
	s, _ := fleetTestService()
	_ = s.SeedFleet(ctx)
	var refused *ErrFleet
	for name, err := range map[string]error{
		"unknown pool": s.AssignNode(ctx, NodeAssignment{Node: "gpu-3", Pool: "wi", Tier: "gross"}),
		"unknown tier": s.AssignNode(ctx, NodeAssignment{Node: "gpu-3", Pool: "allgemein", Tier: "riesig"}),
		"unknown node": s.AssignNode(ctx, NodeAssignment{Node: "gpu-9", Pool: "allgemein", Tier: "gross"}),
		"pool id":      s.SavePool(ctx, Pool{ID: "W I", Name: "WI"}),
		"default pool": s.DeletePool(ctx, DefaultPool),
	} {
		if !errors.As(err, &refused) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := s.SavePool(ctx, Pool{ID: "wi", Name: "Wirtschaftsinformatik"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AssignNode(ctx, NodeAssignment{Node: "gpu-3", Pool: "wi", Tier: "gross"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePool(ctx, "wi"); !errors.As(err, &refused) {
		t.Errorf("pool with a class deleted: %v", err)
	}
}
