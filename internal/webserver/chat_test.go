package webserver

import (
	"context"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pfisterer/llm-management-api/internal/keys"
	"github.com/pfisterer/llm-management-api/internal/litellm"
	"github.com/pfisterer/llm-management-api/internal/roleprovider"
	"github.com/pfisterer/llm-management-api/internal/workspace"
	"go.uber.org/zap"
)

func TestChatExchange(t *testing.T) {
	var gotAuth, gotBody string
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		if r.Header.Get("X-Librechat-User-Email") != "" {
			t.Error("identity header leaked to the gateway")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {}\n\n")
	}))
	defer gateway.Close()

	svc := accessFor(t, &roleprovider.Mock{Memberships: map[string][]string{"g@dhbw.de": {"group:kurs"}}}, []string{"user:admin@dhbw.de"}, nil)
	ws := workspace.NewService(workspace.NewMemoryStore(), workspace.Config{}, func(context.Context) ([]workspace.Running, error) { return nil, nil })
	if _, err := ws.UpsertFromTree(context.Background(), "p_1", workspace.TreeProject{Name: "A", Members: []workspace.Member{{Token: "user:admin@dhbw.de"}, {Token: "user:member@dhbw.de"}, {Token: "group:kurs"}}, LLMMTokens: 40, LLMRPM: 30}); err != nil {
		t.Fatal(err)
	}
	f, srv := newFakeLite(t)
	h := ChatRouter(ChatOptions{
		Access: svc, Keys: keys.NewService(litellm.New(srv.URL, "master", 5*time.Second), 5, keys.KeyBudget{Share: 0.05, Window: "4h"}),
		Quota:   LLMQuota{DefaultRPM: 60, DefaultTPM: 200000, BaseModels: []string{"chat-default"}, Projects: ws},
		Gateway: gateway.URL, MasterKey: "master", Token: "chat-secret", Log: zap.NewNop().Sugar(),
	})
	post := func(sub, email, body string) (int, string) {
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer chat-secret")
		r.Header.Set("X-Librechat-User-Openid", sub)
		r.Header.Set("X-Librechat-User-Email", email)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code, rec.Body.String()
	}
	plain := `{"model":"chat-default","messages":[{"role":"user","content":"hallo"}]}`

	if code, _ := post("{{LIBRECHAT_USER_OPENIDID}}", "admin@dhbw.de", plain); code != 401 {
		t.Errorf("placeholder: %d", code)
	}
	if code, body := post("sub-x", "other@dhbw.de", plain); code != 403 || !strings.Contains(body, "Kein Projekt mit LLM-Tokens") {
		t.Errorf("no project: %d %s", code, body)
	}
	// The project's tokens are enough, also through a group; the chat key is in the project's team (with its limits) and has the project's key budget.
	if code, body := post("sub-m", "member@dhbw.de", plain); code != 200 {
		t.Errorf("member: %d %s", code, body)
	}
	if code, body := post("sub-g", "g@dhbw.de", plain); code != 200 {
		t.Errorf("member through a group: %d %s", code, body)
	}
	if k := f.keyBy(keys.ChatKey("master", "kc-sub-m")); k == nil || math.Abs(k["max_budget"].(float64)-0.2) > 1e-9 || k["rpm_limit"] != nil || k["team_id"] != "t-1" || f.teams["t-1"]["rpm_limit"] != 30.0 {
		t.Errorf("chat key: %v", k)
	}
	if code, body := post("sub-a", "admin@dhbw.de", plain); code != 200 || !strings.Contains(body, "data:") {
		t.Fatalf("admin: %d %s", code, body)
	}
	if want := "Bearer " + keys.ChatKey("master", "kc-sub-a"); gotAuth != want {
		t.Errorf("gateway got %q, want %q", gotAuth, want)
	}

	// A document on the newest message is refused, on an older one replaced.
	newest := `{"messages":[{"role":"user","content":[{"type":"file","file":{"filename":"a.pdf"}}]}]}`
	if code, body := post("sub-a", "admin@dhbw.de", newest); code != 400 || !strings.Contains(body, "a.pdf") {
		t.Errorf("newest attachment: %d %s", code, body)
	}
	older := `{"messages":[{"role":"user","content":[{"type":"file","file":{"filename":"b.pdf"}}]},{"role":"assistant","content":"ok"},{"role":"user","content":"weiter"}]}`
	if code, _ := post("sub-a", "admin@dhbw.de", older); code != 200 || !strings.Contains(gotBody, "wurde nicht an das Modell übergeben") || strings.Contains(gotBody, `"type":"file"`) {
		t.Errorf("older attachment: %d %s", code, gotBody)
	}
}

// Regression: anything in the LibreChat pod (an agent's HTTP action) could call the exchange with any identity headers. Now it needs the shared token, compared in constant time; without a configured token nothing passes.
func TestChatExchangeToken(t *testing.T) {
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "{}") }))
	defer gateway.Close()
	svc := accessFor(t, &roleprovider.Mock{}, nil, nil)
	ws := workspace.NewService(workspace.NewMemoryStore(), workspace.Config{}, func(context.Context) ([]workspace.Running, error) { return nil, nil })
	if _, err := ws.UpsertFromTree(context.Background(), "p_1", workspace.TreeProject{Name: "A", Members: []workspace.Member{{Token: "user:member@dhbw.de"}}, LLMMTokens: 40}); err != nil {
		t.Fatal(err)
	}
	_, srv := newFakeLite(t)
	router := func(token string) http.Handler {
		return ChatRouter(ChatOptions{
			Access: svc, Keys: keys.NewService(litellm.New(srv.URL, "master", 5*time.Second), 5, keys.KeyBudget{Share: 0.05, Window: "4h"}),
			Quota: LLMQuota{BaseModels: []string{"chat-default"}, Projects: ws}, Gateway: gateway.URL, MasterKey: "master", Token: token, Log: zap.NewNop().Sugar(),
		})
	}
	call := func(h http.Handler, auth string) int {
		r := httptest.NewRequest("GET", "/v1/models", nil)
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		r.Header.Set("X-Librechat-User-Openid", "sub-m")
		r.Header.Set("X-Librechat-User-Email", "member@dhbw.de")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}
	h := router("chat-secret")
	for _, auth := range []string{"", "Bearer via-llm-management-api", "Bearer chat-secre", "Bearer chat-secret2", "chat-secret"} {
		if code := call(h, auth); code != http.StatusUnauthorized {
			t.Errorf("%q: %d, want 401", auth, code)
		}
	}
	if code := call(h, "Bearer chat-secret"); code != http.StatusOK {
		t.Errorf("right token: %d", code)
	}
	for _, auth := range []string{"", "Bearer ", "Bearer chat-secret"} {
		if code := call(router(""), auth); code != http.StatusUnauthorized {
			t.Errorf("no token configured, %q: %d, want 401", auth, code)
		}
	}
}

// Regression: sub and e-mail were taken as they came. Now a sub whose LiteLLM user has another e-mail, or an e-mail that is another sub's, is refused (and not served from the cache of the right pair).
func TestChatExchangeIdentityBinding(t *testing.T) {
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "{}") }))
	defer gateway.Close()
	svc := accessFor(t, &roleprovider.Mock{}, nil, nil)
	ws := workspace.NewService(workspace.NewMemoryStore(), workspace.Config{}, func(context.Context) ([]workspace.Running, error) { return nil, nil })
	if _, err := ws.UpsertFromTree(context.Background(), "p_1", workspace.TreeProject{Name: "A", Members: []workspace.Member{{Token: "user:victim@dhbw.de"}, {Token: "user:attacker@dhbw.de"}}, LLMMTokens: 40}); err != nil {
		t.Fatal(err)
	}
	f, srv := newFakeLite(t)
	h := ChatRouter(ChatOptions{
		Access: svc, Keys: keys.NewService(litellm.New(srv.URL, "master", 5*time.Second), 5, keys.KeyBudget{Share: 0.05, Window: "4h"}),
		Quota: LLMQuota{BaseModels: []string{"chat-default"}, Projects: ws}, Gateway: gateway.URL, MasterKey: "master", Token: "chat-secret", Log: zap.NewNop().Sugar(),
	})
	call := func(sub, email string) int {
		r := httptest.NewRequest("GET", "/v1/models", nil)
		r.Header.Set("Authorization", "Bearer chat-secret")
		r.Header.Set("X-Librechat-User-Openid", sub)
		r.Header.Set("X-Librechat-User-Email", email)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}
	if code := call("sub-v", "victim@dhbw.de"); code != http.StatusOK {
		t.Fatalf("victim: %d", code)
	}
	if code := call("sub-a", "attacker@dhbw.de"); code != http.StatusOK {
		t.Fatalf("attacker as themself: %d", code)
	}
	for _, tc := range []struct{ name, sub, email string }{
		{"victim's sub, own e-mail", "sub-v", "attacker@dhbw.de"},
		{"victim's e-mail, own sub", "sub-a", "victim@dhbw.de"},
		{"victim's e-mail, new sub", "sub-new", "victim@dhbw.de"},
	} {
		if code := call(tc.sub, tc.email); code != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", tc.name, code)
		}
	}
	if u := f.users["kc-sub-v"]; u == nil || u["user_email"] != "victim@dhbw.de" {
		t.Errorf("victim's LiteLLM user changed: %v", u)
	}
	if _, ok := f.users["kc-sub-new"]; ok {
		t.Errorf("user made for a foreign e-mail")
	}
}
