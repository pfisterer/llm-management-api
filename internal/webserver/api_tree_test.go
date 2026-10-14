package webserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pfisterer/llm-management-api/internal/keys"
	"github.com/pfisterer/llm-management-api/internal/litellm"
	"github.com/pfisterer/llm-management-api/internal/workspace"
	"go.uber.org/zap"
)

func treeServer(t *testing.T) (http.Handler, *workspace.Service) {
	t.Helper()
	ws := workspace.NewService(workspace.NewMemoryStore(), workspace.Config{
		Profiles:   []workspace.Profile{{Slug: "cpu", Name: "CPU"}, {Slug: "gpu", Name: "GPU", GPUClass: "l4-3q"}},
		GPUClasses: []string{"l4-3q"},
	}, func(context.Context) ([]workspace.Running, error) { return nil, nil })
	return New(Options{DevMode: true, DevAuthUser: testDevUser, Workspaces: ws, TreeToken: "tree-secret", Log: zap.NewNop().Sugar()}).Router(), ws
}

func treeDo(h http.Handler, method, path, token, body string) (int, string) {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// Only the tree's token gets through, also in development mode; with it the tree creates, lists and deletes its projects.
func TestTreeAPI(t *testing.T) {
	h, ws := treeServer(t)
	body := `{"name": "Kurs", "members": [{"token": "user:o@dhbw.de", "role": "manager"}], "budgets": [{"unit": "gpu_hours", "gpu_class": "l4-3q", "window": "30d", "amount": 500}], "gpus_by_class": {"l4-3q": 2}, "llm_mtokens": 80}`
	for _, tok := range []string{"", "wrong"} {
		if code, _ := treeDo(h, "PUT", "/v1/tree/projects/p_1", tok, body); code != http.StatusUnauthorized {
			t.Errorf("token %q: %d, want 401", tok, code)
		}
	}
	code, out := treeDo(h, "PUT", "/v1/tree/projects/p_1", "tree-secret", body)
	if code != http.StatusOK || !strings.Contains(out, `"slug":"t-1"`) {
		t.Fatalf("put: %d %s", code, out)
	}
	if code, out := treeDo(h, "PUT", "/v1/tree/projects/p_2", "tree-secret", `{"name": "x", "gpus_by_class": {"h100": 1}}`); code != http.StatusBadRequest {
		t.Errorf("unknown class: %d %s", code, out)
	}
	code, out = treeDo(h, "GET", "/v1/tree/projects", "tree-secret", "")
	var list []workspace.TreeStatus
	if code != http.StatusOK || json.Unmarshal([]byte(out), &list) != nil || len(list) != 1 || list[0].Node != "p_1" {
		t.Errorf("list: %d %s", code, out)
	}
	if p, _ := ws.Get(context.Background(), "t-1"); p.LLMMTokens != 80 || p.GPUsByClass["l4-3q"] != 2 {
		t.Errorf("project = %+v", p)
	}
	if code, _ := treeDo(h, "DELETE", "/v1/tree/projects/p_1", "tree-secret", ""); code != http.StatusNoContent {
		t.Errorf("delete: %d", code)
	}
	if code, _ := treeDo(h, "DELETE", "/v1/tree/projects/p_1", "tree-secret", ""); code != http.StatusNotFound {
		t.Errorf("second delete: %d", code)
	}
}

// The pools are the person's projects with tokens, with the project's rate limits or the defaults, and the base models plus the project's own; without tokens there is no LLM.
func TestLLMQuota(t *testing.T) {
	ctx := context.Background()
	_, ws := treeServer(t)
	for node, tp := range map[string]workspace.TreeProject{
		"p_1": {Name: "A", Members: []workspace.Member{{Token: "group:k"}}, LLMMTokens: 80, LLMRPM: 120, LLMModels: []string{"code", "chat-fast"}},
		"p_2": {Name: "B", Members: []workspace.Member{{Token: "user:a@dhbw.de"}}, LLMMTokens: 20, LLMTPM: 5000},
	} {
		if _, err := ws.UpsertFromTree(ctx, node, tp); err != nil {
			t.Fatal(err)
		}
	}
	q := LLMQuota{DefaultRPM: 60, DefaultTPM: 200000, BaseModels: []string{"chat-fast", "chat-default"}, Projects: ws, SelfServiceURL: "https://self-service"}
	g, status, _ := q.GrantFor(ctx, []string{"user:a@dhbw.de", "group:k"})
	want := keys.Grant{Pools: []keys.Pool{
		{Slug: "t-1", Name: "A", MTokens: 80, RPM: 120, TPM: 200000, Models: []string{"chat-default", "chat-fast", "code"}},
		{Slug: "t-2", Name: "B", MTokens: 20, RPM: 60, TPM: 5000, Models: []string{"chat-default", "chat-fast"}},
	}}
	if status != 0 || !reflect.DeepEqual(g, want) {
		t.Errorf("grant = %+v, status %d; want %+v", g, status, want)
	}
	if _, status, msg := q.GrantFor(ctx, []string{"user:b@dhbw.de"}); status != http.StatusForbidden || !strings.Contains(msg, "Kein Projekt mit LLM-Tokens") || !strings.Contains(msg, "https://self-service") {
		t.Errorf("without tokens: %d %q", status, msg)
	}
	q.Projects = nil
	if _, status, _ := q.GrantFor(ctx, []string{"group:k"}); status != http.StatusServiceUnavailable {
		t.Errorf("without projects: %d", status)
	}
}

// The catalog names the LLM base models and defaults besides GPU classes and profiles; a project's LLM limits and models arrive with the project.
func TestTreeCatalogAndLLMLimits(t *testing.T) {
	ws := workspace.NewService(workspace.NewMemoryStore(), workspace.Config{Profiles: []workspace.Profile{{Slug: "cpu", Name: "CPU"}}},
		func(context.Context) ([]workspace.Running, error) { return nil, nil })
	h := New(Options{DevMode: true, DevAuthUser: testDevUser, Workspaces: ws, TreeToken: "tree-secret", Log: zap.NewNop().Sugar(),
		Quota: LLMQuota{DefaultRPM: 60, DefaultTPM: 200000, BaseModels: []string{"chat-default", "chat-fast", "embedding"}}}).Router()
	code, out := treeDo(h, "GET", "/v1/tree/catalog", "tree-secret", "")
	var cat TreeCatalog
	if code != http.StatusOK || json.Unmarshal([]byte(out), &cat) != nil || !slices.Equal(cat.LLMBaseModels, []string{"chat-default", "chat-fast", "embedding"}) || cat.LLMDefaultRPM != 60 || cat.LLMDefaultTPM != 200000 || len(cat.Profiles) != 1 {
		t.Fatalf("catalog: %d %s", code, out)
	}
	if code, out := treeDo(h, "PUT", "/v1/tree/projects/p_1", "tree-secret", `{"name": "Kurs", "llm_mtokens": 80, "llm_rpm": 120, "llm_tpm": 400000, "llm_models": ["code", "all-proxy-models"]}`); code != http.StatusOK {
		t.Fatalf("put: %d %s", code, out)
	}
	if p, _ := ws.Get(context.Background(), "t-1"); p.LLMRPM != 120 || p.LLMTPM != 400000 || !slices.Equal(p.LLMModels, []string{"all-proxy-models", "code"}) {
		t.Errorf("project = %+v", p)
	}
	if code, out := treeDo(h, "PUT", "/v1/tree/projects/p_2", "tree-secret", `{"name": "x", "llm_rpm": -1}`); code != http.StatusBadRequest {
		t.Errorf("negative rpm: %d %s", code, out)
	}
}

// The tree status reports a project's LLM pool with what its team spent.
func TestTreeStatusLLM(t *testing.T) {
	h, ws := treeServer(t)
	f, srv := newFakeLite(t)
	ws.SetLLMUsed(keys.NewService(litellm.New(srv.URL, "master", 5*time.Second), 5, keys.KeyBudget{Share: 0.05, Window: "4h"}).UsedMTokens)
	code, out := treeDo(h, "PUT", "/v1/tree/projects/p_1", "tree-secret", `{"name": "Kurs", "llm_mtokens": 80}`)
	if code != http.StatusOK || !strings.Contains(out, `{"unit":"llm_mtokens","window":"30d","amount":80,"used":0}`) {
		t.Fatalf("before the team exists: %d %s", code, out)
	}
	f.teams["t-1"] = map[string]any{"team_id": "t-1", "spend": 1.5}
	code, out = treeDo(h, "GET", "/v1/tree/projects", "tree-secret", "")
	if code != http.StatusOK || !strings.Contains(out, `{"unit":"llm_mtokens","window":"30d","amount":80,"used":15}`) {
		t.Fatalf("with spend: %d %s", code, out)
	}
}
