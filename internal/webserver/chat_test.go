package webserver

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pfisterer/llm-management-api/internal/access"
	"github.com/pfisterer/llm-management-api/internal/keys"
	"github.com/pfisterer/llm-management-api/internal/litellm"
	"github.com/pfisterer/llm-management-api/internal/roleprovider"
	"go.uber.org/zap"
)

// A LiteLLM management API that knows users and keys by value — just enough
// for the chat exchange.
func fakeBackend(t *testing.T) *httptest.Server {
	var mu sync.Mutex
	users, keyVals := map[string]bool{}, map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch r.URL.Path {
		case "/user/info":
			if !users[r.URL.Query().Get("user_id")] {
				w.WriteHeader(404)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"user_info": map[string]any{"user_id": r.URL.Query().Get("user_id"), "max_budget": 1, "metadata": map[string]any{"tier": "staff"}}, "keys": []any{}})
		case "/user/new":
			users[body["user_id"].(string)] = true
		case "/user/update":
		case "/key/info":
			if !keyVals[r.URL.Query().Get("key")] {
				w.WriteHeader(404)
			}
		case "/key/generate":
			keyVals[body["key"].(string)] = true
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

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

	roles := &roleprovider.Mock{}
	svc, err := access.NewService(access.NewMemoryStore(), roles, access.Config{
		Tiers: []string{"staff"}, BootstrapAdmins: []string{"admin@dhbw.de"}, BootstrapAdminTier: "staff",
	})
	if err != nil {
		t.Fatal(err)
	}
	h := ChatRouter(ChatOptions{
		Access: svc, Keys: keys.NewService(litellm.New(fakeBackend(t).URL, "master", 5*time.Second), 5),
		Tiers:   map[string]keys.Tier{"staff": {Name: "staff", KeyBudget: 2, UserBudget: 40}},
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
	if code, body := post("sub-x", "other@dhbw.de", plain); code != 403 || !strings.Contains(body, "Kein Zugang") {
		t.Errorf("no rule: %d %s", code, body)
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
