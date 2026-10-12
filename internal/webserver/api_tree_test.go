package webserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pfisterer/llm-management-api/internal/access"
	"github.com/pfisterer/llm-management-api/internal/keys"
	"github.com/pfisterer/llm-management-api/internal/litellm"
	"github.com/pfisterer/llm-management-api/internal/workspace"
	"go.uber.org/zap"
)

func treeServer(t *testing.T) (http.Handler, *workspace.Service) {
	t.Helper()
	ws := workspace.NewService(workspace.NewMemoryStore(), workspace.Config{
		Profiles:   []workspace.Profile{{Slug: "cpu", Name: "CPU"}, {Slug: "pytorch", Name: "PyTorch", GPUClass: "l4-3q"}},
		GPUClasses: []string{"l4-3q"},
	}, func(context.Context) ([]workspace.Running, error) { return nil, nil })
	return New(Options{DevMode: true, Workspaces: ws, TreeToken: "tree-secret", Log: zap.NewNop().Sugar()}).Router(), ws
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

// The pools are the person's projects with tokens, with or without an access rule; the rule's tier (else the default tier) gives the rest; without tokens there is no LLM.
func TestLLMQuota(t *testing.T) {
	ctx := context.Background()
	_, ws := treeServer(t)
	if _, err := ws.UpsertFromTree(ctx, "p_1", workspace.TreeProject{Name: "A", Members: []workspace.Member{{Token: "group:k"}}, LLMMTokens: 80}); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.UpsertFromTree(ctx, "p_2", workspace.TreeProject{Name: "B", Members: []workspace.Member{{Token: "user:a@dhbw.de"}}, LLMMTokens: 20}); err != nil {
		t.Fatal(err)
	}
	tiers := map[string]keys.Tier{"student": {Name: "student", RPM: 60}, "staff": {Name: "staff", RPM: 600}}
	q := LLMQuota{Tiers: tiers, DefaultTier: "student", Projects: ws, SelfServiceURL: "https://self-service"}
	mine := []string{"user:a@dhbw.de", "group:k"}
	for _, tc := range []struct {
		name string
		d    access.Decision
		rpm  int
	}{
		{"no rule", access.Decision{}, 60},
		{"rule with a tier", access.Decision{Role: access.RoleUser, Tier: "staff"}, 600},
		{"rule with an unknown tier", access.Decision{Role: access.RoleUser, Tier: "gone"}, 60},
	} {
		g, status, _ := q.GrantFor(ctx, tc.d, mine)
		want := []keys.Pool{{Slug: "t-1", Name: "A", MTokens: 80}, {Slug: "t-2", Name: "B", MTokens: 20}}
		if status != 0 || g.Tier.RPM != tc.rpm || !slices.Equal(g.Pools, want) {
			t.Errorf("%s: grant = %+v, status %d; want pools A and B and RPM %d", tc.name, g, status, tc.rpm)
		}
	}
	for _, d := range []access.Decision{{}, {Role: access.RoleAdmin, Tier: "staff"}} {
		if _, status, msg := q.GrantFor(ctx, d, []string{"user:b@dhbw.de"}); status != http.StatusForbidden || !strings.Contains(msg, "Kein Projekt mit LLM-Tokens") || !strings.Contains(msg, "https://self-service") {
			t.Errorf("%+v without tokens: %d %q", d, status, msg)
		}
	}
	q.Projects = nil
	if _, status, _ := q.GrantFor(ctx, access.Decision{}, mine); status != http.StatusServiceUnavailable {
		t.Errorf("without projects: %d", status)
	}
}

// The tree status reports a project's LLM pool with what its team spent.
func TestTreeStatusLLM(t *testing.T) {
	h, ws := treeServer(t)
	f, srv := newFakeLite(t)
	ws.SetLLMUsed(keys.NewService(litellm.New(srv.URL, "master", 5*time.Second), 5).UsedMTokens)
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
