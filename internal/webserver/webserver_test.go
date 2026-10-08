package webserver

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pfisterer/llm-management-api/internal/access"
	"github.com/pfisterer/llm-management-api/internal/roleprovider"
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

	code, body = do(t, h, "GET", "/v1/me", "student@dhbw.de", nil)
	_ = json.Unmarshal(body, &me)
	if code != 200 || me.Role != access.RoleUser || me.Tier != "student" || me.ChatURL == "" {
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
