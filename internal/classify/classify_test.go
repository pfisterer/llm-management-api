package classify

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"go.uber.org/zap"
)

// A gateway with one capable model ("good") and one that only chats ("prose").
func fakeGateway(t *testing.T, calls *atomic.Int32) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		prompt := req["messages"].([]any)[0].(map[string]any)["content"].(string)
		good := req["model"] == "good"
		msg := map[string]any{"content": "Gern."}
		switch {
		case req["tools"] != nil && good:
			msg["tool_calls"] = []any{map[string]any{"function": map[string]any{"name": "wetter", "arguments": `{"ort":"Mannheim"}`}}}
		case good && strings.Contains(prompt, "Python"):
			msg["content"] = "def is_even(n): return n % 2 == 0"
		case good && strings.Contains(prompt, "JavaScript"):
			msg["content"] = "function sum(a, b) { return a + b }"
		case good && strings.Contains(prompt, "SQL"):
			msg["content"] = "SELECT * FROM kunden;"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": msg}},
			"usage": map[string]any{"completion_tokens": 100000}})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

var aliases = map[string]AliasDef{"chat-default": {}, "chat-fast": {}, "code": {Members: []string{"pinned"}}}

func TestClassify(t *testing.T) {
	var calls atomic.Int32
	store := NewMemoryStore()
	no := false
	svc := NewService(aliases, Thresholds{FastTokensPerSecond: 60, RequireToolsForChat: &no}, store, fakeGateway(t, &calls), "k", zap.NewNop().Sugar())

	got := svc.Many(t.Context(), []string{"good", "prose", "pinned"})
	if strings.Join(got["good"], ",") != "chat-fast,code" {
		t.Errorf("good: %v", got["good"])
	}
	// Tools are not required here, so prose still gets a chat alias but no code.
	if len(got["prose"]) != 1 || got["prose"][0] != "chat-fast" {
		t.Errorf("prose: %v", got["prose"])
	}
	if strings.Join(got["pinned"], ",") != "code" {
		t.Errorf("pinned: %v", got["pinned"])
	}
	r, ok, _ := store.Get(t.Context(), "good")
	if !ok || r.Stamp != "60/3/false" || r.Probes == nil || !r.Probes.Tools {
		t.Errorf("stored: %+v %v", r, ok)
	}

	// Cached: no further gateway calls for the same thresholds.
	before := calls.Load()
	svc.Many(t.Context(), []string{"good"})
	if calls.Load() != before {
		t.Errorf("cache not used")
	}
	// Requiring tools invalidates the stamp; prose loses its chat alias.
	yes := true
	strict := NewService(aliases, Thresholds{FastTokensPerSecond: 60, RequireToolsForChat: &yes}, store, svc.gateway, "k", zap.NewNop().Sugar())
	if got := strict.Many(t.Context(), []string{"prose"}); len(got["prose"]) != 0 {
		t.Errorf("strict prose: %v", got["prose"])
	}
}
