package gpu

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestModelCatalog(t *testing.T) {
	ctx := context.Background()
	s, k := gapTestService()
	s.fleetCfg.Inference.Models["medium"] = InferenceModel{Model: "Qwen/Qwen3-8B-AWQ", Args: []string{"--max-model-len", "12288"},
		Resources: map[string]any{"requests": map[string]any{"cpu": "2", "memory": "8Gi"}, "limits": map[string]any{"memory": "12Gi"}}}
	s.SetModels(NewMemoryModelStore())
	_ = s.SeedFleet(ctx)
	if err := s.SeedModels(ctx); err != nil {
		t.Fatal(err)
	}
	cat, _ := s.ModelCatalog(ctx)
	if len(cat.Models) != 2 || cat.TierModels["medium"] != "medium" || cat.Models[0].Key != "medium" || cat.Models[0].CPU != "2" || cat.Models[0].MemoryLimit != "12Gi" {
		t.Fatalf("seeded catalogue: %+v", cat)
	}
	// A second start does not overwrite what admins changed.
	_ = s.SaveModel(ctx, CatalogModel{Key: "medium", Model: "Qwen/Qwen3-8B-AWQ", Args: []string{"--max-model-len", "8192"}, MinGB: 10})
	_ = s.SeedModels(ctx)
	cat, _ = s.ModelCatalog(ctx)
	if cat.Models[0].Args[1] != "8192" || cat.Models[0].MinGB != 10 {
		t.Errorf("seed overwrote the catalogue: %+v", cat.Models[0])
	}

	var refused *ErrFleet
	for name, err := range map[string]error{
		"key":        s.SaveModel(ctx, CatalogModel{Key: "Groß", Model: "a/b"}),
		"repo":       s.SaveModel(ctx, CatalogModel{Key: "x", Model: "not a repo"}),
		"port":       s.SaveModel(ctx, CatalogModel{Key: "x", Model: "a/b", Args: []string{"--port", "9"}}),
		"quantity":   s.SaveModel(ctx, CatalogModel{Key: "x", Model: "a/b", MemoryReq: "lots"}),
		"tier":       s.SetTierModel(ctx, "riesig", "small"),
		"tier model": s.SetTierModel(ctx, "small", "nope"),
		"in use":     s.DeleteModel(ctx, "medium"),
	} {
		if !errors.As(err, &refused) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}

	// The gap filler of gpu-2 (tier medium) runs the catalogue's version.
	_ = s.AssignNode(ctx, NodeAssignment{Node: "gpu-2", Pool: "general", Tier: "medium"})
	_ = s.ReconcileFleet(ctx)
	if err := s.ReconcileGapFillers(ctx); err != nil {
		t.Fatal(err)
	}
	dep := dump(k.objects["/apis/apps/v1/namespaces/inference/deployments/vllm-gap-gpu-2"])
	if !strings.Contains(dep, `"8192"`) || !strings.Contains(dep, `"memory":"3Gi"`) || !strings.Contains(dep, `"dhbw.cloud/inference-model":"medium"`) {
		t.Errorf("gap filler from the catalogue: %s", dep)
	}
	// Without a tier default and no choice at the node, no gap filler.
	_ = s.SetTierModel(ctx, "medium", "")
	_ = s.ReconcileGapFillers(ctx)
	if _, ok := k.objects["/apis/apps/v1/namespaces/inference/deployments/vllm-gap-gpu-2"]; ok {
		t.Errorf("gap filler without a model kept")
	}
	if err := s.DeleteModel(ctx, "medium"); err != nil {
		t.Errorf("unused model not deleted: %v", err)
	}
}

// vLLM never gets an argument that logs prompts or answers: the catalogue refuses it, and a gap filler drops it even when it got into the store another way (an old entry, the inventory seed).
func TestNoRequestLoggingArgs(t *testing.T) {
	for _, a := range []string{"--enable-log-requests", "--enable-log-outputs", "--enable_log_requests", "--enable-log-requests=true", "--Enable-Log-Requests", "--enable-log", "--max-model-len 4096 --enable-log-outputs"} {
		if !requestLoggingArg(a) {
			t.Errorf("%q not recognised as request logging", a)
		}
	}
	for _, a := range []string{"--disable-log-stats", "--max-model-len", "4096", "--enable-lora", "--reasoning-parser", "--enable-prefix-caching"} {
		if requestLoggingArg(a) {
			t.Errorf("%q wrongly taken for request logging", a)
		}
	}

	ctx := context.Background()
	s, k := gapTestService()
	s.SetModels(NewMemoryModelStore())
	_ = s.SeedFleet(ctx)
	_ = s.SeedModels(ctx)
	var refused *ErrFleet
	for _, a := range []string{"--enable-log-requests", "--enable-log-outputs", "--enable_log_requests=1"} {
		if err := s.SaveModel(ctx, CatalogModel{Key: "medium", Model: "a/b", Args: []string{"--max-model-len", "4096", a}}); !errors.As(err, &refused) {
			t.Errorf("SaveModel accepted %s: %v", a, err)
		}
	}

	if err := s.models.SaveModel(ctx, CatalogModel{Key: "medium", Model: "a/b", Args: []string{"--max-model-len", "4096", "--enable-log-requests", "--enable-log-outputs"}}); err != nil {
		t.Fatal(err)
	}
	_ = s.SetTierModel(ctx, "medium", "medium")
	_ = s.AssignNode(ctx, NodeAssignment{Node: "gpu-2", Pool: "general", Tier: "medium"})
	_ = s.ReconcileFleet(ctx)
	if err := s.ReconcileGapFillers(ctx); err != nil {
		t.Fatal(err)
	}
	dep := dump(k.objects["/apis/apps/v1/namespaces/inference/deployments/vllm-gap-gpu-2"])
	if !strings.Contains(dep, `"4096"`) || strings.Contains(dep, "enable-log") {
		t.Errorf("gap filler args: %s", dep)
	}
}
