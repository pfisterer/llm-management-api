package webserver

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/pfisterer/llm-management-api/internal/keys"
	"github.com/pfisterer/llm-management-api/internal/litellm"
	"github.com/pfisterer/llm-management-api/internal/roleprovider"
	"github.com/pfisterer/llm-management-api/internal/workspace"
)

// The pass makes a team per project with the project's tokens (0 for an ended project and for a team whose project is gone), sets every person's budget to 0 (the pools pay), moves a member's keys into their project and takes the keys of somebody without a project out of every team; a user without email is not touched.
func TestBudgetSyncPass(t *testing.T) {
	f, srv := newFakeLite(t)
	f.users["kc-member"] = map[string]any{"user_id": "kc-member", "user_email": "member@dhbw.de", "max_budget": 8.0, "budget_duration": "30d"}
	f.users["kc-gone"] = map[string]any{"user_id": "kc-gone", "user_email": "gone@dhbw.de", "max_budget": 0.0, "budget_duration": "30d"}
	f.users["service"] = map[string]any{"user_id": "service", "max_budget": 5.0}
	f.keys["old"] = map[string]any{"token": "old", "user_id": "kc-member", "rpm_limit": 60.0}  // from before the pools, with a limit of its own
	f.keys["stale"] = map[string]any{"token": "stale", "user_id": "kc-gone", "team_id": "t-x"} // in a project that is gone
	f.keys["svc"] = map[string]any{"token": "svc", "user_id": "service"}
	f.teams["t-x"] = map[string]any{"team_id": "t-x", "team_alias": "Alt", "max_budget": 3.0, "budget_duration": "30d", "spend": 1.0, "metadata": map[string]any{"source": "llm-management-api"}}

	acc := accessFor(t, &roleprovider.Mock{}, nil, nil)
	ws := workspace.NewService(workspace.NewMemoryStore(), workspace.Config{Profiles: []workspace.Profile{{Slug: "cpu", Name: "CPU"}}},
		func(context.Context) ([]workspace.Running, error) { return nil, nil })
	ctx := context.Background()
	if _, err := ws.UpsertFromTree(ctx, "p_1", workspace.TreeProject{Name: "Kurs", Members: []workspace.Member{{Token: "user:member@dhbw.de"}}, LLMMTokens: 30}); err != nil {
		t.Fatal(err)
	}
	ended := time.Now().AddDate(0, 0, -10)
	if _, err := ws.UpsertFromTree(ctx, "p_2", workspace.TreeProject{Name: "Vorbei", Members: []workspace.Member{{Token: "user:gone@dhbw.de"}}, LLMMTokens: 50, EndsAt: &ended}); err != nil {
		t.Fatal(err)
	}
	f.teams["t-2"] = map[string]any{"team_id": "t-2", "team_alias": "Vorbei", "max_budget": 5.0, "budget_duration": "30d", "spend": 4.0, "metadata": map[string]any{"source": "llm-management-api"}}
	client := litellm.New(srv.URL, "master", 5*time.Second)
	b := NewBudgetSync(BudgetSync{Lite: client, Keys: keys.NewService(client, 5, keys.KeyBudget{Share: 0.05, Window: "4h"}), Access: acc, Interval: time.Hour, Log: zap.NewNop().Sugar(),
		Quota: LLMQuota{DefaultRPM: 60, DefaultTPM: 200000, BaseModels: []string{"chat-default"}, Projects: ws}})

	// Teams: t-1 new, t-2 and t-x to 0 (t-2 also gets its limits). Quotas: the member's budget back to 0. Keys: old moved and given its budget (its own limit cleared), stale detached and given a budget of 0.
	n, err := b.Pass(ctx)
	if err != nil || n != 8 {
		t.Fatalf("pass: %d %v", n, err)
	}
	if tm := f.teams["t-1"]; tm["max_budget"] != 30*keys.DollarsPerMTokens || tm["rpm_limit"] != 60.0 || tm["tpm_limit"] != 200000.0 || fmt.Sprint(tm["models"]) != "[chat-default]" {
		t.Fatalf("project team: %v", tm)
	}
	if f.teams["t-2"]["max_budget"] != 0.0 || f.teams["t-x"]["max_budget"] != 0.0 || f.teams["t-2"]["spend"] != 4.0 {
		t.Fatalf("ended/gone teams: %v %v", f.teams["t-2"], f.teams["t-x"])
	}
	if got := f.users["kc-member"]["max_budget"]; got != 0.0 {
		t.Fatalf("member budget: %v", got)
	}
	if f.keys["old"]["team_id"] != "t-1" || !f.member("t-1", "kc-member") {
		t.Fatalf("member's key not moved: %v", f.keys["old"])
	}
	if f.keys["stale"]["team_id"] != nil {
		t.Fatalf("key of somebody without project still in %v", f.keys["stale"]["team_id"])
	}
	if old := f.keys["old"]; old["rpm_limit"] != nil || math.Abs(old["max_budget"].(float64)-3*0.05) > 1e-9 || old["budget_duration"] != "4h" {
		t.Fatalf("key not given its pool's budget: %v", old)
	}
	if f.users["service"]["max_budget"] != 5.0 || f.keys["svc"]["team_id"] != nil {
		t.Fatalf("service user touched: %v %v", f.users["service"], f.keys["svc"])
	}
	// Nothing changed: the next pass writes nothing.
	if n, err := b.Pass(ctx); err != nil || n != 0 {
		t.Fatalf("second pass: %d %v", n, err)
	}
}
