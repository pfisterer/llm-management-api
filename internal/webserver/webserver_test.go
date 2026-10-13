package webserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pfisterer/llm-management-api/internal/access"
	"github.com/pfisterer/llm-management-api/internal/fleet"
	"github.com/pfisterer/llm-management-api/internal/gpu"
	"github.com/pfisterer/llm-management-api/internal/keys"
	"github.com/pfisterer/llm-management-api/internal/litellm"
	"github.com/pfisterer/llm-management-api/internal/roleprovider"
	"github.com/pfisterer/llm-management-api/internal/workspace"
	"go.uber.org/zap"
)

// accessFor resolves tokens through roles and makes the holders of adminTokens admins and of fleetAdminTokens fleet-admins.
func accessFor(t *testing.T, roles roleprovider.Provider, adminTokens, fleetAdminTokens []string) *access.Service {
	t.Helper()
	r, err := access.NewRoles(adminTokens, fleetAdminTokens)
	if err != nil {
		t.Fatal(err)
	}
	return access.NewService(roles, r, 0)
}

var testQuota = LLMQuota{DefaultRPM: 60, DefaultTPM: 200000, BaseModels: []string{"chat-default", "chat-fast", "embedding"}}

func testKeys(url string) *keys.Service {
	return keys.NewService(litellm.New(url, "master", 5*time.Second), 5, keys.KeyBudget{Share: 0.05, Window: "4h"})
}

// newTestServer has admin@dhbw.de and the group it-leitung as admins, the group it as fleet-admins, and no projects.
func newTestServer(t *testing.T, devMode bool) http.Handler {
	t.Helper()
	roles := &roleprovider.Mock{Memberships: map[string][]string{
		"student@dhbw.de": {"group:wwi23seb"},
		"it@dhbw.de":      {"group:it"},
		"chef@dhbw.de":    {"group:it", "group:it-leitung"},
	}}
	svc := accessFor(t, roles, []string{"user:admin@dhbw.de", "group:it-leitung"}, []string{"group:it"})
	fc, wg, err := fleet.ParseConfig(`{"enabled":true,"enrollToken":"e","adminToken":"a"}`, `{"hubPublicKey":"HUB="}`)
	if err != nil {
		t.Fatal(err)
	}
	return New(Options{DevMode: devMode, Access: svc, Log: zap.NewNop().Sugar(), ChatURL: "https://chat", AdminUIURL: "https://admin/autologin",
		Fleet: fleet.NewService(fc, wg, fleet.NewMemoryStore(), fleet.NewScripts(t.TempDir())), Quota: testQuota}).Router()
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

// The role comes from the configured tokens, through the person's groups too; everybody else is a user, and the access rules are gone.
func TestMeAndRoles(t *testing.T) {
	h := newTestServer(t, true)
	for email, want := range map[string]access.Role{"nobody@dhbw.de": access.RoleUser, "student@dhbw.de": access.RoleUser, "it@dhbw.de": access.RoleFleetAdmin,
		"chef@dhbw.de": access.RoleAdmin, "Admin@dhbw.de": access.RoleAdmin} {
		var me MeResponse
		code, body := do(t, h, "GET", "/v1/me", email, nil)
		if _ = json.Unmarshal(body, &me); code != 200 || me.Role != want || me.LLMMTokens != 0 || me.ChatURL != "" || (me.AdminUIURL != "") != (want == access.RoleAdmin) {
			t.Errorf("%s: %d %s, want role %s", email, code, body, want)
		}
		if strings.Contains(string(body), "tier") || strings.Contains(string(body), "matched_token") {
			t.Errorf("%s: gone fields in %s", email, body)
		}
	}
	// The fleet needs fleet-admin (the readme is missing here, so passing the check answers 404), deleting its package admin.
	for email, want := range map[string]int{"student@dhbw.de": 403, "it@dhbw.de": 404, "chef@dhbw.de": 404} {
		if code, body := do(t, h, "GET", "/v1/fleet/readme", email, nil); code != want {
			t.Errorf("fleet readme for %s: %d %s, want %d", email, code, body, want)
		}
	}
	if code, _ := do(t, h, "DELETE", "/v1/fleet/package", "it@dhbw.de", nil); code != http.StatusForbidden {
		t.Errorf("fleet-admin deleting the package: %d", code)
	}
	for _, path := range []string{"/v1/access-rules", "/v1/principals/search", "/v1/tiers"} {
		if code, _ := do(t, h, "GET", path, "admin@dhbw.de", nil); code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", path, code)
		}
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

// llmServer has an admin (root, through the group root_uni), a project with LLM tokens and a GPU for member@dhbw.de, and nobody@dhbw.de without either.
func llmServer(t *testing.T) (*access.Service, *workspace.Service, http.Handler) {
	t.Helper()
	acc := accessFor(t, &roleprovider.Mock{Memberships: map[string][]string{"root@dhbw.de": {"group:root_uni"}}}, []string{"group:root_uni"}, nil)
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
	quota := testQuota
	quota.Projects, quota.SelfServiceURL = ws, "https://self-service"
	h := New(Options{DevMode: true, Access: acc, Log: zap.NewNop().Sugar(), ChatURL: "https://chat",
		Keys: testKeys(fakeBackend(t).URL), Quota: quota, GPU: g, Workspaces: ws}).Router()
	return acc, ws, h
}

// The tokens of a project are the LLM; without them there is none, also for an admin.
func TestLLMFromProjects(t *testing.T) {
	_, _, h := llmServer(t)
	var me MeResponse
	code, body := do(t, h, "GET", "/v1/me", "member@dhbw.de", nil)
	_ = json.Unmarshal(body, &me)
	if code != 200 || me.Role != access.RoleUser || me.LLMMTokens != 80 || me.ChatURL != "https://chat" || me.WorkspaceProjects != 1 || me.JupyterURL == "" {
		t.Fatalf("member: %d %s", code, body)
	}
	if code, body := do(t, h, "GET", "/v1/usage", "member@dhbw.de", nil); code != 200 {
		t.Fatalf("usage of the member: %d %s", code, body)
	}
	me = MeResponse{}
	code, body = do(t, h, "GET", "/v1/me", "nobody@dhbw.de", nil)
	_ = json.Unmarshal(body, &me)
	if code != 200 || me.LLMMTokens != 0 || me.ChatURL != "" || me.JupyterURL != "" {
		t.Fatalf("nobody: %d %s", code, body)
	}
	for _, path := range []string{"/v1/usage", "/v1/keys"} {
		if code, body := do(t, h, "GET", path, "nobody@dhbw.de", nil); code != http.StatusForbidden || !strings.Contains(string(body), "no_quota") {
			t.Errorf("%s without tokens: %d %s", path, code, body)
		}
	}
	// Being admin gives no LLM either.
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

// Keys go into a project's pool: the named one if the caller may draw from it, the default project otherwise; the default can be set to an eligible project only, and the usage shows the pools with their limits.
func TestKeysWithProjects(t *testing.T) {
	ctx := context.Background()
	acc := accessFor(t, &roleprovider.Mock{}, nil, nil)
	ws := workspace.NewService(workspace.NewMemoryStore(), workspace.Config{}, func(context.Context) ([]workspace.Running, error) { return nil, nil })
	for node, tp := range map[string]workspace.TreeProject{
		"p_1": {Name: "Kurs A", Members: []workspace.Member{{Token: "user:member@dhbw.de"}}, LLMMTokens: 80},
		"p_2": {Name: "Kurs B", Members: []workspace.Member{{Token: "user:member@dhbw.de"}}, LLMMTokens: 20, LLMRPM: 300, LLMModels: []string{"code"}},
		"p_3": {Name: "Fremd", Members: []workspace.Member{{Token: "user:other@dhbw.de"}}, LLMMTokens: 50},
	} {
		if _, err := ws.UpsertFromTree(ctx, node, tp); err != nil {
			t.Fatal(err)
		}
	}
	f, srv := newFakeLite(t)
	quota := testQuota
	quota.Projects = ws
	h := New(Options{DevMode: true, Access: acc, Log: zap.NewNop().Sugar(), Keys: testKeys(srv.URL), Quota: quota, Workspaces: ws}).Router()

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
	// The teams carry the limits: A the defaults, B its own RPM and its extra model.
	if a, b := f.teams["t-1"], f.teams["t-2"]; a["rpm_limit"] != 60.0 || a["tpm_limit"] != 200000.0 || fmt.Sprint(a["models"]) != "[chat-default chat-fast embedding]" ||
		b["rpm_limit"] != 300.0 || fmt.Sprint(b["models"]) != "[chat-default chat-fast code embedding]" {
		t.Errorf("team limits: %v %v", a, b)
	}
	// Each key has only a share of its pool's budget.
	for _, k := range f.keys {
		want := map[any]float64{"t-1": 0.4, "t-2": 0.1}[k["team_id"]]
		if mb, _ := k["max_budget"].(float64); math.Abs(mb-want) > 1e-9 || k["rpm_limit"] != nil || k["models"] != nil {
			t.Errorf("key %v in %v: %v, want budget %v", k["key_alias"], k["team_id"], k, want)
		}
	}
	// The person has a budget of 0 and no limits of their own.
	if u := f.users["kc-dev-member@dhbw.de"]; u["rpm_limit"] != nil || u["tpm_limit"] != nil || u["models"] != nil || u["max_budget"] != 0.0 {
		t.Errorf("user quota: %v", u)
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
	if a, b := u.Projects[0], u.Projects[1]; a.RPM != 60 || a.TPM != 200000 || !slices.Equal(a.Models, []string{"chat-default", "chat-fast", "embedding"}) || math.Abs(a.KeyBudget-0.4) > 1e-9 || a.KeyBudgetDuration != "4h" ||
		b.RPM != 300 || b.TPM != 200000 || !slices.Equal(b.Models, []string{"chat-default", "chat-fast", "code", "embedding"}) {
		t.Errorf("limits in the usage: %+v", u.Projects)
	}
	if strings.Contains(string(body), "tier") {
		t.Errorf("tier in the usage: %s", body)
	}
	var raw map[string]any
	_ = json.Unmarshal(body, &raw)
	for _, gone := range []string{"rpm", "tpm", "models"} {
		if _, ok := raw[gone]; ok {
			t.Errorf("person-level %s still in the usage: %s", gone, body)
		}
	}
	if !strings.Contains(string(body), `"project_name":"Kurs B"`) {
		t.Errorf("usage JSON: %s", body)
	}
}
