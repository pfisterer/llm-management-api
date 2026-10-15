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
		if base, _, isList := strings.Cut(path, "?"); isList {
			var items []any
			for p, o := range f.objects {
				if strings.HasPrefix(p, base+"/") {
					items = append(items, o)
				}
			}
			b, _ := json.Marshal(map[string]any{"items": items})
			return json.Unmarshal(b, out)
		}
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
	case "DELETE":
		if _, ok := f.objects[path]; !ok {
			return ErrNotFound
		}
		delete(f.objects, path)
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
	s.SetFleet(NewMemoryFleetStore(), FleetConfig{ReserveCPU: 1, ReserveMemoryGi: 2})
	return s, k
}

func TestReconcileFleet(t *testing.T) {
	ctx := context.Background()
	s, k := fleetTestService()
	if err := s.SeedFleet(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.AssignNode(ctx, NodeAssignment{Node: "gpu-1", Pool: "general", Tier: "small"}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileFleet(ctx); err != nil {
		t.Fatal(err)
	}
	nodes, _ := s.GPUNodes(ctx)
	if nodes[0].Class != "general-small" || nodes[0].Assignment == nil {
		t.Fatalf("assigned node: %+v", nodes[0])
	}
	// gpu-2 carries a class nobody knows: it loses the label until an admin assigns it; gpu-3 is new.
	if _, has := k.nodes[1].Metadata.Labels[labelClass]; has || nodes[1].Assignment != nil || nodes[2].SuggestedTier != "large" || nodes[1].SuggestedTier != "medium" {
		t.Fatalf("unknown and new nodes: %+v %+v %v", nodes[1], nodes[2], k.nodes[1].Metadata.Labels)
	}
	if k.nodes[0].Metadata.Labels[labelClass] != "general-small" {
		t.Errorf("assigned node labelled: %v", k.nodes[0].Metadata.Labels)
	}
	cq := k.objects["/apis/kueue.x-k8s.io/v1beta2/clusterqueues/gpu-general-small"]
	b, _ := json.Marshal(cq)
	if !strings.Contains(string(b), `"nominalQuota":"7"`) || !strings.Contains(string(b), `"nominalQuota":"22Gi"`) || !strings.Contains(string(b), `"name":"nvidia.com/gpu","nominalQuota":"1"`) {
		t.Errorf("queue of general-small: %s", b)
	}
	if k.objects["/apis/kueue.x-k8s.io/v1beta2/namespaces/jupyterhub/localqueues/gpu-general-small"] == nil || k.objects["/apis/kueue.x-k8s.io/v1beta2/resourceflavors/general-small"] == nil {
		t.Errorf("flavor and local queue: %v", k.objects)
	}

	// An admin assigns gpu-2 to general/medium: a new class with its own queue.
	if err := s.AssignNode(ctx, NodeAssignment{Node: "gpu-2", Pool: "general", Tier: "medium"}); err != nil {
		t.Fatal(err)
	}
	writes := k.writes
	if err := s.ReconcileFleet(ctx); err != nil {
		t.Fatal(err)
	}
	if k.nodes[1].Metadata.Labels[labelClass] != "general-medium" || k.nodes[1].Metadata.Labels[labelPool] != "general" || k.objects["/apis/kueue.x-k8s.io/v1beta2/clusterqueues/gpu-general-medium"] == nil {
		t.Errorf("assigned node: %v", k.nodes[1].Metadata.Labels)
	}
	classes, _ := s.GPUClasses(ctx)
	if len(classes) != 2 || classes[1].ID != "general-small" || classes[0].ID != "general-medium" || classes[0].Name != "General · medium" || classes[0].GPUs != 1 || classes[0].Products != "NVIDIA-L40S-12Q" {
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
	b, _ = json.Marshal(k.objects["/apis/kueue.x-k8s.io/v1beta2/clusterqueues/gpu-general-medium"])
	if !strings.Contains(string(b), `"name":"nvidia.com/gpu","nominalQuota":"0"`) {
		t.Errorf("empty class keeps its queue with quota 0: %s", b)
	}
}

func TestFleetRefusals(t *testing.T) {
	ctx := context.Background()
	s, _ := fleetTestService()
	_ = s.SeedFleet(ctx)
	var refused *ErrFleet
	for name, err := range map[string]error{
		"unknown pool": s.AssignNode(ctx, NodeAssignment{Node: "gpu-3", Pool: "wi", Tier: "large"}),
		"unknown tier": s.AssignNode(ctx, NodeAssignment{Node: "gpu-3", Pool: "general", Tier: "riesig"}),
		"unknown node": s.AssignNode(ctx, NodeAssignment{Node: "gpu-9", Pool: "general", Tier: "large"}),
		"pool id":      s.SavePool(ctx, Pool{ID: "W I", Name: "WI"}),
		"underscore":   s.SavePool(ctx, Pool{ID: "ma_arm64", Name: "MA"}),
		"double -":     s.SavePool(ctx, Pool{ID: "ma--arm64", Name: "MA"}),
		"trailing -":   s.SavePool(ctx, Pool{ID: "ma-", Name: "MA"}),
		"too long":     s.SavePool(ctx, Pool{ID: "mannheim-arm64-gb10-x", Name: "MA"}),
		"default pool": s.DeletePool(ctx, DefaultPool),
	} {
		if !errors.As(err, &refused) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := s.SavePool(ctx, Pool{ID: "wi", Name: "Wirtschaftsinformatik"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SavePool(ctx, Pool{ID: "mannheim-arm64", Name: "Mannheim arm64"}); err != nil {
		t.Errorf("a hyphen inside the id: %v", err)
	}
	if err := s.AssignNode(ctx, NodeAssignment{Node: "gpu-3", Pool: "wi", Tier: "large"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePool(ctx, "wi"); !errors.As(err, &refused) {
		t.Errorf("pool with a class deleted: %v", err)
	}
}

func dump(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// Two pools of one tier: one cohort; each queue may borrow the other's flavor, which only lends when its pool does.
func TestFleetLending(t *testing.T) {
	ctx := context.Background()
	s, k := fleetTestService()
	_ = s.SeedFleet(ctx)
	_ = s.SavePool(ctx, Pool{ID: "wi", Name: "WI", Lend: true})
	_ = s.AssignNode(ctx, NodeAssignment{Node: "gpu-2", Pool: "general", Tier: "medium"})
	_ = s.AssignNode(ctx, NodeAssignment{Node: "gpu-3", Pool: "wi", Tier: "medium"})
	if err := s.ReconcileFleet(ctx); err != nil {
		t.Fatal(err)
	}
	wi := dump(k.objects["/apis/kueue.x-k8s.io/v1beta2/clusterqueues/gpu-wi-medium"])
	all := dump(k.objects["/apis/kueue.x-k8s.io/v1beta2/clusterqueues/gpu-general-medium"])
	for _, q := range []string{wi, all} {
		if !strings.Contains(q, `"cohortName":"tier-medium"`) || !strings.Contains(q, `"reclaimWithinCohort":"Any"`) {
			t.Errorf("cohort: %s", q)
		}
	}
	// wi lends: no lendingLimit on its own flavor; general keeps its GPUs; each lists the other with no quota of its own.
	if strings.Contains(wi, `"lendingLimit"`) || !strings.Contains(all, `"lendingLimit":"0"`) || !strings.Contains(all, `"name":"wi-medium"`) || !strings.Contains(wi, `"name":"general-medium"`) {
		t.Errorf("lending:\nwi  %s\nall %s", wi, all)
	}
}

// A renamed pool takes its classes along: new ids that remember the old ones (the tree, projects and usage follow), its nodes, and the old Kueue objects go.
func TestRenamePool(t *testing.T) {
	ctx := context.Background()
	s, k := fleetTestService()
	_ = s.SeedFleet(ctx)
	if err := s.SavePool(ctx, Pool{ID: "maarm64", Name: "Mannheim arm64"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AssignNode(ctx, NodeAssignment{Node: "gpu-3", Pool: "maarm64", Tier: "large"}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileFleet(ctx); err != nil {
		t.Fatal(err)
	}
	if k.objects["/apis/kueue.x-k8s.io/v1beta2/clusterqueues/gpu-maarm64-large"] == nil {
		t.Fatalf("queue of the old class missing before: %v", keys(k.objects))
	}
	var moved [][2]string
	s.OnClassRenamed = func(_ context.Context, old, new string) error { moved = append(moved, [2]string{old, new}); return nil }

	var refused *ErrFleet
	for name, err := range map[string]error{
		"general":     s.RenamePool(ctx, DefaultPool, "allgemein"),
		"unknown":     s.RenamePool(ctx, "nope", "ma-arm64"),
		"taken":       s.RenamePool(ctx, "maarm64", DefaultPool),
		"invalid new": s.RenamePool(ctx, "maarm64", "ma_arm64"),
	} {
		if !errors.As(err, &refused) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := s.RenamePool(ctx, "maarm64", "ma-arm64"); err != nil {
		t.Fatal(err)
	}
	classes, _ := s.fleet.Classes(ctx)
	var got []GPUClass
	for _, c := range classes {
		if strings.HasPrefix(c.ID, "ma") {
			got = append(got, c)
		}
	}
	if len(got) != 1 || got[0].ID != "ma-arm64-large" || got[0].Pool != "ma-arm64" || got[0].Replaces != "maarm64-large" {
		t.Fatalf("classes after the rename: %+v", got)
	}
	if len(moved) != 1 || moved[0] != [2]string{"maarm64-large", "ma-arm64-large"} {
		t.Errorf("projects and usage moved: %v", moved)
	}
	assigned, _ := s.fleet.Assignments(ctx)
	for _, a := range assigned {
		if a.Node == "gpu-3" && a.Pool != "ma-arm64" {
			t.Errorf("node stays in the old pool: %+v", a)
		}
	}
	pools, _ := s.fleet.Pools(ctx)
	for _, p := range pools {
		if p.ID == "maarm64" {
			t.Errorf("old pool kept")
		}
		if p.ID == "ma-arm64" && p.Name != "Mannheim arm64" {
			t.Errorf("name lost: %+v", p)
		}
	}
	if k.objects["/apis/kueue.x-k8s.io/v1beta2/clusterqueues/gpu-ma-arm64-large"] == nil {
		t.Errorf("queue of the new class missing")
	}
	for _, p := range []string{"/apis/kueue.x-k8s.io/v1beta2/clusterqueues/gpu-maarm64-large", "/apis/kueue.x-k8s.io/v1beta2/resourceflavors/maarm64-large", "/apis/kueue.x-k8s.io/v1beta2/namespaces/jupyterhub/localqueues/gpu-maarm64-large"} {
		if _, ok := k.objects[p]; ok {
			t.Errorf("%s kept", p)
		}
	}
	if l := k.patches["gpu-3"]; l[labelPool] != "ma-arm64" || l[labelClass] != "ma-arm64-large" {
		t.Errorf("node labels: %v", l)
	}
}
