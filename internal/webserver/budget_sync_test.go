package webserver

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/pfisterer/llm-management-api/internal/access"
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
	f.keys["old"] = map[string]any{"token": "old", "user_id": "kc-member"}                     // from before the pools
	f.keys["stale"] = map[string]any{"token": "stale", "user_id": "kc-gone", "team_id": "t-x"} // in a project that is gone
	f.keys["svc"] = map[string]any{"token": "svc", "user_id": "service"}
	f.teams["t-x"] = map[string]any{"team_id": "t-x", "team_alias": "Alt", "max_budget": 3.0, "budget_duration": "30d", "spend": 1.0, "metadata": map[string]any{"source": "llm-management-api"}}

	acc, err := access.NewService(access.NewMemoryStore(), &roleprovider.Mock{}, access.Config{Tiers: []string{"student"}})
	if err != nil {
		t.Fatal(err)
	}
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
	tiers := map[string]keys.Tier{"student": {Name: "student", RPM: 60}}
	b := NewBudgetSync(BudgetSync{Lite: client, Keys: keys.NewService(client, 5), Access: acc, Interval: time.Hour, Log: zap.NewNop().Sugar(),
		Quota: LLMQuota{Tiers: tiers, DefaultTier: "student", Projects: ws}})

	// Teams: t-1 new, t-2 and t-x to 0. Quotas: both people (rpm). Keys: both get the tier's key quota, old moved, stale detached.
	n, err := b.Pass(ctx)
	if err != nil || n != 9 {
		t.Fatalf("pass: %d %v", n, err)
	}
	if got := f.teams["t-1"]["max_budget"]; got != 30*keys.DollarsPerMTokens {
		t.Fatalf("project team budget: %v", got)
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
	if f.keys["old"]["rpm_limit"] != 60.0 {
		t.Fatalf("key without the tier's limits: %v", f.keys["old"])
	}
	if f.users["service"]["max_budget"] != 5.0 || f.keys["svc"]["team_id"] != nil {
		t.Fatalf("service user touched: %v %v", f.users["service"], f.keys["svc"])
	}
	// Nothing changed: the next pass writes nothing.
	if n, err := b.Pass(ctx); err != nil || n != 0 {
		t.Fatalf("second pass: %d %v", n, err)
	}
}
