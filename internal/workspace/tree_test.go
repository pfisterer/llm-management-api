package workspace

import (
	"context"
	"errors"
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
		GPUsByClass: map[string]int{"l4-3q": 4}, LLMMTokens: 200, LLMRPM: 120, LLMTPM: 400000, LLMModels: []string{" code", "all-proxy-models", "code", ""}}
	st, err := s.UpsertFromTree(ctx, node, tp)
	if err != nil {
		t.Fatal(err)
	}
	if st.Slug != "t-7841d99e-68a2-47b7-88a5-1ab55180f4f5" || st.Node != node {
		t.Errorf("status = %+v", st)
	}
	p, _ := s.Get(ctx, st.Slug)
	if !p.Managed() || !slices.Contains(p.Profiles, "gpu") || !slices.Contains(p.Profiles, "cpu") || p.Limits.GPUsPerPerson != 1 || p.GPUsByClass["l4-3q"] != 4 || len(p.Budgets) != 2 || p.LLMRPM != 120 || p.LLMTPM != 400000 || !slices.Equal(p.LLMModels, []string{"all-proxy-models", "code"}) {
		t.Fatalf("created project = %+v", p)
	}
	if pools, _ := s.LLMPoolsFor(ctx, []string{"group:k"}); len(pools) != 1 || pools[0].RPM != 120 || pools[0].TPM != 400000 || !slices.Equal(pools[0].Models, []string{"all-proxy-models", "code"}) {
		t.Fatalf("pool = %+v, want the project's limits and models", pools)
	}

	// An admin narrows: a share per person, fewer profiles; members and the LLM limits stay the tree's, also for an admin.
	p.Budgets = append(p.Budgets, Budget{Unit: "gpu_hours", GPUClass: "l4-3q", Window: "7d", Amount: 10, PerPerson: true})
	p.Profiles = []string{"cpu"}
	p.Members = []Member{{Token: "user:x@dhbw.de"}}
	p.LLMRPM, p.LLMModels, p.LLMTPM = 9999, []string{"everything"}, 1
	if _, err := s.Update(ctx, p, "root@dhbw.de", true); err != nil {
		t.Fatal(err)
	}
	p, _ = s.Get(ctx, st.Slug)
	if len(p.Members) != 2 || len(p.Budgets) != 3 || !slices.Equal(p.Profiles, []string{"cpu"}) || p.LLMRPM != 120 || p.LLMTPM != 400000 || len(p.LLMModels) != 2 {
		t.Errorf("after the admin: %+v", p)
	}
	// Not beyond what the tree gives.
	p.Budgets = append(p.Budgets, Budget{Unit: "vcpu_hours", Window: "24h", Amount: 5000})
	if _, err := s.Update(ctx, p, "root@dhbw.de", true); err == nil || !strings.Contains(reason(err), "größer") {
		t.Errorf("a budget above the tree's: %v", err)
	}

	// The tree shrinks: its budgets are replaced, the manager's share stays; the LLM limits and models are dropped (the defaults then).
	tp.Budgets = []TreeBudget{{Unit: "gpu_hours", GPUClass: "l4-3q", Window: "30d", Amount: 100}}
	tp.LLMRPM, tp.LLMTPM, tp.LLMModels = 0, 0, nil
	if _, err := s.UpsertFromTree(ctx, node, tp); err != nil {
		t.Fatal(err)
	}
	p, _ = s.Get(ctx, st.Slug)
	if p.LLMRPM != 0 || p.LLMTPM != 0 || len(p.LLMModels) != 0 {
		t.Errorf("LLM limits after the tree dropped them = %d %d %v", p.LLMRPM, p.LLMTPM, p.LLMModels)
	}
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

// The tree reads what a project used of its budgets in the current window, the LLM pool included; a person draws from the pools of their active projects with tokens.
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
	if _, err := s.UpsertFromTree(ctx, "p_4", TreeProject{Name: "A", Members: []Member{{Token: "group:other"}}, LLMMTokens: 5}); err != nil {
		t.Fatal(err)
	}
	list, err := s.TreeProjects(ctx)
	if err != nil || len(list) != 4 {
		t.Fatalf("tree projects: %v %v", list, err)
	}
	// Without LiteLLM nothing is measured of the LLM pools, so nothing is reported.
	if u := list[0].Used; len(u) != 1 || u[0].Used != 3 {
		t.Errorf("used in the window = %+v, want 3 (the 100 are older)", u)
	}

	var asked []string
	s.SetLLMUsed(func(_ context.Context, slugs []string) (map[string]float64, error) {
		asked = slugs
		return map[string]float64{st.Slug: 12.5}, nil
	})
	list, _ = s.TreeProjects(ctx)
	byNode := map[string]TreeStatus{}
	for _, x := range list {
		byNode[x.Node] = x
	}
	if len(asked) != 4 {
		t.Errorf("asked for %v, want every project with tokens at once", asked)
	}
	if u := byNode["p_1"].Used; len(u) != 2 || u[1] != (TreeUse{TreeBudget: TreeBudget{Unit: "llm_mtokens", Window: "30d", Amount: 80}, Used: 12.5}) {
		t.Errorf("LLM pool = %+v", u)
	}
	if u := byNode["p_2"].Used; len(u) != 1 || u[0].Unit != "llm_mtokens" || u[0].Used != 0 || u[0].Amount != 20 {
		t.Errorf("LLM pool without a team yet = %+v", u)
	}
	s.SetLLMUsed(func(context.Context, []string) (map[string]float64, error) { return nil, errors.New("down") })
	if st, err := s.UpsertFromTree(ctx, "p_2", TreeProject{Name: "B", Members: []Member{{Token: "user:a@dhbw.de"}}, LLMMTokens: 20}); err != nil || len(st.Used) != 0 {
		t.Errorf("LiteLLM down: %+v %v", st, err)
	}

	pools, _ := s.LLMPoolsFor(ctx, []string{"user:a@dhbw.de", "group:k"})
	if len(pools) != 2 || pools[0].Name != "A" || pools[0].MTokens != 80 || pools[1].Name != "B" {
		t.Errorf("pools = %+v, want A (80) and B (20); the ended one and the one of others do not count", pools)
	}
	all, _ := s.LLMPools(ctx)
	for _, p := range all {
		if p.Name == "C" && p.MTokens != 0 {
			t.Errorf("ended project's pool = %d, want 0", p.MTokens)
		}
	}
}

// Regression: a manager of a tree project lifted its limits with one PUT (no_workspaces=false, every profile, 0 = unlimited per person, 99 GPUs, more budget, longer end); now only the description and the environment change, and a member of the LLM-only project still cannot start a server.
func TestManagerCannotLiftTreeLimits(t *testing.T) {
	ctx := context.Background()
	s := service()
	end := time.Now().AddDate(0, 1, 0)
	del := end.AddDate(0, 0, 30)
	node := "p_llm-only"
	st, err := s.UpsertFromTree(ctx, node, TreeProject{Name: "Nur LLM", Members: []Member{{Token: "user:m@dhbw.de", Role: RoleManager}, {Token: "group:k", Role: RoleMember}},
		LLMMTokens: 10, EndsAt: &end, DeleteAt: &del, Budgets: []TreeBudget{{Unit: "vcpu_hours", Window: "30d", Amount: 10}}})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := s.Get(ctx, st.Slug)
	if !before.NoWorkspaces {
		t.Fatalf("LLM-only project has workspaces: %+v", before)
	}
	// Admin narrows the preset limits and profiles first, so the manager has something to lift.
	before.Profiles, before.Limits = []string{"cpu"}, Limits{ServersPerPerson: 1, CPUPerPerson: 2, RAMGBPerPerson: 4}
	before.Budgets = append(before.Budgets, Budget{Unit: "vcpu_hours", Window: "7d", Amount: 1, PerPerson: true})
	if before, err = s.Update(ctx, before, "root@dhbw.de", true); err != nil {
		t.Fatal(err)
	}

	later := end.AddDate(5, 0, 0)
	attack := before
	attack.Description = "neu"
	attack.NoWorkspaces = false
	attack.Profiles = []string{"cpu", "gpu", SplitProfile}
	attack.Limits = Limits{GPUsPerPerson: 99}
	attack.GPUClasses, attack.GPUsByClass = []string{"l4-3q"}, map[string]int{"l4-3q": 200}
	attack.Budgets = []Budget{{Unit: "vcpu_hours", Window: "30d", Amount: 1e9, Tree: true}}
	attack.StorageGB, attack.LLMMTokens, attack.EndsAt, attack.DeleteAt = 1000, 1e6, &later, nil
	attack.Members = append(attack.Members, Member{Token: "user:x@dhbw.de", Role: RoleManager})
	out, err := s.Update(ctx, attack, "m@dhbw.de", false)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := s.Get(ctx, st.Slug)
	if out.Description != "neu" || after.Description != "neu" {
		t.Errorf("description not changed: %q", after.Description)
	}
	if !after.NoWorkspaces || !slices.Equal(after.Profiles, before.Profiles) || after.Limits != before.Limits || len(after.GPUClasses) != 0 || len(after.GPUsByClass) != 0 ||
		!slices.Equal(after.Budgets, before.Budgets) || after.StorageGB != 0 || after.LLMMTokens != 10 || !after.EndsAt.Equal(end) || after.DeleteAt == nil || !after.DeleteAt.Equal(del) || len(after.Members) != 2 {
		t.Fatalf("manager changed tree or admin fields:\nbefore %+v\nafter  %+v", before, after)
	}
	// Even an admin cannot give an LLM-only project workspaces; that is the tree's.
	adminTry := after
	adminTry.NoWorkspaces = false
	if after, err = s.Update(ctx, adminTry, "root@dhbw.de", true); err != nil || !after.NoWorkspaces {
		t.Fatalf("admin lifted no_workspaces: %+v %v", after, err)
	}
	err = s.CheckStart(ctx, []string{"user:a@dhbw.de", "group:k"}, StartRequest{Person: "a@dhbw.de", Project: st.Slug, Profile: "cpu", CPU: 1, RAMGB: 1})
	if !strings.Contains(reason(err), "keine Workspaces") {
		t.Fatalf("member started a server in an LLM-only project: %v", err)
	}
}

// The tree grants the server types: exactly those (unknown ones dropped, none = none), and nobody changes them here afterwards; a tree that sends none keeps what the project has.
func TestTreeProfiles(t *testing.T) {
	ctx := context.Background()
	s := service()
	only := []string{"cpu-gpukernel", "gibt-es-nicht"}
	st, err := s.UpsertFromTree(ctx, "p_kurs", TreeProject{Name: "Kurs", Profiles: &only, Workspaces: true})
	if err != nil {
		t.Fatal(err)
	}
	p := mustGet(t, s, st.Slug)
	if !slices.Equal(p.Profiles, []string{"cpu-gpukernel"}) || !p.TreeProfiles {
		t.Fatalf("profiles %v %v", p.Profiles, p.TreeProfiles)
	}
	p.Profiles = []string{"cpu", "gpu", "cpu-gpukernel"}
	if out, err := s.Update(ctx, p, "admin@dhbw.de", true); err != nil || !slices.Equal(out.Profiles, []string{"cpu-gpukernel"}) {
		t.Fatalf("admin changed the tree's profiles: %v %v", out.Profiles, err)
	}
	none := []string{}
	_, _ = s.UpsertFromTree(ctx, "p_kurs", TreeProject{Name: "Kurs", Profiles: &none, Workspaces: true})
	if p := mustGet(t, s, st.Slug); len(p.Profiles) != 0 {
		t.Fatalf("no grant, still %v", p.Profiles)
	}
	// A tree from before server types: the project keeps its own.
	st2, _ := s.UpsertFromTree(ctx, "p_alt", TreeProject{Name: "Alt", Workspaces: true})
	if p := mustGet(t, s, st2.Slug); len(p.Profiles) == 0 || p.TreeProfiles {
		t.Fatalf("old tree: %v %v", p.Profiles, p.TreeProfiles)
	}
}
