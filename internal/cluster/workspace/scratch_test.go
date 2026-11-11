package workspace

import (
	"context"
	"strings"
	"testing"
)

// Without a value from the budget a project gets the default (1 GB, or the configured one); the budget's value applies up to the upper bound; the default never exceeds the upper bound.
func TestScratchGB(t *testing.T) {
	s := service()
	if got := s.ScratchGB(Project{}); got != DefaultScratchGB {
		t.Errorf("default = %d, want %d", got, DefaultScratchGB)
	}
	if got := s.ScratchGB(Project{ScratchGB: 7}); got != 7 {
		t.Errorf("budget's 7 = %d", got)
	}
	if got := s.ScratchGB(Project{ScratchGB: DefaultScratchMaxGB + 50}); got != DefaultScratchMaxGB {
		t.Errorf("above the upper bound = %d, want %d", got, DefaultScratchMaxGB)
	}
	s.cfg.ScratchDefaultGB, s.cfg.ScratchMaxGB = 3, 10
	if got := s.ScratchGB(Project{}); got != 3 {
		t.Errorf("configured default = %d, want 3", got)
	}
	if got := s.ScratchGB(Project{ScratchGB: 20}); got != 10 {
		t.Errorf("configured upper bound = %d, want 10", got)
	}
	s.cfg.ScratchDefaultGB = 50
	if got := s.ScratchGB(Project{}); got != 10 {
		t.Errorf("a default above the upper bound = %d, want 10", got)
	}
}

// The tree owns the value: an upsert takes it (out of range is cleaned, never a failed sync), a manager or admin cannot change it, and the hub's projects and the person's list carry the effective value.
func TestScratchFromTree(t *testing.T) {
	ctx := context.Background()
	s := service()
	s.cfg.ScratchMaxGB = 50
	node := "p_scratch"
	tp := TreeProject{Name: "Kurs", Workspaces: true, Members: []Member{{Token: "user:m@dhbw.de", Role: RoleManager}}, Budgets: []TreeBudget{{Unit: "vcpu_hours", Window: "30d", Amount: 10}}, WorkspaceScratchGB: 5}
	st, err := s.UpsertFromTree(ctx, node, tp)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := s.Get(ctx, st.Slug)
	if p.ScratchGB != 5 || s.ScratchGB(p) != 5 {
		t.Fatalf("scratch = %d", p.ScratchGB)
	}
	for _, c := range []struct{ sent, want int }{{500, 50}, {-3, 0}, {0, 0}} {
		tp.WorkspaceScratchGB = c.sent
		if _, err := s.UpsertFromTree(ctx, node, tp); err != nil {
			t.Fatalf("sent %d: %v", c.sent, err)
		}
		if p, _ = s.Get(ctx, st.Slug); p.ScratchGB != c.want {
			t.Errorf("sent %d: stored %d, want %d", c.sent, p.ScratchGB, c.want)
		}
	}
	tp.WorkspaceScratchGB = 5
	if _, err := s.UpsertFromTree(ctx, node, tp); err != nil {
		t.Fatal(err)
	}
	p, _ = s.Get(ctx, st.Slug)
	for _, admin := range []bool{false, true} {
		try := p
		try.ScratchGB = 40
		out, err := s.Update(ctx, try, "m@dhbw.de", admin)
		if err != nil {
			t.Fatal(err)
		}
		if out.ScratchGB != 5 {
			t.Errorf("admin=%v changed the tree's value: %d", admin, out.ScratchGB)
		}
	}
	mine, err := s.ForPerson(ctx, []string{"user:m@dhbw.de"}, "", false)
	if err != nil || len(mine) != 1 || mine[0].ScratchEffectiveGB != 5 {
		t.Fatalf("mine = %+v, %v", mine, err)
	}
}

// For a project without the tree its admins set the value, within 0 (the default) and the upper bound; a stored value above a lowered upper bound does not block other edits.
func TestScratchAdminProject(t *testing.T) {
	ctx := context.Background()
	s := service()
	p := Project{Slug: "frei", Name: "Frei", Profiles: []string{"cpu"}}
	if err := s.store.Create(ctx, &p); err != nil {
		t.Fatal(err)
	}
	p.ScratchGB = 30
	out, err := s.Update(ctx, p, "root@dhbw.de", true)
	if err != nil || out.ScratchGB != 30 {
		t.Fatalf("admin sets 30: %+v %v", out.ScratchGB, err)
	}
	p = out
	p.ScratchGB = DefaultScratchMaxGB + 1
	if _, err := s.Update(ctx, p, "root@dhbw.de", true); err == nil || !strings.Contains(reason(err), "Lokaler Speicher") {
		t.Errorf("above the upper bound: %v", err)
	}
	p.ScratchGB = -1
	if _, err := s.Update(ctx, p, "root@dhbw.de", true); err == nil {
		t.Error("negative should be refused")
	}
	s.cfg.ScratchMaxGB = 10
	p = out
	p.Description = "neu"
	if _, err := s.Update(ctx, p, "root@dhbw.de", true); err != nil {
		t.Errorf("an unchanged value above a lowered upper bound should not block: %v", err)
	}
	if got := s.ScratchGB(p); got != 10 {
		t.Errorf("capped where used: %d", got)
	}
}
