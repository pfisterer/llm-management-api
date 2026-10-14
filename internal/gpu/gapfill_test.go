package gpu

import (
	"context"
	"strings"
	"testing"
)

func gapTestService() (*Service, *fakeFleetKube) {
	s, k := fleetTestService()
	s.fleetCfg.Inference = InferenceConfig{Image: "vllm/vllm-openai:v1", Hostname: "inference.example",
		Models:     map[string]InferenceModel{"small": {Model: "Qwen/Qwen2.5-0.5B-Instruct", Args: []string{"--enforce-eager"}}, "medium": {Model: "Qwen/Qwen3-8B-AWQ"}},
		TierModels: map[string]string{"klein": "small", "mittel": "medium"}}
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
	if k.objects["/apis/kueue.x-k8s.io/v1beta2/namespaces/inference/localqueues/inference-allgemein-klein"] == nil {
		t.Errorf("inference queue of the class missing")
	}
	b := dump(dep)
	for _, want := range []string{`"Qwen/Qwen2.5-0.5B-Instruct"`, `"--enforce-eager"`, `"kueue.x-k8s.io/queue-name":"inference-allgemein-klein"`, `"kubernetes.io/hostname":"gpu-1"`, `"kueue.x-k8s.io/priority-class":"gap-filler"`} {
		if !strings.Contains(b, want) {
			t.Errorf("deployment lacks %s: %s", want, b)
		}
	}
	if _, ok := k.objects["/apis/apps/v1/namespaces/inference/deployments/vllm-gap-gpu-2"]; ok {
		t.Errorf("unplanned gap filler of before kept")
	}

	// gpu-2 assigned with the tier's model, gpu-1 switched off.
	_ = s.AssignNode(ctx, NodeAssignment{Node: "gpu-2", Pool: "allgemein", Tier: "mittel"})
	_ = s.AssignNode(ctx, NodeAssignment{Node: "gpu-1", Pool: "allgemein", Tier: "klein", GapModel: NoGapFiller})
	_ = s.ReconcileFleet(ctx)
	_ = s.ReconcileGapFillers(ctx)
	if !strings.Contains(dump(k.objects["/apis/apps/v1/namespaces/inference/deployments/vllm-gap-gpu-2"]), `"Qwen/Qwen3-8B-AWQ"`) {
		t.Errorf("gpu-2 runs the tier's model")
	}
	if _, ok := k.objects["/apis/apps/v1/namespaces/inference/deployments/vllm-gap-gpu-1"]; ok {
		t.Errorf("switched-off gap filler kept")
	}
	if err := s.AssignNode(ctx, NodeAssignment{Node: "gpu-1", Pool: "allgemein", Tier: "klein", GapModel: "unknown"}); err == nil {
		t.Errorf("unknown model accepted")
	}
}

func keys(m map[string]map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
