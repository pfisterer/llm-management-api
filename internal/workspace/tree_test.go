package workspace

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pfisterer/llm-management-api/internal/usage"
)

// The tree creates a project with what it owns and sensible rest; a second upsert changes only the tree's part and keeps what the managers set.
func TestUpsertFromTree(t *testing.T) {
	ctx := context.Background()
	s := service()
	node := "p_7841d99e-68a2-47b7-88a5-1ab55180f4f5"
	tp := TreeProject{Name: "Kurs", Members: []Member{{Token: "user:o@dhbw.de", Role: RoleManager}, {Token: "group:k", Role: RoleMember}},
		Budgets:     []TreeBudget{{Unit: "gpu_hours", GPUClass: "l4-3q", Window: "30d", Amount: 500}, {Unit: "vcpu_hours", Window: "30d", Amount: 1000}},
		GPUsByClass: map[string]int{"l4-3q": 4}, LLMMTokens: 200}
	st, err := s.UpsertFromTree(ctx, node, tp)
	if err != nil {
		t.Fatal(err)
	}
	if st.Slug != "t-7841d99e-68a2-47b7-88a5-1ab55180f4f5" || st.Node != node {
		t.Errorf("status = %+v", st)
	}
	p, _ := s.Get(ctx, st.Slug)
	if !p.Managed() || !slices.Contains(p.Profiles, "pytorch") || !slices.Contains(p.Profiles, "cpu") || p.Limits.GPUsPerPerson != 1 || p.GPUsByClass["l4-3q"] != 4 || len(p.Budgets) != 2 {
		t.Fatalf("created project = %+v", p)
	}

	// A manager narrows: a share per person, fewer profiles; members stay the tree's.
	p.Budgets = append(p.Budgets, Budget{Unit: "gpu_hours", GPUClass: "l4-3q", Window: "7d", Amount: 10, PerPerson: true})
	p.Profiles = []string{"cpu"}
	p.Members = []Member{{Token: "user:x@dhbw.de"}}
	if _, err := s.Update(ctx, p, "o@dhbw.de", false); err != nil {
		t.Fatal(err)
	}
	p, _ = s.Get(ctx, st.Slug)
	if len(p.Members) != 2 || len(p.Budgets) != 3 || !slices.Equal(p.Profiles, []string{"cpu"}) {
		t.Errorf("after the manager: %+v", p)
	}
	// Not beyond what the tree gives.
	p.Budgets = append(p.Budgets, Budget{Unit: "vcpu_hours", Window: "24h", Amount: 5000})
	if _, err := s.Update(ctx, p, "o@dhbw.de", false); err == nil || !strings.Contains(reason(err), "größer") {
		t.Errorf("a budget above the tree's: %v", err)
	}

	// The tree shrinks: its budgets are replaced, the manager's share stays.
	tp.Budgets = []TreeBudget{{Unit: "gpu_hours", GPUClass: "l4-3q", Window: "30d", Amount: 100}}
	if _, err := s.UpsertFromTree(ctx, node, tp); err != nil {
		t.Fatal(err)
	}
	p, _ = s.Get(ctx, st.Slug)
	var tree, own int
	for _, b := range p.Budgets {
		if b.Tree {
			tree++
		} else {
			own++
		}
	}
	if tree != 1 || own != 1 || !slices.Equal(p.Profiles, []string{"cpu"}) {
		t.Errorf("after the tree shrank: %+v", p.Budgets)
	}

	if err := s.DeleteFromTree(ctx, node); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteFromTree(ctx, node); err != ErrNotFound {
		t.Errorf("second delete = %v, want not found", err)
	}
}

// The tree reads what a project used of its budgets in the current window; LLM tokens of a person are the sum over their active projects.
func TestTreeUsageAndLLM(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	s := service()
	s.now = func() time.Time { return now }
	u := usage.NewMemoryStore()
	s.SetUsage(u)
	st, err := s.UpsertFromTree(ctx, "p_1", TreeProject{Name: "A", Members: []Member{{Token: "group:k"}},
		Budgets: []TreeBudget{{Unit: "gpu_hours", GPUClass: "l4-3q", Window: "30d", Amount: 50}}, LLMMTokens: 80})
	if err != nil {
		t.Fatal(err)
	}
	_ = u.Add(ctx, []usage.Row{
		{Day: usage.Day(now), Project: st.Slug, Person: "a@dhbw.de", GPUClass: "l4-3q", GPUHours: 3},
		{Day: usage.Day(now).AddDate(0, 0, -40), Project: st.Slug, Person: "a@dhbw.de", GPUClass: "l4-3q", GPUHours: 100},
	})
	ended := now.AddDate(0, 0, -10)
	if _, err := s.UpsertFromTree(ctx, "p_2", TreeProject{Name: "B", Members: []Member{{Token: "user:a@dhbw.de"}}, LLMMTokens: 20}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertFromTree(ctx, "p_3", TreeProject{Name: "C", Members: []Member{{Token: "group:k"}}, LLMMTokens: 1000, EndsAt: &ended}); err != nil {
		t.Fatal(err)
	}
	list, err := s.TreeProjects(ctx)
	if err != nil || len(list) != 3 {
		t.Fatalf("tree projects: %v %v", list, err)
	}
	if u := list[0].Used; len(u) != 1 || u[0].Used != 3 {
		t.Errorf("used in the window = %+v, want 3 (the 100 are older)", u)
	}
	if n, _ := s.LLMMTokensFor(ctx, []string{"user:a@dhbw.de", "group:k"}); n != 100 {
		t.Errorf("LLM tokens = %d, want 80 + 20 (the ended project does not count)", n)
	}
}
