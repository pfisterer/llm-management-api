package keys

import (
	"errors"
	"testing"
)

// A workspace key lives in its project's pool; a workspace project without LLM tokens (the LLM in a project of its own) draws from the person's default project.
func TestEnsureWorkspaceKey(t *testing.T) {
	ctx := t.Context()
	svc, f := setup(t)
	p := Person{Subject: "abc", Email: "a@dhbw.de"}

	key, err := svc.EnsureWorkspaceKey(ctx, p, grant(poolA, poolB), "master", "t-b")
	if err != nil || f.keyBy(key)["team_id"] != "t-b" {
		t.Fatalf("own pool: %v %v", err, f.keyBy(key)["team_id"])
	}
	ws, err := svc.EnsureWorkspaceKey(ctx, p, grant(poolA, poolB), "master", "t-ws")
	if err != nil || ws != WorkspaceKey("master", p.UserID(), "t-ws") || f.keyBy(ws)["team_id"] != "t-a" {
		t.Fatalf("default pool: %v %v", err, f.keyBy(ws)["team_id"])
	}
	// The key list names the workspace project of a workspace key.
	u, err := svc.Usage(ctx, p, grant(poolA, poolB))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, k := range u.Keys {
		if k.Workspace == "t-ws" && k.Project == "t-a" {
			found = true
		}
	}
	if !found || u.OwnKeys != 0 {
		t.Fatalf("workspace key not named or counted as own: own=%d %+v", u.OwnKeys, u.Keys)
	}
	if _, _, err := svc.Create(ctx, p, grant(poolA), "workspace-x", ""); !errors.Is(err, ErrNameReserved) {
		t.Fatalf("reserved name accepted: %v", err)
	}
	if _, err := svc.EnsureWorkspaceKey(ctx, p, grant(), "master", "t-ws"); !errors.Is(err, ErrNotEligible) {
		t.Fatalf("without a pool: %v", err)
	}
}
