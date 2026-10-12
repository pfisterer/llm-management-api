package webserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/pfisterer/llm-management-api/internal/access"
	"github.com/pfisterer/llm-management-api/internal/keys"
	"github.com/pfisterer/llm-management-api/internal/litellm"
	"github.com/pfisterer/llm-management-api/internal/roleprovider"
	"github.com/pfisterer/llm-management-api/internal/workspace"
)

// The pass sets every person's budget from the tree: the project's tokens for a member, 0 for somebody without a project (whose old key must stop), nothing for a user without email.
func TestBudgetSyncPass(t *testing.T) {
	var mu sync.Mutex
	users := map[string]map[string]any{
		"kc-member": {"user_id": "kc-member", "user_email": "member@dhbw.de", "max_budget": 8.0, "budget_duration": "30d"},
		"kc-gone":   {"user_id": "kc-gone", "user_email": "gone@dhbw.de", "max_budget": 40.0, "budget_duration": "30d"},
		"service":   {"user_id": "service", "max_budget": 5.0},
	}
	var updates []string
	lite := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/user/list":
			var list []map[string]any
			for _, u := range users {
				list = append(list, u)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"users": list, "total_pages": 1})
		case "/user/update":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			uid := body["user_id"].(string)
			for k, v := range body {
				users[uid][k] = v
			}
			updates = append(updates, uid)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(lite.Close)

	acc, err := access.NewService(access.NewMemoryStore(), &roleprovider.Mock{}, access.Config{Tiers: []string{"student"}})
	if err != nil {
		t.Fatal(err)
	}
	ws := workspace.NewService(workspace.NewMemoryStore(), workspace.Config{Profiles: []workspace.Profile{{Slug: "cpu", Name: "CPU"}}},
		func(context.Context) ([]workspace.Running, error) { return nil, nil })
	if _, err := ws.UpsertFromTree(context.Background(), "p_1", workspace.TreeProject{Name: "Kurs", Members: []workspace.Member{{Token: "user:member@dhbw.de"}}, LLMMTokens: 30}); err != nil {
		t.Fatal(err)
	}
	client := litellm.New(lite.URL, "master", 5*time.Second)
	tiers := map[string]keys.Tier{"student": {Name: "student", UserBudget: 8, UserBudgetDuration: "30d"}}
	b := NewBudgetSync(BudgetSync{Lite: client, Keys: keys.NewService(client, 5), Access: acc, Interval: time.Hour, Log: zap.NewNop().Sugar(),
		Quota: LLMQuota{Tiers: tiers, DefaultTier: "student", Projects: ws}})

	n, err := b.Pass(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("pass: %d %v (updates %v)", n, err, updates)
	}
	if got := users["kc-member"]["max_budget"]; got != 30*DollarsPerMTokens {
		t.Fatalf("member budget: %v", got)
	}
	if got := users["kc-gone"]["max_budget"]; got != 0.0 {
		t.Fatalf("budget of somebody without project: %v", got)
	}
	if got := users["service"]["max_budget"]; got != 5.0 {
		t.Fatalf("service user touched: %v", got)
	}
	// Nothing changed: the next pass writes nothing.
	if n, err := b.Pass(context.Background()); err != nil || n != 0 {
		t.Fatalf("second pass: %d %v", n, err)
	}
}
