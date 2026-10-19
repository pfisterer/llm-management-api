package webserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pfisterer/llm-management-api/internal/litellm"
	"github.com/pfisterer/llm-management-api/internal/roleprovider"
	"github.com/pfisterer/llm-management-api/internal/workspace"
	"go.uber.org/zap"
)

// fakeModelList is LiteLLM's /model/info and /health as the discovery leaves them: per machine a passthrough deployment and one per alias, with ids "<site>::<public name>".
func fakeModelList(t *testing.T, calls *atomic.Int32, healthDown *atomic.Bool) string {
	dep := func(site, public, model, base string) map[string]any {
		return map[string]any{"model_name": public, "litellm_params": map[string]any{"model": "openai/" + model, "api_base": base, "api_key": "sk-***"},
			"model_info": map[string]any{"id": site + "::" + public, "site": site}}
	}
	var data []any
	for _, site := range []string{"mac1", "mac2", "mac3"} {
		base := "http://10.90.4.1:11434/v1"
		data = append(data, dep(site, "qwen3.6:35b-a3b-coding-nvfp4", "qwen3.6:35b-a3b-coding-nvfp4", base), dep(site, "chat-default", "qwen3.6:35b-a3b-coding-nvfp4", base), dep(site, "code", "qwen3.6:35b-a3b-coding-nvfp4", base))
	}
	for _, site := range []string{"gap-a", "gap-b"} {
		data = append(data, dep(site, "chat-fast", "Qwen/Qwen3.6-35B-A3B-FP8", "https://inference/"+site+"/v1"))
	}
	data = append(data, dep("gap-c", "code", "Qwen/Qwen3.5-4B-AWQ", "https://inference/gap-c/v1"), dep("gap-d", "Qwen/Qwen2.5-0.5B-Instruct", "Qwen/Qwen2.5-0.5B-Instruct", "https://inference/gap-d/v1"))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/model/info":
			calls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
		case "/health":
			if healthDown.Load() {
				w.WriteHeader(500)
				return
			}
			ep := func(site, public, model, base string) map[string]any {
				return map[string]any{"api_base": base, "model": "openai/" + model, "model_id": site + "::" + public}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"healthy_endpoints": []any{ep("mac1", "chat-default", "qwen3.6:35b-a3b-coding-nvfp4", "http://10.90.4.1:11434/v1"), ep("mac2", "chat-default", "qwen3.6:35b-a3b-coding-nvfp4", "http://10.90.4.1:11434/v1"),
					ep("gap-a", "chat-fast", "Qwen/Qwen3.6-35B-A3B-FP8", "https://inference/gap-a/v1"),
					// Without an id (older LiteLLM): matched by address and model.
					map[string]any{"api_base": "https://inference/gap-c/v1", "model": "openai/Qwen/Qwen3.5-4B-AWQ"}},
				"unhealthy_endpoints": []any{ep("mac3", "chat-default", "qwen3.6:35b-a3b-coding-nvfp4", "http://10.90.4.1:11434/v1"), ep("gap-b", "chat-fast", "Qwen/Qwen3.6-35B-A3B-FP8", "https://inference/gap-b/v1")},
			})
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func aliasesServer(t *testing.T, src AliasSource) (*ModelAliases, http.Handler) {
	t.Helper()
	acc := accessFor(t, &roleprovider.Mock{}, nil, nil)
	ws := workspace.NewService(workspace.NewMemoryStore(), workspace.Config{}, func(context.Context) ([]workspace.Running, error) { return nil, nil })
	if _, err := ws.UpsertFromTree(context.Background(), "p_llm", workspace.TreeProject{Name: "Nur LLM", Members: []workspace.Member{{Token: "user:member@dhbw.de"}}, LLMMTokens: 10}); err != nil {
		t.Fatal(err)
	}
	quota := testQuota
	quota.Projects = ws
	ma := NewModelAliases(src, []string{"chat-default", "chat-fast", "code", "embedding"}, zap.NewNop().Sugar())
	return ma, New(Options{DevMode: true, DevAuthUser: testDevUser, Access: acc, Log: zap.NewNop().Sugar(), Quota: quota, Workspaces: ws, Aliases: ma}).Router()
}

func TestModelAliases(t *testing.T) {
	var calls atomic.Int32
	var healthDown atomic.Bool
	ma, h := aliasesServer(t, litellm.New(fakeModelList(t, &calls, &healthDown), "master", 5*time.Second))
	clock := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	ma.now = func() time.Time { return clock }

	// Somebody with LLM tokens and an admin may read it, somebody without tokens may not.
	if code, body := do(t, h, "GET", "/v1/models/aliases", "nobody@dhbw.de", nil); code != http.StatusForbidden || !strings.Contains(string(body), "no_quota") {
		t.Fatalf("without tokens: %d %s", code, body)
	}
	if code, body := do(t, h, "GET", "/v1/models/aliases", "", nil); code != http.StatusOK {
		t.Fatalf("admin: %d %s", code, body)
	}
	code, body := do(t, h, "GET", "/v1/models/aliases", "member@dhbw.de", nil)
	if code != http.StatusOK {
		t.Fatalf("member: %d %s", code, body)
	}
	// Neither addresses nor sites leave the service.
	for _, leak := range []string{"10.90", "inference/", "mac1", "gap-", "api_base", "openai/"} {
		if strings.Contains(string(body), leak) {
			t.Errorf("%q in %s", leak, body)
		}
	}
	var v ModelAliasesResponse
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	got := map[string][]AliasModel{}
	var order []string
	for _, a := range v.Aliases {
		got[a.Alias] = a.Models
		order = append(order, a.Alias)
	}
	if strings.Join(order, ",") != "chat-default,chat-fast,code,embedding" || !v.HealthKnown {
		t.Fatalf("aliases %v, health %v", order, v.HealthKnown)
	}
	want := map[string]string{
		"chat-default": "qwen3.6:35b-a3b-coding-nvfp4 3/2",
		"chat-fast":    "Qwen/Qwen3.6-35B-A3B-FP8 2/1",
		// Most deployments first; the 4B model is healthy through the address match.
		"code":      "qwen3.6:35b-a3b-coding-nvfp4 3/0, Qwen/Qwen3.5-4B-AWQ 1/1",
		"embedding": "",
	}
	for alias, w := range want {
		var parts []string
		for _, m := range got[alias] {
			parts = append(parts, m.Model+" "+strconv.Itoa(m.Deployments)+"/"+strconv.Itoa(m.Healthy))
		}
		if s := strings.Join(parts, ", "); s != w {
			t.Errorf("%s: %q, want %q", alias, s, w)
		}
		if got[alias] == nil {
			t.Errorf("%s: models must be a list, not null", alias)
		}
	}

	// Cached for a minute.
	n := calls.Load()
	do(t, h, "GET", "/v1/models/aliases", "member@dhbw.de", nil)
	if calls.Load() != n {
		t.Errorf("model list read again within the minute")
	}
	clock = clock.Add(61 * time.Second)
	healthDown.Store(true)
	_, body = do(t, h, "GET", "/v1/models/aliases", "member@dhbw.de", nil)
	v = ModelAliasesResponse{}
	_ = json.Unmarshal(body, &v)
	if calls.Load() != n+1 || v.HealthKnown || v.Aliases[0].Models[0].Healthy != 0 || v.Aliases[0].Models[0].Deployments != 3 {
		t.Errorf("after a minute, health down: calls %d, %s", calls.Load()-n, body)
	}
}

// LiteLLM down: an error at first, the previous view once there is one.
func TestModelAliasesUpstreamDown(t *testing.T) {
	src := &flakySource{}
	ma, h := aliasesServer(t, src)
	clock := time.Now()
	ma.now = func() time.Time { return clock }
	src.fail = true
	if code, _ := do(t, h, "GET", "/v1/models/aliases", "member@dhbw.de", nil); code != http.StatusBadGateway {
		t.Fatalf("no view yet: %d", code)
	}
	src.fail = false
	if code, _ := do(t, h, "GET", "/v1/models/aliases", "member@dhbw.de", nil); code != http.StatusOK {
		t.Fatalf("up: %d", code)
	}
	src.fail = true
	clock = clock.Add(2 * time.Minute)
	if code, body := do(t, h, "GET", "/v1/models/aliases", "member@dhbw.de", nil); code != http.StatusOK || !strings.Contains(string(body), "m1") {
		t.Fatalf("previous view: %d %s", code, body)
	}
}

type flakySource struct{ fail bool }

func (f *flakySource) ModelInfo(context.Context) ([]litellm.Deployment, error) {
	if f.fail {
		return nil, &litellm.Error{Status: 500}
	}
	var d litellm.Deployment
	d.ModelName, d.Params.Model = "chat-fast", "openai/m1"
	return []litellm.Deployment{d}, nil
}

func (f *flakySource) Health(context.Context) ([]litellm.HealthEndpoint, []litellm.HealthEndpoint, error) {
	return nil, nil, nil
}
