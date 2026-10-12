package webserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pfisterer/llm-management-api/internal/access"
	"github.com/pfisterer/llm-management-api/internal/gpu"
	"github.com/pfisterer/llm-management-api/internal/keys"
	"github.com/pfisterer/llm-management-api/internal/litellm"
	"github.com/pfisterer/llm-management-api/internal/roleprovider"
	"github.com/pfisterer/llm-management-api/internal/workspace"
	"go.uber.org/zap"
)

func newTestServer(t *testing.T, devMode bool) http.Handler {
	t.Helper()
	roles := &roleprovider.Mock{Memberships: map[string][]string{
		"student@dhbw.de": {"group:wwi23seb"},
	}}
	svc, err := access.NewService(access.NewMemoryStore(), roles, access.Config{
		Tiers: []string{"student", "staff"}, BootstrapAdmins: []string{"admin@dhbw.de"}, BootstrapAdminTier: "staff",
	})
	if err != nil {
		t.Fatal(err)
	}
	return New(Options{DevMode: devMode, Access: svc, RoleProvider: roles, Log: zap.NewNop().Sugar(), ChatURL: "https://chat"}).Router()
}

func do(t *testing.T, h http.Handler, method, path, user string, body any) (int, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, r)
	if user != "" {
		req.Header.Set("X-Dummy-Auth-User", user)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func TestUnauthenticated(t *testing.T) {
	h := newTestServer(t, true)
	if code, _ := do(t, h, "GET", "/v1/me", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("no identity: want 401, got %d", code)
	}
	// Outside development mode the dummy header must be ignored.
	prod := newTestServer(t, false)
	if code, _ := do(t, prod, "GET", "/v1/me", "admin@dhbw.de", nil); code != http.StatusUnauthorized {
		t.Fatalf("dummy header in production: want 401, got %d", code)
	}
}

func TestMeAndRoles(t *testing.T) {
	h := newTestServer(t, true)

	code, body := do(t, h, "GET", "/v1/me", "nobody@dhbw.de", nil)
	var me MeResponse
	_ = json.Unmarshal(body, &me)
	if code != 200 || me.Role != access.RoleNone || me.ChatURL != "" {
		t.Fatalf("no access: %d %s", code, body)
	}

	// The admin grants the student's course access.
	code, body = do(t, h, "POST", "/v1/access-rules", "admin@dhbw.de",
		RuleRequest{Token: "group:WWI23SEB", Role: access.RoleUser, Tier: "student"})
	if code != http.StatusCreated {
		t.Fatalf("create rule: %d %s", code, body)
	}
	var created access.Rule
	_ = json.Unmarshal(body, &created)

	// The rule gives the role; the LLM needs tokens from a project, which this server has none of.
	code, body = do(t, h, "GET", "/v1/me", "student@dhbw.de", nil)
	_ = json.Unmarshal(body, &me)
	if code != 200 || me.Role != access.RoleUser || me.MatchedToken != "group:wwi23seb" || me.LLMMTokens != 0 || me.Tier != "" || me.ChatURL != "" {
		t.Fatalf("student after rule: %d %s", code, body)
	}

	// A user may not touch the access list.
	if code, _ := do(t, h, "GET", "/v1/access-rules", "student@dhbw.de", nil); code != http.StatusForbidden {
		t.Fatalf("user listing rules: want 403, got %d", code)
	}
	if code, _ := do(t, h, "DELETE", "/v1/access-rules/1", "student@dhbw.de", nil); code != http.StatusForbidden {
		t.Fatalf("user deleting rule: want 403, got %d", code)
	}

	// Validation and conflicts.
	if code, _ := do(t, h, "POST", "/v1/access-rules", "admin@dhbw.de", RuleRequest{Token: "group:x", Role: "boss", Tier: "staff"}); code != http.StatusBadRequest {
		t.Fatalf("unknown role: want 400, got %d", code)
	}
	if code, _ := do(t, h, "POST", "/v1/access-rules", "admin@dhbw.de", RuleRequest{Token: "group:wwi23seb", Role: access.RoleUser, Tier: "staff"}); code != http.StatusConflict {
		t.Fatalf("duplicate: want 409, got %d", code)
	}
	if code, _ := do(t, h, "POST", "/v1/access-rules", "admin@dhbw.de", RuleRequest{Token: "user:admin@dhbw.de", Role: access.RoleUser, Tier: "staff"}); code != http.StatusConflict {
		t.Fatalf("overriding bootstrap admin: want 409, got %d", code)
	}

	// List shows the bootstrap admin first, then the stored rule.
	code, body = do(t, h, "GET", "/v1/access-rules", "admin@dhbw.de", nil)
	var rules []access.Rule
	_ = json.Unmarshal(body, &rules)
	if code != 200 || len(rules) != 2 || !rules[0].Bootstrap || rules[1].Token != "group:wwi23seb" {
		t.Fatalf("list: %d %s", code, body)
	}

	// Removing the rule removes the access.
	if code, _ := do(t, h, "DELETE", "/v1/access-rules/1", "admin@dhbw.de", nil); code != http.StatusNoContent {
		t.Fatalf("delete: want 204, got %d", code)
	}
	_, body = do(t, h, "GET", "/v1/me", "student@dhbw.de", nil)
	_ = json.Unmarshal(body, &me)
	if me.Role != access.RoleNone {
		t.Fatalf("access should be gone, got %s", body)
	}
}

func TestPublicEndpoints(t *testing.T) {
	h := newTestServer(t, false)
	for _, p := range []string{"/health", "/config.json"} {
		if code, _ := do(t, h, "GET", p, "", nil); code != 200 {
			t.Fatalf("%s: want 200, got %d", p, code)
		}
	}
}

func TestPrincipalSearch(t *testing.T) {
	roles := &roleprovider.Mock{
		Groups: []roleprovider.Group{{Token: "group:wwi23seb", Description: "Kurs WWI23SEB"}, {Token: "group:it", Label: "IT-Service"}},
		Users:  []string{"student@dhbw.de", "dozent@dhbw.de"},
	}
	svc, err := access.NewService(access.NewMemoryStore(), roles, access.Config{
		Tiers: []string{"staff"}, BootstrapAdmins: []string{"admin@dhbw.de"}, BootstrapAdminTier: "staff",
	})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Options{DevMode: true, Access: svc, RoleProvider: roles, Log: zap.NewNop().Sugar()}).Router()

	var got PrincipalSearchResponse
	code, body := do(t, h, "GET", "/v1/principals/search?q=kurs", "admin@dhbw.de", nil)
	_ = json.Unmarshal(body, &got)
	if code != 200 || len(got.Groups) != 1 || got.Groups[0].Description != "Kurs WWI23SEB" || len(got.Users) != 0 {
		t.Fatalf("description match: %d %s", code, body)
	}
	code, body = do(t, h, "GET", "/v1/principals/search?q=student", "admin@dhbw.de", nil)
	_ = json.Unmarshal(body, &got)
	if code != 200 || len(got.Users) != 1 || got.Users[0] != "student@dhbw.de" {
		t.Fatalf("user match: %d %s", code, body)
	}
	// Without a query: groups yes, people never.
	code, body = do(t, h, "GET", "/v1/principals/search", "admin@dhbw.de", nil)
	_ = json.Unmarshal(body, &got)
	if code != 200 || len(got.Groups) != 2 || len(got.Users) != 0 {
		t.Fatalf("empty query: %d %s", code, body)
	}
	if code, _ := do(t, h, "GET", "/v1/principals/search?q=x", "student@dhbw.de", nil); code != 403 {
		t.Fatalf("non-admin: %d", code)
	}
}

// llmServer has a bootstrap admin (root), a project with LLM tokens and a GPU for member@dhbw.de, and nobody@dhbw.de without either; no access rule besides root's.
func llmServer(t *testing.T) (*access.Service, *workspace.Service, http.Handler) {
	t.Helper()
	acc, err := access.NewService(access.NewMemoryStore(), &roleprovider.Mock{}, access.Config{
		Tiers: []string{"student", "staff"}, BootstrapAdmins: []string{"root@dhbw.de"}, BootstrapAdminTier: "staff"})
	if err != nil {
		t.Fatal(err)
	}
	ws := workspace.NewService(workspace.NewMemoryStore(), workspace.Config{
		Profiles:   []workspace.Profile{{Slug: "cpu", Name: "CPU"}, {Slug: "pytorch", Name: "PyTorch", GPUClass: "l4-3q"}},
		GPUClasses: []string{"l4-3q"},
	}, func(context.Context) ([]workspace.Running, error) { return nil, nil })
	if _, err := ws.UpsertFromTree(context.Background(), "p_1", workspace.TreeProject{Name: "Kurs", Members: []workspace.Member{{Token: "user:member@dhbw.de"}},
		GPUsByClass: map[string]int{"l4-3q": 2}, LLMMTokens: 80}); err != nil {
		t.Fatal(err)
	}
	g, err := gpu.NewForTest(gpu.Config{JupyterURL: "https://jupyter"})
	if err != nil {
		t.Fatal(err)
	}
	tiers := map[string]keys.Tier{"student": {Name: "student", KeyBudget: 1}, "staff": {Name: "staff", KeyBudget: 2}}
	h := New(Options{DevMode: true, Access: acc, RoleProvider: &roleprovider.Mock{}, Log: zap.NewNop().Sugar(), ChatURL: "https://chat",
		Keys: keys.NewService(litellm.New(fakeBackend(t).URL, "master", 5*time.Second), 5), Tiers: tiers,
		Quota: LLMQuota{DefaultTier: "student", Projects: ws, SelfServiceURL: "https://self-service"}, GPU: g, Workspaces: ws}).Router()
	return acc, ws, h
}

// No access rule needed for the LLM: the tokens of a project are enough; without them there is none.
func TestLLMWithoutRule(t *testing.T) {
	_, _, h := llmServer(t)
	var me MeResponse
	code, body := do(t, h, "GET", "/v1/me", "member@dhbw.de", nil)
	_ = json.Unmarshal(body, &me)
	if code != 200 || me.Role != access.RoleNone || me.LLMMTokens != 80 || me.Tier != "student" || me.ChatURL != "https://chat" || me.WorkspaceProjects != 1 || me.JupyterURL == "" {
		t.Fatalf("member: %d %s", code, body)
	}
	if code, body := do(t, h, "GET", "/v1/usage", "member@dhbw.de", nil); code != 200 {
		t.Fatalf("usage of the member: %d %s", code, body)
	}
	me = MeResponse{}
	code, body = do(t, h, "GET", "/v1/me", "nobody@dhbw.de", nil)
	_ = json.Unmarshal(body, &me)
	if code != 200 || me.LLMMTokens != 0 || me.Tier != "" || me.ChatURL != "" || me.JupyterURL != "" {
		t.Fatalf("nobody: %d %s", code, body)
	}
	for _, path := range []string{"/v1/usage", "/v1/keys"} {
		if code, body := do(t, h, "GET", path, "nobody@dhbw.de", nil); code != http.StatusForbidden || !strings.Contains(string(body), "no_quota") {
			t.Errorf("%s without tokens: %d %s", path, code, body)
		}
	}
	// An admin's rule gives no LLM either.
	if code, _ := do(t, h, "GET", "/v1/usage", "root@dhbw.de", nil); code != http.StatusForbidden {
		t.Errorf("admin without tokens: %d", code)
	}
}

// Workspaces: members of a project and admins, nobody else.
func TestRequireWorkspace(t *testing.T) {
	_, _, h := llmServer(t)
	for user, want := range map[string]int{"member@dhbw.de": 200, "root@dhbw.de": 200, "nobody@dhbw.de": http.StatusForbidden} {
		if code, body := do(t, h, "GET", "/v1/gpu/environments", user, nil); code != want {
			t.Errorf("%s: %d %s, want %d", user, code, body, want)
		}
	}
}

func TestGPUHubAccess(t *testing.T) {
	acc, ws, _ := llmServer(t)
	r := GPUHubRouter(GPUHubOptions{Access: acc, Workspaces: ws, Token: "secret", SelfServiceURL: "https://self-service", Log: zap.NewNop().Sugar()}, true)
	for _, tc := range []struct {
		token, user string
		status      int
		body        string
	}{
		{"", "root@dhbw.de", 401, ""},
		{"wrong", "root@dhbw.de", 401, ""},
		{"secret", "root@dhbw.de", 200, `"allowed":true,"max_gpus":0,"admin":true,"projects":[]`},
		{"secret", "member@dhbw.de", 200, `"allowed":true,"max_gpus":1,"admin":false,"projects":[{"slug":"t-1"`},
		{"secret", "nobody@dhbw.de", 200, `"allowed":false,"max_gpus":0,"admin":false,"projects":[],"get_project_url":"https://self-service"`},
	} {
		req := httptest.NewRequest(http.MethodGet, "/gpu/hub/access?user="+tc.user, nil)
		if tc.token != "" {
			req.Header.Set("Authorization", "Bearer "+tc.token)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.body) {
			t.Errorf("%s/%s: %d %s", tc.token, tc.user, w.Code, w.Body.String())
		}
	}
}

// Keys go into a project's pool: the named one if the caller may draw from it, the default project otherwise; the default can be set to an eligible project only, and the usage shows the pools.
func TestKeysWithProjects(t *testing.T) {
	ctx := context.Background()
	acc, err := access.NewService(access.NewMemoryStore(), &roleprovider.Mock{}, access.Config{Tiers: []string{"student"}})
	if err != nil {
		t.Fatal(err)
	}
	ws := workspace.NewService(workspace.NewMemoryStore(), workspace.Config{}, func(context.Context) ([]workspace.Running, error) { return nil, nil })
	for node, tp := range map[string]workspace.TreeProject{
		"p_1": {Name: "Kurs A", Members: []workspace.Member{{Token: "user:member@dhbw.de"}}, LLMMTokens: 80},
		"p_2": {Name: "Kurs B", Members: []workspace.Member{{Token: "user:member@dhbw.de"}}, LLMMTokens: 20},
		"p_3": {Name: "Fremd", Members: []workspace.Member{{Token: "user:other@dhbw.de"}}, LLMMTokens: 50},
	} {
		if _, err := ws.UpsertFromTree(ctx, node, tp); err != nil {
			t.Fatal(err)
		}
	}
	f, srv := newFakeLite(t)
	h := New(Options{DevMode: true, Access: acc, RoleProvider: &roleprovider.Mock{}, Log: zap.NewNop().Sugar(),
		Keys: keys.NewService(litellm.New(srv.URL, "master", 5*time.Second), 5), Tiers: map[string]keys.Tier{"student": {Name: "student", KeyBudget: 1}},
		Quota: LLMQuota{DefaultTier: "student", Projects: ws}, Workspaces: ws}).Router()

	var created CreateKeyResponse
	code, body := do(t, h, "POST", "/v1/keys", "member@dhbw.de", CreateKeyRequest{Name: "laptop"})
	if _ = json.Unmarshal(body, &created); code != http.StatusCreated || created.Project != "t-1" || created.ProjectName != "Kurs A" {
		t.Fatalf("default project: %d %s", code, body)
	}
	code, body = do(t, h, "POST", "/v1/keys", "member@dhbw.de", CreateKeyRequest{Name: "vscode", Project: "t-2"})
	if _ = json.Unmarshal(body, &created); code != http.StatusCreated || created.Project != "t-2" {
		t.Fatalf("named project: %d %s", code, body)
	}
	for _, slug := range []string{"t-3", "t-unknown"} {
		if code, body := do(t, h, "POST", "/v1/keys", "member@dhbw.de", CreateKeyRequest{Name: "x" + slug, Project: slug}); code != http.StatusForbidden || !strings.Contains(string(body), "project_not_allowed") {
			t.Errorf("project %s: %d %s", slug, code, body)
		}
	}
	if f.teams["t-1"]["max_budget"] != 8.0 || f.teams["t-2"]["max_budget"] != 2.0 {
		t.Errorf("teams: %v %v", f.teams["t-1"], f.teams["t-2"])
	}

	if code, body := do(t, h, "PUT", "/v1/keys/default-project", "member@dhbw.de", DefaultProjectRequest{Project: "t-3"}); code != http.StatusForbidden {
		t.Errorf("default to a project of others: %d %s", code, body)
	}
	if code, body := do(t, h, "PUT", "/v1/keys/default-project", "member@dhbw.de", map[string]string{}); code != http.StatusBadRequest {
		t.Errorf("default without project: %d %s", code, body)
	}
	var def DefaultProjectResponse
	code, body = do(t, h, "PUT", "/v1/keys/default-project", "member@dhbw.de", DefaultProjectRequest{Project: "t-2"})
	if _ = json.Unmarshal(body, &def); code != http.StatusOK || def.Project != "t-2" || def.ProjectName != "Kurs B" {
		t.Fatalf("default: %d %s", code, body)
	}
	code, body = do(t, h, "POST", "/v1/keys", "member@dhbw.de", CreateKeyRequest{Name: "tablet"})
	if _ = json.Unmarshal(body, &created); code != http.StatusCreated || created.Project != "t-2" {
		t.Fatalf("after the default changed: %d %s", code, body)
	}

	var u keys.Usage
	code, body = do(t, h, "GET", "/v1/usage", "member@dhbw.de", nil)
	if _ = json.Unmarshal(body, &u); code != http.StatusOK || len(u.Projects) != 2 || u.Projects[0].Slug != "t-1" || u.Projects[0].Default || !u.Projects[1].Default || u.Projects[1].Budget != 2 || len(u.Keys) != 3 {
		t.Fatalf("usage: %d %s", code, body)
	}
	if !strings.Contains(string(body), `"project_name":"Kurs B"`) {
		t.Errorf("usage JSON: %s", body)
	}
}
