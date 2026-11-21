package webserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/pfisterer/llm-management-api/internal/cluster/workspace"
	"github.com/pfisterer/llm-management-api/internal/llm/litellm"
	"github.com/pfisterer/llm-management-api/internal/llm/modeldesc"
	"github.com/pfisterer/llm-management-api/internal/roleprovider"
	"go.uber.org/zap"
)

// prodLikeSource is the model list as production has it: chat-default on Macs with Ollama, chat-large on two GB10 slots (Strata, gpu-…) and Strata Macs as overflow, agent on a GPU node with vLLM, and the GB10's model also under its own name.
type prodLikeSource struct{}

func prodDep(public, model, site string, fleet, overflow bool, in, out float64) litellm.Deployment {
	var d litellm.Deployment
	d.ModelName, d.Params.Model, d.Params.APIBase = public, "openai/"+model, "http://10.90.4.7:8000/v1"
	d.Info.ID, d.Info.Site, d.Info.Fleet, d.Info.Overflow = site+"::"+public, site, fleet, overflow
	d.Info.MaxInputTokens, d.Info.MaxOutputTokens = in, out
	return d
}

func (prodLikeSource) ModelInfo(context.Context) ([]litellm.Deployment, error) {
	return []litellm.Deployment{
		prodDep("chat-default", "qwen3.6:35b-a3b-coding-nvfp4", "dmmac21", true, false, 24576, 8192),
		prodDep("chat-default", "qwen3.6:35b-a3b-coding-nvfp4", "dmmac22", true, false, 24576, 8192),
		prodDep("chat-large", "qwen3.8-flash-next-iq3_s", "gpu-gap-dell-gb10-1", false, false, 98304, 32768),
		prodDep("chat-large", "qwen3.8-flash-next-iq3_s", "gpu-gap-dell-gb10-2", false, false, 98304, 32768),
		prodDep("chat-large", "qwen3.8-flash-next-gyro-gyro-s", "dmmac30", true, true, 24576, 8192),
		prodDep("qwen3.8-flash-next-iq3_s", "qwen3.8-flash-next-iq3_s", "gpu-gap-dell-gb10-1", false, false, 98304, 32768),
		prodDep("agent", "Qwen/Qwen3.6-35B-A3B-FP8", "gpu-gap-rtx-1", false, false, 98304, 32768),
		prodDep("chat-fast", "Qwen/Qwen3-8B-AWQ", "lab-server", false, false, 0, 0),
	}, nil
}

func (prodLikeSource) Health(context.Context) ([]litellm.HealthEndpoint, []litellm.HealthEndpoint, error) {
	return []litellm.HealthEndpoint{{ModelID: "dmmac21::chat-default"}, {ModelID: "gpu-gap-dell-gb10-1::chat-large"}, {ModelID: "dmmac30::chat-large"}, {ModelID: "gpu-gap-rtx-1::agent"}},
		[]litellm.HealthEndpoint{{ModelID: "dmmac22::chat-default"}}, nil
}

func usableServer(t *testing.T) (*ModelAliases, http.Handler) {
	t.Helper()
	acc := accessFor(t, &roleprovider.Mock{}, nil, nil)
	ws := workspace.NewService(workspace.NewMemoryStore(), workspace.Config{}, func(context.Context) ([]workspace.Running, error) { return nil, nil })
	for slug, p := range map[string]workspace.TreeProject{
		"p_plain": {Name: "Nur LLM", Members: []workspace.Member{{Token: "user:member@dhbw.de"}}, LLMMTokens: 10},
		"p_large": {Name: "Groß", Members: []workspace.Member{{Token: "user:large@dhbw.de"}}, LLMMTokens: 10, LLMModels: []string{"chat-large", "all-proxy-models"}},
		"p_model": {Name: "Modell", Members: []workspace.Member{{Token: "user:large@dhbw.de"}}, LLMMTokens: 10, LLMModels: []string{"chat-large", "qwen3.8-flash-next-iq3_s", "retired-model"}},
	} {
		if _, err := ws.UpsertFromTree(context.Background(), slug, p); err != nil {
			t.Fatal(err)
		}
	}
	quota := testQuota
	quota.Projects = ws
	// LLM_BASE_MODELS names the base for both, as in production.
	quota.BaseModels = []string{"chat-default", "chat-fast", "agent"}
	ma := NewModelAliases(prodLikeSource{}, quota.BaseModels, zap.NewNop().Sugar())
	ma.SetEngines(func(context.Context) (map[string]string, error) {
		return map[string]string{"qwen3.8-flash-next-iq3_s": "strata", "Qwen/Qwen3.6-35B-A3B-FP8": "vllm"}, nil
	})
	store := modeldesc.NewMemoryStore()
	yes := true
	_ = store.Save(context.Background(), modeldesc.Description{Model: "qwen3.8-flash-next-iq3_s", Summary: "Großes MoE-Modell", Description: "Erster Absatz.\n\nZweiter.", SourceURL: "https://huggingface.co/x/y", License: "Apache-2.0", ToolCalls: &yes, UpdatedBy: "admin@dhbw.de"})
	_ = store.Save(context.Background(), modeldesc.Description{Model: "retired-model", Description: "Nicht mehr im Betrieb."})
	ma.SetDescriptions(store)
	return ma, New(Options{DevMode: true, DevAuthUser: testDevUser, Access: acc, Log: zap.NewNop().Sugar(), Quota: quota, Workspaces: ws, Aliases: ma}).Router()
}

func usable(t *testing.T, h http.Handler, user string) (UsableModelsResponse, string) {
	t.Helper()
	code, body := do(t, h, "GET", "/v1/models/usable", user, nil)
	if code != http.StatusOK {
		t.Fatalf("%s: %d %s", user, code, body)
	}
	var v UsableModelsResponse
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	return v, string(body)
}

func TestUsableModels(t *testing.T) {
	_, h := usableServer(t)
	if code, _ := do(t, h, "GET", "/v1/models/usable", "nobody@dhbw.de", nil); code != http.StatusForbidden {
		t.Fatalf("without tokens: %d", code)
	}

	// A member without grants: the base names only, through every project.
	v, _ := usable(t, h, "member@dhbw.de")
	var names []string
	for _, n := range v.Names {
		names = append(names, n.Name)
		if !n.Base || n.Kind != KindAlias || len(n.Projects) != 0 {
			t.Errorf("%s: base %v kind %s projects %v", n.Name, n.Base, n.Kind, n.Projects)
		}
	}
	if strings.Join(names, ",") != "chat-default,chat-fast,agent" || !v.HealthKnown {
		t.Fatalf("names %v", names)
	}

	v, body := usable(t, h, "large@dhbw.de")
	// Neither sites, hosts nor addresses, nor who edited a description; all-proxy-models is no name.
	for _, leak := range []string{"dmmac", "gpu-gap", "dell-gb10", "rtx-1", "lab-server", "10.90", "api_base", "openai/", "admin@dhbw.de", "all-proxy-models"} {
		if strings.Contains(body, leak) {
			t.Errorf("%q in %s", leak, body)
		}
	}
	by := map[string]UsableName{}
	names = nil
	for _, n := range v.Names {
		by[n.Name] = n
		names = append(names, n.Name)
	}
	if strings.Join(names, ",") != "chat-default,chat-fast,agent,chat-large,qwen3.8-flash-next-iq3_s,retired-model" {
		t.Fatalf("names %v", names)
	}

	// chat-large: an alias through both projects, two models, smallest and largest limits over the GB10s and the Macs.
	cl := by["chat-large"]
	if cl.Kind != KindAlias || cl.Base || len(cl.Projects) != 2 || cl.Deployments != 3 || cl.Healthy != 2 {
		t.Errorf("chat-large: %+v", cl)
	}
	if cl.Limits.Min != (ModelLimit{24576, 8192}) || cl.Limits.Max != (ModelLimit{98304, 32768}) {
		t.Errorf("chat-large limits: %+v", cl.Limits)
	}
	if len(cl.Models) != 2 || cl.Models[0].Model != "qwen3.8-flash-next-iq3_s" || cl.Models[1].Model != "qwen3.8-flash-next-gyro-gyro-s" {
		t.Fatalf("chat-large models: %+v", cl.Models)
	}
	gb := cl.Models[0]
	if gb.Description == nil || gb.Description.License != "Apache-2.0" || gb.Description.ToolCalls == nil || !*gb.Description.ToolCalls || gb.Description.UpdatedBy != "" {
		t.Errorf("description: %+v", gb.Description)
	}
	if len(gb.Devices) != 1 || gb.Devices[0] != (DeviceConfig{Device: DeviceGB10, Engine: "strata", Limits: LimitRange{Min: ModelLimit{98304, 32768}, Max: ModelLimit{98304, 32768}}, Deployments: 2, Healthy: 1}) {
		t.Errorf("gb10 devices: %+v", gb.Devices)
	}
	if mac := cl.Models[1]; mac.Description != nil || len(mac.Devices) != 1 || mac.Devices[0].Device != DeviceMac || mac.Devices[0].Engine != "strata" {
		t.Errorf("strata macs: %+v", mac)
	}

	// The model under its own name: kind model, only through the project that has it.
	m := by["qwen3.8-flash-next-iq3_s"]
	if m.Kind != KindModel || m.Base || len(m.Projects) != 1 || m.Projects[0].Name != "Modell" || len(m.Models) != 1 || m.Models[0].Description == nil {
		t.Errorf("model name: %+v", m)
	}
	// A granted model nothing serves: listed without models, a model since it has a description.
	if r := by["retired-model"]; r.Kind != KindModel || len(r.Models) != 0 || r.Deployments != 0 {
		t.Errorf("retired: %+v", r)
	}
	// Device classes: Macs with Ollama, a GPU node with vLLM, an inventory site.
	if d := by["chat-default"].Models[0].Devices; len(d) != 1 || d[0].Device != DeviceMac || d[0].Engine != "ollama" || d[0].Deployments != 2 || d[0].Healthy != 1 {
		t.Errorf("chat-default devices: %+v", d)
	}
	if d := by["agent"].Models[0].Devices; len(d) != 1 || d[0].Device != DeviceGPU || d[0].Engine != "vllm" {
		t.Errorf("agent devices: %+v", d)
	}
	if d := by["chat-fast"].Models[0].Devices; len(d) != 1 || d[0].Device != DeviceOther || d[0].Engine != "" || by["chat-fast"].Limits != (LimitRange{}) {
		t.Errorf("chat-fast: %+v", by["chat-fast"])
	}

	// The old view stays as it was.
	code, raw := do(t, h, "GET", "/v1/models/aliases", "large@dhbw.de", nil)
	var old ModelAliasesResponse
	if code != http.StatusOK || json.Unmarshal(raw, &old) != nil || len(old.Aliases) != 6 || old.Aliases[3].Alias != "chat-large" || old.Aliases[3].Limit != (ModelLimit{24576, 8192}) {
		t.Errorf("aliases: %d %s", code, raw)
	}
}

func TestModelDescriptionsAdmin(t *testing.T) {
	_, h := usableServer(t)
	if code, _ := do(t, h, "GET", "/v1/models/descriptions", "member@dhbw.de", nil); code != http.StatusForbidden {
		t.Fatalf("member reads: %d", code)
	}
	if code, _ := do(t, h, "PUT", "/v1/models/descriptions", "member@dhbw.de", modeldesc.Description{Model: "x"}); code != http.StatusForbidden {
		t.Fatalf("member writes: %d", code)
	}

	code, body := do(t, h, "GET", "/v1/models/descriptions", "", nil)
	var v ModelDescriptionsResponse
	if code != http.StatusOK || json.Unmarshal(body, &v) != nil || len(v.Descriptions) != 2 {
		t.Fatalf("list: %d %s", code, body)
	}
	if strings.Join(v.Served, ",") != "Qwen/Qwen3-8B-AWQ,Qwen/Qwen3.6-35B-A3B-FP8,qwen3.6:35b-a3b-coding-nvfp4,qwen3.8-flash-next-gyro-gyro-s,qwen3.8-flash-next-iq3_s" {
		t.Errorf("served: %v", v.Served)
	}

	for _, bad := range []modeldesc.Description{{Model: ""}, {Model: "a b"}, {Model: "m", SourceURL: "javascript:alert(1)"}, {Model: "m", SourceURL: "//huggingface.co"}} {
		if code, body := do(t, h, "PUT", "/v1/models/descriptions", "", bad); code != http.StatusBadRequest {
			t.Errorf("%+v: %d %s", bad, code, body)
		}
	}

	no := false
	code, body = do(t, h, "PUT", "/v1/models/descriptions", "", modeldesc.Description{Model: " Qwen/Qwen3.6-35B-A3B-FP8 ", Description: "**MoE**", SourceURL: "https://huggingface.co/Qwen/Qwen3.6-35B-A3B-FP8", Reasoning: &no})
	v = ModelDescriptionsResponse{}
	if code != http.StatusOK || json.Unmarshal(body, &v) != nil || len(v.Descriptions) != 3 {
		t.Fatalf("put: %d %s", code, body)
	}
	got := modeldesc.ByModel(v.Descriptions)["Qwen/Qwen3.6-35B-A3B-FP8"]
	if got.UpdatedBy != testDevUser || got.Reasoning == nil || *got.Reasoning || got.ToolCalls != nil || got.UpdatedAt.IsZero() {
		t.Errorf("saved: %+v", got)
	}
	// Everybody who can use agent sees it.
	u, _ := usable(t, h, "member@dhbw.de")
	if d := u.Names[2].Models[0].Description; d == nil || d.Description != "**MoE**" {
		t.Errorf("agent description: %+v", d)
	}

	code, body = do(t, h, "DELETE", "/v1/models/descriptions?model="+url.QueryEscape("Qwen/Qwen3.6-35B-A3B-FP8"), "", nil)
	v = ModelDescriptionsResponse{}
	if code != http.StatusOK || json.Unmarshal(body, &v) != nil || len(v.Descriptions) != 2 {
		t.Fatalf("delete: %d %s", code, body)
	}
	if code, _ := do(t, h, "DELETE", "/v1/models/descriptions", "", nil); code != http.StatusBadRequest {
		t.Errorf("delete without model: %d", code)
	}
}
