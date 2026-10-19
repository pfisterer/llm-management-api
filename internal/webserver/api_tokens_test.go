package webserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pfisterer/cloud-self-service-golib/authn"
	"github.com/pfisterer/cloud-self-service-golib/token"
	"github.com/pfisterer/llm-management-api/internal/access"
	"github.com/pfisterer/llm-management-api/internal/fleet"
	"github.com/pfisterer/llm-management-api/internal/roleprovider"
	"github.com/pfisterer/llm-management-api/internal/workspace"
	"go.uber.org/zap"
)

// fakeVerifier signs people in by a bearer of the form "oidc:<email>", standing in for Keycloak's ID tokens.
type fakeVerifier struct{}

func (fakeVerifier) Verify(_ context.Context, raw string) (*authn.Claims, error) {
	email, ok := strings.CutPrefix(raw, "oidc:")
	if !ok {
		return nil, errors.New("not a test id token")
	}
	return &authn.Claims{Email: email, Subject: "sub-" + email}, nil
}

// tokenServer runs in production mode (no dev user) with the fake sign-in, the people of newTestServer and an API token service on memory; it returns the service too, so a test can issue a token with a clock of its own.
func tokenServer(t *testing.T, maxPerUser int) (http.Handler, *token.Service, *roleprovider.Mock) {
	t.Helper()
	roles := &roleprovider.Mock{Memberships: map[string][]string{
		"student@dhbw.de": {"group:wwi23seb"},
		"it@dhbw.de":      {"group:it"},
		"chef@dhbw.de":    {"group:it", "group:it-leitung"},
	}}
	svc := accessFor(t, roles, []string{"group:it-leitung"}, []string{"group:it"})
	fc, wg, err := fleet.ParseConfig(`{"enabled":true,"enrollToken":"e","adminToken":"a"}`, `{"hubPublicKey":"HUB="}`)
	if err != nil {
		t.Fatal(err)
	}
	tokens := token.NewService(APITokenPrefix, token.NewMemoryStore())
	h := New(Options{Verifier: fakeVerifier{}, Access: svc, Log: zap.NewNop().Sugar(), Quota: testQuota,
		Fleet:  fleet.NewService(fc, wg, fleet.NewMemoryStore(), fleet.NewScripts(t.TempDir())),
		Tokens: TokenOptions{Service: tokens, TTL: token.TTLPolicy{Default: 24 * time.Hour, Max: 365 * 24 * time.Hour}, MaxPerUser: maxPerUser}}).Router()
	return h, tokens, roles
}

// call sends a request with the given bearer (a signed-in "oidc:<email>" or an API token) and optional X-Act-As.
func call(t *testing.T, h http.Handler, method, path, bearer, actAs string, body any) (int, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, r)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if actAs != "" {
		req.Header.Set("X-Act-As", actAs)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// issue creates a token as the signed-in person and returns it with its secret.
func issue(t *testing.T, h http.Handler, email string, req CreateAPITokenRequest) APIToken {
	t.Helper()
	code, body := call(t, h, "POST", "/v1/tokens", "oidc:"+email, "", req)
	var out APIToken
	if _ = json.Unmarshal(body, &out); code != http.StatusCreated || out.TokenString == "" {
		t.Fatalf("creating a token for %s: %d %s", email, code, body)
	}
	return out
}

func me(t *testing.T, h http.Handler, bearer string) (int, MeResponse) {
	t.Helper()
	code, body := call(t, h, "GET", "/v1/me", bearer, "", nil)
	var m MeResponse
	_ = json.Unmarshal(body, &m)
	return code, m
}

// A token is shown once, recognisable by its prefix, stored as a hash, and then acts as its owner.
func TestAPITokenLifecycle(t *testing.T) {
	h, _, _ := tokenServer(t, 10)
	tok := issue(t, h, "student@dhbw.de", CreateAPITokenRequest{Description: "nightly report", TTLHours: 720})
	if !strings.HasPrefix(tok.TokenString, APITokenPrefix) || !strings.HasPrefix(tok.Prefix, APITokenPrefix) || tok.User != "student@dhbw.de" || tok.ExpiresAt == nil || tok.LastUsedAt != nil {
		t.Fatalf("created token: %+v", tok)
	}
	if got := time.Until(*tok.ExpiresAt); got < 719*time.Hour || got > 721*time.Hour {
		t.Fatalf("lifetime: want 720h, got %s", got)
	}

	code, m := me(t, h, tok.TokenString)
	if code != http.StatusOK || m.Email != "student@dhbw.de" || m.Role != access.RoleUser || m.MayActAs {
		t.Fatalf("/me with the token: %d %+v", code, m)
	}

	code, body := call(t, h, "GET", "/v1/tokens", "oidc:student@dhbw.de", "", nil)
	var list APITokensResponse
	if _ = json.Unmarshal(body, &list); code != http.StatusOK || len(list.Tokens) != 1 || list.Tokens[0].TokenString != "" || list.Tokens[0].Description != "nightly report" || list.Tokens[0].LastUsedAt == nil {
		t.Fatalf("list after use: %d %s", code, body)
	}
	if bytes.Contains(body, []byte(tok.TokenString)) {
		t.Fatal("the list carries the secret")
	}

	// Revoked: refused from then on.
	if code, body := call(t, h, "DELETE", fmt.Sprintf("/v1/tokens/%d", tok.ID), "oidc:student@dhbw.de", "", nil); code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", code, body)
	}
	if code, _ := me(t, h, tok.TokenString); code != http.StatusUnauthorized {
		t.Fatalf("revoked token: want 401, got %d", code)
	}
}

// Expired, unknown and malformed tokens are all simply invalid.
func TestAPITokenRefused(t *testing.T) {
	h, tokens, _ := tokenServer(t, 10)
	past := tokens.WithClock(func() time.Time { return time.Now().Add(-48 * time.Hour) })
	expired, err := past.Issue(context.Background(), "student@dhbw.de", token.IssueOptions{TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	for name, bearer := range map[string]string{
		"expired":   expired.Secret,
		"unknown":   APITokenPrefix + "00000000000000000000000000000000",
		"prefix":    APITokenPrefix,
		"llm key":   "sk-1234",
		"no bearer": "",
	} {
		if code, _ := me(t, h, bearer); code != http.StatusUnauthorized {
			t.Errorf("%s: want 401, got %d", name, code)
		}
	}
}

// A token holds its owner's rights as they are at the time of the request — an admin's acts as admin, and loses it with the group — and never more.
func TestAPITokenRightsAtRequestTime(t *testing.T) {
	h, _, roles := tokenServer(t, 10)
	chef := issue(t, h, "chef@dhbw.de", CreateAPITokenRequest{})
	student := issue(t, h, "student@dhbw.de", CreateAPITokenRequest{})

	if code, m := me(t, h, chef.TokenString); code != 200 || m.Role != access.RoleAdmin {
		t.Fatalf("admin's token: %d %+v", code, m)
	}
	if code, _ := call(t, h, "GET", "/v1/fleet", chef.TokenString, "", nil); code != http.StatusOK {
		t.Fatalf("admin route with the admin's token: want 200, got %d", code)
	}
	if code, _ := call(t, h, "GET", "/v1/fleet", student.TokenString, "", nil); code != http.StatusForbidden {
		t.Fatalf("admin route with a user's token: want 403, got %d", code)
	}

	// The chef leaves it-leitung: the same token is a fleet-admin now, nothing more.
	roles.Memberships["chef@dhbw.de"] = []string{"group:it"}
	if code, m := me(t, h, chef.TokenString); code != 200 || m.Role != access.RoleFleetAdmin {
		t.Fatalf("token after losing the admin group: %d %+v", code, m)
	}
	roles.Memberships["chef@dhbw.de"] = nil
	if code, _ := call(t, h, "GET", "/v1/fleet", chef.TokenString, "", nil); code != http.StatusForbidden {
		t.Fatalf("fleet route after losing every group: want 403, got %d", code)
	}
}

// Acting on behalf and issuing tokens are for people signed in; an admin acting for somebody can see and revoke their tokens (creating one: TestAPITokenOnBehalf).
func TestAPITokenActAs(t *testing.T) {
	h, _, _ := tokenServer(t, 10)
	chef := issue(t, h, "chef@dhbw.de", CreateAPITokenRequest{})
	student := issue(t, h, "student@dhbw.de", CreateAPITokenRequest{})

	if code, _ := call(t, h, "GET", "/v1/me", chef.TokenString, "student@dhbw.de", nil); code != http.StatusForbidden {
		t.Fatalf("X-Act-As with an admin's token: want 403, got %d", code)
	}
	if code, _ := call(t, h, "POST", "/v1/tokens", chef.TokenString, "", CreateAPITokenRequest{}); code != http.StatusForbidden {
		t.Fatalf("a token issuing a token: want 403, got %d", code)
	}
	if code, _ := call(t, h, "POST", "/v1/tokens", chef.TokenString, "student@dhbw.de", CreateAPITokenRequest{}); code != http.StatusForbidden {
		t.Fatalf("issuing on behalf with an admin's token: want 403, got %d", code)
	}
	code, body := call(t, h, "GET", "/v1/tokens", "oidc:chef@dhbw.de", "student@dhbw.de", nil)
	var list APITokensResponse
	if _ = json.Unmarshal(body, &list); code != 200 || len(list.Tokens) != 1 || list.Tokens[0].ID != student.ID {
		t.Fatalf("listing on behalf: %d %s", code, body)
	}
	if code, _ := call(t, h, "DELETE", fmt.Sprintf("/v1/tokens/%d", student.ID), "oidc:chef@dhbw.de", "student@dhbw.de", nil); code != http.StatusNoContent {
		t.Fatalf("revoking on behalf: want 204, got %d", code)
	}
	if code, _ := me(t, h, student.TokenString); code != http.StatusUnauthorized {
		t.Fatalf("token revoked on behalf: want 401, got %d", code)
	}
}

// Somebody else's token can be neither listed nor revoked; the answer does not say whether it exists.
func TestAPITokenWrongOwner(t *testing.T) {
	h, _, _ := tokenServer(t, 10)
	chef := issue(t, h, "chef@dhbw.de", CreateAPITokenRequest{})
	code, body := call(t, h, "GET", "/v1/tokens", "oidc:student@dhbw.de", "", nil)
	var list APITokensResponse
	if _ = json.Unmarshal(body, &list); code != 200 || len(list.Tokens) != 0 {
		t.Fatalf("the student sees: %d %s", code, body)
	}
	if code, _ := call(t, h, "DELETE", fmt.Sprintf("/v1/tokens/%d", chef.ID), "oidc:student@dhbw.de", "", nil); code != http.StatusNotFound {
		t.Fatalf("revoking somebody else's token: want 404, got %d", code)
	}
	if code, _ := call(t, h, "DELETE", "/v1/tokens/999", "oidc:student@dhbw.de", "", nil); code != http.StatusNotFound {
		t.Fatalf("revoking a token that does not exist: want 404, got %d", code)
	}
	if code, _ := me(t, h, chef.TokenString); code != http.StatusOK {
		t.Fatalf("the chef's token after the student's attempt: want 200, got %d", code)
	}
}

// A read-only token may read, and every write under /v1 is refused for it.
func TestAPITokenReadOnly(t *testing.T) {
	h, _, _ := tokenServer(t, 10)
	ro := issue(t, h, "chef@dhbw.de", CreateAPITokenRequest{ReadOnly: true})
	if code, _ := call(t, h, "GET", "/v1/fleet", ro.TokenString, "", nil); code != http.StatusOK {
		t.Fatalf("read with a read-only token: want 200, got %d", code)
	}
	if code, _ := call(t, h, "DELETE", "/v1/fleet/package", ro.TokenString, "", nil); code != http.StatusForbidden {
		t.Fatalf("write with a read-only token: want 403, got %d", code)
	}
	if code, _ := call(t, h, "DELETE", fmt.Sprintf("/v1/tokens/%d", ro.ID), ro.TokenString, "", nil); code != http.StatusForbidden {
		t.Fatalf("a read-only token revoking itself: want 403, got %d", code)
	}
}

// Lifetime within the policy, and at most so many tokens per person.
func TestAPITokenLimits(t *testing.T) {
	h, _, _ := tokenServer(t, 2)
	for name, hours := range map[string]int{"never": token.RequestedNever, "beyond the maximum": 365*24 + 1, "negative": -5, "overflowing": 1 << 62} {
		if code, body := call(t, h, "POST", "/v1/tokens", "oidc:student@dhbw.de", "", CreateAPITokenRequest{TTLHours: hours}); code != http.StatusBadRequest {
			t.Errorf("lifetime %s: want 400, got %d %s", name, code, body)
		}
	}
	if code, _ := call(t, h, "POST", "/v1/tokens", "oidc:student@dhbw.de", "", CreateAPITokenRequest{Description: strings.Repeat("x", token.MaxDescriptionLen+1)}); code != http.StatusBadRequest {
		t.Errorf("description too long: want 400, got %d", code)
	}
	// Defaults with an empty body.
	if code, body := call(t, h, "POST", "/v1/tokens", "oidc:student@dhbw.de", "", nil); code != http.StatusCreated {
		t.Fatalf("empty body: %d %s", code, body)
	}
	issue(t, h, "student@dhbw.de", CreateAPITokenRequest{})
	if code, _ := call(t, h, "POST", "/v1/tokens", "oidc:student@dhbw.de", "", CreateAPITokenRequest{}); code != http.StatusConflict {
		t.Fatalf("third token with a cap of two: want 409, got %d", code)
	}
	// The cap is per person.
	issue(t, h, "chef@dhbw.de", CreateAPITokenRequest{})
}

// Without a token service there are no routes, and a bearer with the prefix is only an invalid sign-in.
func TestAPITokensOff(t *testing.T) {
	h := newTestServer(t, false)
	if code, _ := call(t, h, "GET", "/v1/me", APITokenPrefix+"00000000000000000000000000000000", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("token without a service: want 401, got %d", code)
	}
}

// With an API token, which knows only the e-mail address, LLM keys still go to the owner's LiteLLM user, found by the e-mail as when acting on behalf.
func TestKeysWithAPITokenFindTheSubject(t *testing.T) {
	ctx := context.Background()
	ws := workspace.NewService(workspace.NewMemoryStore(), workspace.Config{}, func(context.Context) ([]workspace.Running, error) { return nil, nil })
	if _, err := ws.UpsertFromTree(ctx, "p_1", workspace.TreeProject{Name: "Kurs A", Members: []workspace.Member{{Token: "user:member@dhbw.de"}}, LLMMTokens: 80}); err != nil {
		t.Fatal(err)
	}
	f, srv := newFakeLite(t)
	f.users["kc-sub-of-member"] = map[string]any{"user_id": "kc-sub-of-member", "user_email": "member@dhbw.de", "metadata": map[string]any{}}
	quota := testQuota
	quota.Projects = ws
	tokens := token.NewService(APITokenPrefix, token.NewMemoryStore())
	tok, err := tokens.Issue(ctx, "member@dhbw.de", token.IssueOptions{TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Options{Verifier: fakeVerifier{}, Access: accessFor(t, &roleprovider.Mock{}, nil, nil), Log: zap.NewNop().Sugar(), Keys: testKeys(srv.URL), Quota: quota, Workspaces: ws,
		Tokens: TokenOptions{Service: tokens, TTL: token.TTLPolicy{Default: time.Hour, Max: time.Hour}, MaxPerUser: 1}}).Router()

	if code, body := call(t, h, "POST", "/v1/keys", tok.Secret, "", CreateKeyRequest{Name: "ci"}); code != http.StatusCreated {
		t.Fatalf("create with an API token: %d %s", code, body)
	}
	for _, k := range f.keys {
		if k["user_id"] != "kc-sub-of-member" {
			t.Errorf("the key should belong to the member's LiteLLM user, got %v", k["user_id"])
		}
	}
}

// No expiry only for a read-only token and for an admin; everyone else's tokens always expire.
func TestAPITokenNeverExpires(t *testing.T) {
	h, _, _ := tokenServer(t, 10)
	never := token.RequestedNever
	if code, body := call(t, h, "POST", "/v1/tokens", "oidc:student@dhbw.de", "", CreateAPITokenRequest{TTLHours: never}); code != http.StatusBadRequest {
		t.Errorf("a user's writable token without expiry: want 400, got %d %s", code, body)
	}
	if code, body := call(t, h, "POST", "/v1/tokens", "oidc:student@dhbw.de", "", CreateAPITokenRequest{TTLHours: never, ReadOnly: true}); code != http.StatusCreated {
		t.Errorf("a user's read-only token without expiry: want 201, got %d %s", code, body)
	} else {
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		if m["expires_at"] != nil {
			t.Errorf("read-only token without expiry has expires_at %v", m["expires_at"])
		}
	}
	if code, body := call(t, h, "POST", "/v1/tokens", "oidc:chef@dhbw.de", "", CreateAPITokenRequest{TTLHours: never}); code != http.StatusCreated {
		t.Errorf("an admin's token without expiry: want 201, got %d %s", code, body)
	}
	if code, body := call(t, h, "POST", "/v1/tokens", "oidc:it@dhbw.de", "", CreateAPITokenRequest{TTLHours: never}); code != http.StatusBadRequest {
		t.Errorf("a fleet-admin's writable token without expiry: want 400, got %d %s", code, body)
	}
}

// A signed-in admin acting on behalf creates a token FOR the person: theirs, with their rights, recorded as created by the admin, and counted toward their cap.
func TestAPITokenOnBehalf(t *testing.T) {
	h, _, _ := tokenServer(t, 2)
	code, body := call(t, h, "POST", "/v1/tokens", "oidc:chef@dhbw.de", "student@dhbw.de", CreateAPITokenRequest{Description: "für das Praktikum"})
	var tok APIToken
	if _ = json.Unmarshal(body, &tok); code != http.StatusCreated || tok.TokenString == "" || tok.User != "student@dhbw.de" || tok.CreatedBy != "chef@dhbw.de" || tok.ExpiresAt == nil {
		t.Fatalf("creating on behalf: %d %s", code, body)
	}

	// The person's rights, not the admin's.
	if code, m := me(t, h, tok.TokenString); code != http.StatusOK || m.Email != "student@dhbw.de" || m.Role != access.RoleUser || m.MayActAs {
		t.Fatalf("/me with the token created on behalf: %d %+v", code, m)
	}
	if code, _ := call(t, h, "GET", "/v1/fleet", tok.TokenString, "", nil); code != http.StatusForbidden {
		t.Fatalf("admin route with the person's token: want 403, got %d", code)
	}

	// The person sees it in their own list, with who created it; their own token has no created_by.
	own := issue(t, h, "student@dhbw.de", CreateAPITokenRequest{})
	if own.CreatedBy != "" {
		t.Fatalf("own token has created_by %q", own.CreatedBy)
	}
	code, body = call(t, h, "GET", "/v1/tokens", "oidc:student@dhbw.de", "", nil)
	var list APITokensResponse
	if _ = json.Unmarshal(body, &list); code != http.StatusOK || len(list.Tokens) != 2 || list.Tokens[0].CreatedBy != "chef@dhbw.de" || list.Tokens[1].CreatedBy != "" {
		t.Fatalf("the person's list: %d %s", code, body)
	}
	// None of it is the admin's.
	code, body = call(t, h, "GET", "/v1/tokens", "oidc:chef@dhbw.de", "", nil)
	if _ = json.Unmarshal(body, &list); code != http.StatusOK || len(list.Tokens) != 0 {
		t.Fatalf("the admin's own list: %d %s", code, body)
	}

	// The cap is the person's: two tokens, so a third on their behalf is refused, while the admin's own still works.
	if code, body := call(t, h, "POST", "/v1/tokens", "oidc:chef@dhbw.de", "student@dhbw.de", CreateAPITokenRequest{}); code != http.StatusConflict {
		t.Fatalf("on behalf beyond the person's cap: want 409, got %d %s", code, body)
	}
	issue(t, h, "chef@dhbw.de", CreateAPITokenRequest{})
}

// On behalf, the lifetime follows the person's rules with no admin exception: never-expiring only read-only.
func TestAPITokenOnBehalfNeverExpires(t *testing.T) {
	h, _, _ := tokenServer(t, 10)
	never := token.RequestedNever
	if code, body := call(t, h, "POST", "/v1/tokens", "oidc:chef@dhbw.de", "student@dhbw.de", CreateAPITokenRequest{TTLHours: never}); code != http.StatusBadRequest {
		t.Errorf("writable never-expiring token on behalf: want 400, got %d %s", code, body)
	}
	code, body := call(t, h, "POST", "/v1/tokens", "oidc:chef@dhbw.de", "student@dhbw.de", CreateAPITokenRequest{TTLHours: never, ReadOnly: true})
	var tok APIToken
	if _ = json.Unmarshal(body, &tok); code != http.StatusCreated || tok.ExpiresAt != nil || !tok.ReadOnly || tok.CreatedBy != "chef@dhbw.de" {
		t.Errorf("read-only never-expiring token on behalf: %d %s", code, body)
	}
	// Default and maximum as usual.
	if code, body := call(t, h, "POST", "/v1/tokens", "oidc:chef@dhbw.de", "student@dhbw.de", CreateAPITokenRequest{TTLHours: 365*24 + 1}); code != http.StatusBadRequest {
		t.Errorf("beyond the maximum on behalf: want 400, got %d %s", code, body)
	}
}

// A writable never-expiring token on behalf of a person who is an admin themself is refused too: the exception is for an admin's OWN token.
func TestAPITokenOnBehalfOfAnAdmin(t *testing.T) {
	h, _, roles := tokenServer(t, 10)
	roles.Memberships["chef2@dhbw.de"] = []string{"group:it-leitung"}
	if code, m := me(t, h, "oidc:chef2@dhbw.de"); code != http.StatusOK || m.Role != access.RoleAdmin {
		t.Fatalf("second admin: %d %+v", code, m)
	}
	if code, body := call(t, h, "POST", "/v1/tokens", "oidc:chef@dhbw.de", "chef2@dhbw.de", CreateAPITokenRequest{TTLHours: token.RequestedNever}); code != http.StatusBadRequest {
		t.Fatalf("writable never-expiring token on behalf of an admin: want 400, got %d %s", code, body)
	}
	if code, body := call(t, h, "POST", "/v1/tokens", "oidc:chef2@dhbw.de", "", CreateAPITokenRequest{TTLHours: token.RequestedNever}); code != http.StatusCreated {
		t.Fatalf("the same admin's own never-expiring token: want 201, got %d %s", code, body)
	}
}

// Nobody but an admin acts on behalf, so nobody else creates a token for another person.
func TestAPITokenOnBehalfOnlyAdmins(t *testing.T) {
	h, _, _ := tokenServer(t, 10)
	for _, actor := range []string{"student@dhbw.de", "it@dhbw.de"} {
		if code, body := call(t, h, "POST", "/v1/tokens", "oidc:"+actor, "chef@dhbw.de", CreateAPITokenRequest{}); code != http.StatusForbidden {
			t.Errorf("%s creating a token on behalf: want 403, got %d %s", actor, code, body)
		}
	}
	code, body := call(t, h, "GET", "/v1/tokens", "oidc:chef@dhbw.de", "", nil)
	var list APITokensResponse
	if _ = json.Unmarshal(body, &list); code != http.StatusOK || len(list.Tokens) != 0 {
		t.Fatalf("the admin's list after the attempts: %d %s", code, body)
	}
}
