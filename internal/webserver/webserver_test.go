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
// testDevUser is who the development auth signs in; an admin, so the tests reach everybody else by acting on their behalf (X-Act-As), as an admin does in production.
const testDevUser = "admin@dhbw.de"

func accessFor(t *testing.T, roles roleprovider.Provider, adminTokens, fleetAdminTokens []string) *access.Service {
	adminTokens = append([]string{"user:" + testDevUser}, adminTokens...)
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
	return New(Options{DevMode: devMode, DevAuthUser: testDevUser, Access: svc, Log: zap.NewNop().Sugar(), ChatURL: "https://chat", AdminUIURL: "https://admin/autologin",
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
		req.Header.Set("X-Act-As", user)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func TestUnauthenticated(t *testing.T) {
	// Outside development mode there is no dev user: no bearer, no identity — X-Act-As or not.
	prod := newTestServer(t, false)
	if code, _ := do(t, prod, "GET", "/v1/me", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("no bearer in production: want 401, got %d", code)
	}
	if code, _ := do(t, prod, "GET", "/v1/me", "student@dhbw.de", nil); code != http.StatusUnauthorized {
		t.Fatalf("X-Act-As without a bearer in production: want 401, got %d", code)
	}
}

// Acting on behalf of somebody: only an admin may, it is faithful (the person's role, not the admin's), and /me says so.
func TestActAs(t *testing.T) {
	h := newTestServer(t, true)
	var me MeResponse
	code, body := do(t, h, "GET", "/v1/me", "student@dhbw.de", nil)
	if _ = json.Unmarshal(body, &me); code != 200 || me.Email != "student@dhbw.de" || me.Role != access.RoleUser || !me.MayActAs || me.ActingAs != "student@dhbw.de" {
		t.Fatalf("admin acting for the student: %d %s", code, body)
	}
	me = MeResponse{}
	code, body = do(t, h, "GET", "/v1/me", "", nil)
	if _ = json.Unmarshal(body, &me); code != 200 || me.Email != testDevUser || me.ActingAs != "" || !me.MayActAs {
		t.Fatalf("admin as themself: %d %s", code, body)
	}
	if code, _ := do(t, h, "GET", "/v1/me", "group:it", nil); code != http.StatusBadRequest {
		t.Fatalf("malformed X-Act-As: want 400, got %d", code)
	}

	// A non-admin signed in may not act for anybody.
	roles := &roleprovider.Mock{Memberships: map[string][]string{"student@dhbw.de": {"group:wwi23seb"}}}
	notAdmin := New(Options{DevMode: true, DevAuthUser: "student@dhbw.de", Access: accessFor(t, roles, nil, nil), Log: zap.NewNop().Sugar(), Quota: testQuota}).Router()
	if code, _ := do(t, notAdmin, "GET", "/v1/me", "admin@dhbw.de", nil); code != http.StatusForbidden {
		t.Fatalf("a student acting for somebody: want 403, got %d", code)
	}
	if code, _ := do(t, notAdmin, "GET", "/v1/me", "", nil); code != http.StatusOK {
		t.Fatalf("a student as themself: want 200, got %d", code)
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
		Profiles: []workspace.Profile{{Slug: "cpu", Name: "CPU"}, {Slug: "gpu", Name: "GPU", GPU: true}},
	}, func(context.Context) ([]workspace.Running, error) { return nil, nil })
	if _, err := ws.UpsertFromTree(context.Background(), "p_1", workspace.TreeProject{Name: "Kurs", Members: []workspace.Member{{Token: "user:member@dhbw.de"}},
		GPUsByClass: map[string]int{"l4-3q": 2}, LLMMTokens: 80, Workspaces: true}); err != nil {
		t.Fatal(err)
	}
	// LLM tokens only: no workspace project, so it adds nothing to workspace_projects and is not offered to the hub.
	if _, err := ws.UpsertFromTree(context.Background(), "p_llm", workspace.TreeProject{Name: "Nur LLM", Members: []workspace.Member{{Token: "user:member@dhbw.de"}}, LLMMTokens: 10}); err != nil {
		t.Fatal(err)
	}
	g, err := gpu.NewForTest(gpu.Config{JupyterURL: "https://jupyter"})
	if err != nil {
		t.Fatal(err)
	}
	quota := testQuota
	quota.Projects, quota.SelfServiceURL = ws, "https://self-service"
	h := New(Options{DevMode: true, DevAuthUser: testDevUser, Access: acc, Log: zap.NewNop().Sugar(), ChatURL: "https://chat",
		Keys: testKeys(fakeBackend(t).URL), Quota: quota, GPU: g, Workspaces: ws}).Router()
	return acc, ws, h
}

// The tokens of a project are the LLM (summed over the projects, the LLM-only one included); without them there is none, also for an admin. Only the project with workspace resources counts as a workspace project.
func TestLLMFromProjects(t *testing.T) {
	_, _, h := llmServer(t)
	var me MeResponse
	code, body := do(t, h, "GET", "/v1/me", "member@dhbw.de", nil)
	_ = json.Unmarshal(body, &me)
	if code != 200 || me.Role != access.RoleUser || me.LLMMTokens != 90 || me.ChatURL != "https://chat" || me.WorkspaceProjects != 1 || me.JupyterURL == "" {
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
		if code, body := do(t, h, "GET", "/v1/gpu/servers", user, nil); code != want {
			t.Errorf("%s: %d %s, want %d", user, code, body, want)
		}
	}
}

// Starting from the self-service UI: only the caller's projects, profiles and GPU classes; the hub checks the rest.
func TestStartServer(t *testing.T) {
	_, _, h := llmServer(t)
	code, body := do(t, h, "GET", "/v1/gpu/start-options", "member@dhbw.de", nil)
	if code != 200 || !strings.Contains(string(body), `"slug":"t-1"`) || strings.Contains(string(body), "Nur LLM") || !strings.Contains(string(body), `"profiles":[{"slug":"cpu"`) {
		t.Fatalf("start options: %d %s", code, body)
	}
	for _, tc := range []struct {
		req  StartServerRequest
		want int
	}{
		{StartServerRequest{Project: "t-2", Profile: "cpu"}, 400},
		{StartServerRequest{Project: "t-1", Profile: "nope"}, 400},
		{StartServerRequest{Project: "t-1", Profile: "gpu", GPUClass: "other"}, 400},
		{StartServerRequest{Project: "t-1", Profile: "gpu"}, 202},
	} {
		if code, body := do(t, h, "POST", "/v1/gpu/servers", "member@dhbw.de", tc.req); code != tc.want {
			t.Errorf("%+v: %d %s, want %d", tc.req, code, body, tc.want)
		}
	}
	if code, _ := do(t, h, "POST", "/v1/gpu/servers", "nobody@dhbw.de", StartServerRequest{Project: "t-1", Profile: "cpu"}); code != http.StatusForbidden {
		t.Errorf("nobody: %d", code)
	}
}

// The progress of a start: for members of a workspace project, only well-formed server names (the caller's own servers, the name goes into the hub's path).
func TestServerProgress(t *testing.T) {
	_, _, h := llmServer(t)
	if code, body := do(t, h, "GET", "/v1/gpu/servers/t-1-gpu/progress", "member@dhbw.de", nil); code != 200 || !strings.Contains(string(body), `"events":[]`) {
		t.Fatalf("progress: %d %s", code, body)
	}
	for _, name := range []string{"..", "a%2Fb", "a%20b"} {
		if code, _ := do(t, h, "GET", "/v1/gpu/servers/"+name+"/progress", "member@dhbw.de", nil); code != 400 && code != 404 {
			t.Errorf("%q: %d, want 400", name, code)
		}
	}
	if code, _ := do(t, h, "GET", "/v1/gpu/servers/t-1-gpu/progress", "nobody@dhbw.de", nil); code != http.StatusForbidden {
		t.Errorf("nobody: %d", code)
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
		// The hub sets the servers' release times from the project (docs/todo.md idle_release): per profile and for GPU kernels, in seconds.
		if tc.user == "member@dhbw.de" && tc.status == 200 && (!strings.Contains(w.Body.String(), `"release":{"servers":{`) || !strings.Contains(w.Body.String(), `"kernel":{"idle_open":300,"idle_closed":300,"max":14400}`)) {
			t.Errorf("release in the access answer: %s", w.Body.String())
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
	h := New(Options{DevMode: true, DevAuthUser: testDevUser, Access: acc, Log: zap.NewNop().Sugar(), Keys: testKeys(srv.URL), Quota: quota, Workspaces: ws}).Router()

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

// Acting on somebody's behalf, keys go to that person's LiteLLM user, found by the e-mail (an admin does not know the person's Keycloak sub).
func TestKeysOnBehalfFindTheSubject(t *testing.T) {
	ctx := context.Background()
	ws := workspace.NewService(workspace.NewMemoryStore(), workspace.Config{}, func(context.Context) ([]workspace.Running, error) { return nil, nil })
	if _, err := ws.UpsertFromTree(ctx, "p_1", workspace.TreeProject{Name: "Kurs A", Members: []workspace.Member{{Token: "user:member@dhbw.de"}}, LLMMTokens: 80}); err != nil {
		t.Fatal(err)
	}
	f, srv := newFakeLite(t)
	f.users["kc-sub-of-member"] = map[string]any{"user_id": "kc-sub-of-member", "user_email": "Member@dhbw.de", "metadata": map[string]any{}}
	quota := testQuota
	quota.Projects = ws
	h := New(Options{DevMode: true, DevAuthUser: testDevUser, Access: accessFor(t, &roleprovider.Mock{}, nil, nil), Log: zap.NewNop().Sugar(), Keys: testKeys(srv.URL), Quota: quota, Workspaces: ws}).Router()

	if code, body := do(t, h, "POST", "/v1/keys", "member@dhbw.de", CreateKeyRequest{Name: "ticket"}); code != http.StatusCreated {
		t.Fatalf("create on behalf: %d %s", code, body)
	}
	for _, k := range f.keys {
		if k["user_id"] != "kc-sub-of-member" {
			t.Errorf("the key should belong to the member's LiteLLM user, got %v", k["user_id"])
		}
	}
}

// Regression: the hub's workspace key went to anyone for any project (the key then drew from the person's default LLM project). Now only members get one, and only from the project's own LLM tokens.
func TestGPUHubWorkspaceKey(t *testing.T) {
	ctx := context.Background()
	acc := accessFor(t, &roleprovider.Mock{}, nil, nil)
	ws := workspace.NewService(workspace.NewMemoryStore(), workspace.Config{}, func(context.Context) ([]workspace.Running, error) { return nil, nil })
	for node, tp := range map[string]workspace.TreeProject{
		"p_1":     {Name: "Kurs", Members: []workspace.Member{{Token: "user:member@dhbw.de"}}, LLMMTokens: 80, Workspaces: true},
		"p_ws":    {Name: "Nur Workspaces", Members: []workspace.Member{{Token: "user:member@dhbw.de"}}, Workspaces: true},
		"p_other": {Name: "Fremd", Members: []workspace.Member{{Token: "user:other@dhbw.de"}}, LLMMTokens: 50, Workspaces: true},
	} {
		if _, err := ws.UpsertFromTree(ctx, node, tp); err != nil {
			t.Fatal(err)
		}
	}
	f, srv := newFakeLite(t)
	quota := testQuota
	quota.Projects = ws
	r := GPUHubRouter(GPUHubOptions{Access: acc, Workspaces: ws, Token: "secret", Keys: testKeys(srv.URL), Quota: quota, MasterKey: "master", Log: zap.NewNop().Sugar()}, true)
	post := func(project string) (int, WorkspaceKeyResponse) {
		b, _ := json.Marshal(WorkspaceKeyRequest{User: "member@dhbw.de", Subject: "sub-m", Project: project})
		req := httptest.NewRequest(http.MethodPost, "/gpu/hub/workspace-key", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer secret")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out WorkspaceKeyResponse
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	if code, out := post("t-1"); code != 200 || out.Key == "" || f.keyBy(out.Key)["team_id"] != "t-1" {
		t.Fatalf("member, own tokens: %d %+v", code, out)
	}
	for _, project := range []string{"t-other", "t-nope"} {
		if code, out := post(project); code != http.StatusForbidden || out.Key != "" {
			t.Errorf("%s: %d %+v, want 403 without a key", project, code, out)
		}
	}
	if code, out := post("t-ws"); code != 200 || out.Key != "" || out.Reason == "" {
		t.Errorf("project without tokens: %d %+v, want no key", code, out)
	}
	if k := f.keyBy(keys.WorkspaceKey("master", "kc-sub-m", "t-ws")); k != nil {
		t.Errorf("key drawn from another pool: %v", k)
	}
	// Projects not readable is an error, not "no key": the hub removes the VS Code provider only on a 200 with a reason.
	broken := quota
	broken.Projects = nil
	r = GPUHubRouter(GPUHubOptions{Access: acc, Workspaces: ws, Token: "secret", Keys: testKeys(srv.URL), Quota: broken, MasterKey: "master", Log: zap.NewNop().Sugar()}, true)
	if code, out := post("t-1"); code != http.StatusServiceUnavailable || out.Key != "" || out.Reason != "" {
		t.Errorf("projects not readable: %d %+v, want 503 without a reason", code, out)
	}
}
