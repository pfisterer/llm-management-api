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
		Gateway: gateway.URL, MasterKey: "master", Log: zap.NewNop().Sugar(),
	})
	post := func(sub, email, body string) (int, string) {
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
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
