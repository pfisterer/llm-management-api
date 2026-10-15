package gpu

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func gapTestService() (*Service, *fakeFleetKube) {
	s, k := fleetTestService()
	s.fleetCfg.Inference = InferenceConfig{Image: "vllm/vllm-openai:v1", Hostname: "inference.example",
		Models:     map[string]InferenceModel{"small": {Model: "Qwen/Qwen2.5-0.5B-Instruct", Args: []string{"--enforce-eager"}}, "medium": {Model: "Qwen/Qwen3-8B-AWQ"}},
		TierModels: map[string]string{"small": "small", "medium": "medium"}}
	_ = s.SeedFleet(context.Background())
	_ = s.AssignNode(context.Background(), NodeAssignment{Node: "gpu-1", Pool: "general", Tier: "small"})
	return s, k
}

func TestReconcileGapFillers(t *testing.T) {
	ctx := context.Background()
	s, k := gapTestService()
	_ = s.SeedFleet(ctx)
	_ = s.ReconcileFleet(ctx)
	// Ansible's gap filler of before on gpu-2 (not assigned yet): it goes.
	k.objects["/apis/apps/v1/namespaces/inference/deployments/vllm-gap-gpu-2"] = map[string]any{"metadata": map[string]any{"name": "vllm-gap-gpu-2", "labels": map[string]any{"app": "vllm"}}}
	if err := s.ReconcileGapFillers(ctx); err != nil {
		t.Fatal(err)
	}
	dep := k.objects["/apis/apps/v1/namespaces/inference/deployments/vllm-gap-gpu-1"]
	if dep == nil || k.objects["/api/v1/namespaces/inference/services/vllm-gap-gpu-1"] == nil || k.objects["/apis/networking.k8s.io/v1/namespaces/inference/ingresses/vllm-gap-gpu-1"] == nil {
		t.Fatalf("gap filler of the assigned node: %v", keys(k.objects))
	}
	if k.objects["/apis/kueue.x-k8s.io/v1beta2/namespaces/inference/localqueues/inference-general-small"] == nil {
		t.Errorf("inference queue of the class missing")
	}
	b := dump(dep)
	for _, want := range []string{`"Qwen/Qwen2.5-0.5B-Instruct"`, `"--enforce-eager"`, `"kueue.x-k8s.io/queue-name":"inference-general-small"`, `"kubernetes.io/hostname":"gpu-1"`, `"kueue.x-k8s.io/priority-class":"gap-filler"`} {
		if !strings.Contains(b, want) {
			t.Errorf("deployment lacks %s: %s", want, b)
		}
	}
	if _, ok := k.objects["/apis/apps/v1/namespaces/inference/deployments/vllm-gap-gpu-2"]; ok {
		t.Errorf("unplanned gap filler of before kept")
	}
	if got := cacheKeep(k); got["gpu-1"] != "Qwen/Qwen2.5-0.5B-Instruct" || got["gpu-2"] != "" {
		t.Errorf("model cache keeps %v, want gpu-1 its gap model and gpu-2 nothing", got)
	}

	// gpu-2 assigned with the tier's model, gpu-1 switched off.
	_ = s.AssignNode(ctx, NodeAssignment{Node: "gpu-2", Pool: "general", Tier: "medium"})
	_ = s.AssignNode(ctx, NodeAssignment{Node: "gpu-1", Pool: "general", Tier: "small", GapModel: NoGapFiller})
	_ = s.ReconcileFleet(ctx)
	_ = s.ReconcileGapFillers(ctx)
	if !strings.Contains(dump(k.objects["/apis/apps/v1/namespaces/inference/deployments/vllm-gap-gpu-2"]), `"Qwen/Qwen3-8B-AWQ"`) {
		t.Errorf("gpu-2 runs the tier's model")
	}
	if _, ok := k.objects["/apis/apps/v1/namespaces/inference/deployments/vllm-gap-gpu-1"]; ok {
		t.Errorf("switched-off gap filler kept")
	}
	if got := cacheKeep(k); got["gpu-1"] != "" || got["gpu-2"] != "Qwen/Qwen3-8B-AWQ" {
		t.Errorf("model cache keeps %v after the switch", got)
	}
	if err := s.AssignNode(ctx, NodeAssignment{Node: "gpu-1", Pool: "general", Tier: "small", GapModel: "unknown"}); err == nil {
		t.Errorf("unknown model accepted")
	}
}

func cacheKeep(k *fakeFleetKube) map[string]any {
	cm := k.objects["/api/v1/namespaces/inference/configmaps/"+modelCacheKeepName]
	if cm == nil {
		return nil
	}
	b, _ := json.Marshal(cm["data"])
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

// A node keeps the model of its gap filler and whatever a vLLM pod on it runs: the base load, or a gap filler not yet on its new model.
func TestModelCacheKeep(t *testing.T) {
	models := map[string]CatalogModel{"small": {Key: "small", Model: "Qwen/S"}, "large": {Key: "large", Model: "Qwen/L"}}
	nodes := []GPUNode{
		{Name: "a", Class: "general-small", Assignment: &NodeAssignment{Tier: "small"}},
		{Name: "b", Class: "general-large", Assignment: &NodeAssignment{Tier: "large", GapModel: NoGapFiller}},
		{Name: "c"},
	}
	var pods []podInfo
	_ = json.Unmarshal([]byte(`[
		{"spec":{"nodeName":"b","containers":[{"args":["--model=Qwen/Base","--port","8000"]}]}},
		{"spec":{"nodeName":"a","containers":[{"args":["--model","Qwen/Old","--served-model-name","old"]}]}}
	]`), &pods)
	got := modelCacheKeep(nodes, models, map[string]string{"small": "small", "large": "large"}, pods)
	want := map[string]string{"a": "Qwen/Old\nQwen/S", "b": "Qwen/Base", "c": ""}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for n, w := range want {
		if got[n] != w {
			t.Errorf("%s keeps %q, want %q", n, got[n], w)
		}
	}
}

func keys(m map[string]map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
