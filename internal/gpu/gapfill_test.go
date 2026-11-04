package gpu

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
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
	dep := k.objects["/apis/apps/v1/namespaces/inference/deployments/inference-gap-gpu-1"]
	if dep == nil || k.objects["/api/v1/namespaces/inference/services/inference-gap-gpu-1"] == nil || k.objects["/apis/networking.k8s.io/v1/namespaces/inference/ingresses/inference-gap-gpu-1"] == nil {
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
	if !strings.Contains(dump(k.objects["/apis/apps/v1/namespaces/inference/deployments/inference-gap-gpu-2"]), `"Qwen/Qwen3-8B-AWQ"`) {
		t.Errorf("gpu-2 runs the tier's model")
	}
	if _, ok := k.objects["/apis/apps/v1/namespaces/inference/deployments/inference-gap-gpu-1"]; ok {
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

func TestGapHeldBack(t *testing.T) {
	s := &Service{}
	r := gapReplica{name: "gap-gpu-1", node: "gpu-1"}
	busy := map[string]time.Time{"gpu-1": time.Now().Add(-2 * time.Minute)}
	if !s.gapHeldBack(r, busy, nil) {
		t.Errorf("GPU in use 2 min ago (default delay 5 min): the gap filler waits")
	}
	if s.gapHeldBack(r, map[string]time.Time{"gpu-1": time.Now().Add(-6 * time.Minute)}, nil) {
		t.Errorf("GPU free for 6 min: the gap filler comes back")
	}
	if s.gapHeldBack(r, map[string]time.Time{}, nil) {
		t.Errorf("node never busy: the gap filler runs")
	}
	admitted := podInfo{}
	admitted.Metadata.Labels = map[string]string{"replica": "gap-gpu-1"}
	if s.gapHeldBack(r, busy, []podInfo{admitted}) {
		t.Errorf("a gap filler pod that Kueue admitted is not taken down")
	}
	gated := admitted
	gated.Spec.SchedulingGates = []struct {
		Name string `json:"name"`
	}{{Name: "kueue.x-k8s.io/admission"}}
	if !s.gapHeldBack(r, busy, []podInfo{gated}) {
		t.Errorf("a gap filler pod still waiting for admission is held back")
	}
	zero := 0
	s.fleetCfg.Inference.GapDelayMinutes = &zero
	if s.gapHeldBack(r, busy, nil) {
		t.Errorf("delay 0: never held back")
	}
}

// A catalogue model with engine strata runs the Strata image with its settings as variables, the key from the shared secret and its own data volume; without a Strata image it gets no replica, and a node that changes engine gets a new Deployment.
func TestStrataGapFiller(t *testing.T) {
	ctx := context.Background()
	s, k := gapTestService()
	s.SetModels(NewMemoryModelStore())
	_ = s.SeedFleet(ctx)
	_ = s.SeedModels(ctx)

	var refused *ErrFleet
	for name, m := range map[string]CatalogModel{
		"no quant":      {Key: "big", Engine: EngineStrata, Model: "qwen3.8-flash-next-iq3_s", Args: []string{"--family", "qwen"}},
		"unknown":       {Key: "big", Engine: EngineStrata, Model: "qwen3.8-flash-next-iq3_s", Args: []string{"--family", "qwen", "--quant", "IQ3_S", "--save-session", "x"}},
		"parallel":      {Key: "big", Engine: EngineStrata, Model: "qwen3.8-flash-next-iq3_s", Args: []string{"--family", "qwen", "--quant", "IQ3_S", "--parallel", "16"}},
		"ple-io":        {Key: "big", Engine: EngineStrata, Model: "qwen3.8-flash-next-iq3_s", Args: []string{"--family", "qwen", "--quant", "IQ3_S", "--ple-io", "tmpfs"}},
		"served name":   {Key: "big", Engine: EngineStrata, Model: "Qwen/Qwen3.8", Args: []string{"--family", "qwen", "--quant", "IQ3_S"}},
		"engine":        {Key: "big", Engine: "sglang", Model: "a/b"},
		"value with $x": {Key: "big", Engine: EngineStrata, Model: "qwen3.8-flash-next-iq3_s", Args: []string{"--family", "$(id)", "--quant", "IQ3_S"}},
	} {
		if err := s.SaveModel(ctx, m); !errors.As(err, &refused) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
	if err := s.SaveModel(ctx, CatalogModel{Key: "big", Engine: EngineStrata, Model: "qwen3.8-flash-next-iq3_s", Args: []string{"--quant=IQ3_S", "--family qwen", "--ple-io", "ssd", "--parallel", "4", "--context", "65536"}, CPU: "8", MemoryReq: "64Gi", MemoryLimit: "100Gi"}); err != nil {
		t.Fatal(err)
	}
	cat, _ := s.ModelCatalog(ctx)
	for _, m := range cat.Models {
		if m.Key == "big" && strings.Join(m.Args, " ") != "--family qwen --quant IQ3_S --context 65536 --parallel 4 --ple-io ssd" {
			t.Errorf("settings not normalised: %v", m.Args)
		}
	}

	_ = s.AssignNode(ctx, NodeAssignment{Node: "gpu-2", Pool: "general", Tier: "medium", GapModel: "big"})
	_ = s.ReconcileFleet(ctx)
	_ = s.ReconcileGapFillers(ctx)
	if _, ok := k.objects["/apis/apps/v1/namespaces/inference/deployments/inference-gap-gpu-2"]; ok {
		t.Errorf("Strata replica without a Strata image")
	}

	s.fleetCfg.Inference.StrataImage = "registry.example/platform/strata:1"
	if err := s.ReconcileGapFillers(ctx); err != nil {
		t.Fatal(err)
	}
	dep := dump(k.objects["/apis/apps/v1/namespaces/inference/deployments/inference-gap-gpu-2"])
	for _, want := range []string{`"image":"registry.example/platform/strata:1"`, `"name":"STRATA_QUANT","value":"IQ3_S"`, `"name":"STRATA_PLE_IO","value":"ssd"`, `"key":"VLLM_API_KEY"`, `"IPC_LOCK"`, `"path":"/var/lib/dhbw-gpu/strata"`, `"dhbw.cloud/inference-engine":"strata"`, `"dhbw.cloud/inference-served-model":"qwen3.8-flash-next-iq3_s"`, `"app":"inference"`, `"memory":"64Gi"`, `"terminationGracePeriodSeconds":120`} {
		if !strings.Contains(dep, want) {
			t.Errorf("Strata deployment lacks %s: %s", want, dep)
		}
	}
	if strings.Contains(dep, `"args"`) || strings.Contains(dep, "huggingface") {
		t.Errorf("Strata deployment has vLLM's arguments or cache: %s", dep)
	}
	if got := cacheKeep(k); got["gpu-2"] != "" {
		t.Errorf("Strata model in the Hugging Face cache list: %v", got)
	}

	// Back to vLLM: the reconcile removes the Strata Deployment (engine label differs) and creates the vLLM one under the same name.
	k.deleted = nil
	_ = s.AssignNode(ctx, NodeAssignment{Node: "gpu-2", Pool: "general", Tier: "medium"})
	_ = s.ReconcileGapFillers(ctx)
	if !slices.Contains(k.deleted, "/apis/apps/v1/namespaces/inference/deployments/inference-gap-gpu-2") {
		t.Errorf("engine change did not recreate the Deployment: deleted %v", k.deleted)
	}
	if !strings.Contains(dump(k.objects["/apis/apps/v1/namespaces/inference/deployments/inference-gap-gpu-2"]), `"dhbw.cloud/inference-engine":"vllm"`) {
		t.Errorf("vLLM replica after the engine change missing")
	}
}
