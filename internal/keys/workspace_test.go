package keys

import (
	"errors"
	"testing"
)

// A workspace key lives only in its own project's pool: a workspace project without LLM tokens (the LLM in a project of its own) gets no key, and never one from the person's default project.
func TestEnsureWorkspaceKey(t *testing.T) {
	ctx := t.Context()
	svc, f := setup(t)
	p := Person{Subject: "abc", Email: "a@dhbw.de"}

	key, err := svc.EnsureWorkspaceKey(ctx, p, grant(poolA, poolB), "master", "t-b")
	if err != nil || f.keyBy(key)["team_id"] != "t-b" {
		t.Fatalf("own pool: %v %v", err, f.keyBy(key)["team_id"])
	}
	// The key list names the workspace project of a workspace key.
	u, err := svc.Usage(ctx, p, grant(poolA, poolB))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, k := range u.Keys {
		if k.Workspace == "t-b" && k.Project == "t-b" {
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

// Regression: a project without LLM tokens drew its workspace key from the member's default project, so a project image (chosen by the project's managers) got a key on another project's budget. Now there is no key, and one made that way before is deleted.
func TestWorkspaceKeyNotFromDefaultPool(t *testing.T) {
	ctx := t.Context()
	svc, f := setup(t)
	p := Person{Subject: "abc", Email: "a@dhbw.de"}
	if _, err := svc.EnsureWorkspaceKey(ctx, p, grant(poolA, poolB), "master", "t-ws"); !errors.Is(err, ErrNotEligible) {
		t.Fatalf("project without tokens: %v, want no key", err)
	}
	if len(f.keys) != 0 {
		t.Fatalf("keys made: %v", f.keys)
	}
	// A key from before, in the default pool: deleted on the next start, and by the sync.
	old := WorkspaceKey("master", p.UserID(), "t-ws")
	f.keys["ws-old"] = map[string]any{"token": "ws-old", "key": old, "user_id": "kc-abc", "team_id": "t-a", "key_alias": ownAlias("kc-abc", workspaceAlias("t-ws"))}
	if _, err := svc.EnsureWorkspaceKey(ctx, p, grant(poolA, poolB), "master", "t-ws"); !errors.Is(err, ErrNotEligible) || f.keyBy(old) != nil {
		t.Fatalf("old key kept: %v %v", err, f.keyBy(old))
	}
	f.keys["ws-old"] = map[string]any{"token": "ws-old", "key": old, "user_id": "kc-abc", "team_id": "t-a", "key_alias": ownAlias("kc-abc", workspaceAlias("t-ws"))}
	// A workspace key of t-b sitting in t-a goes back to t-b; the one without a pool goes.
	f.keys["ws-b"] = map[string]any{"token": "ws-b", "user_id": "kc-abc", "team_id": "t-a", "key_alias": ownAlias("kc-abc", workspaceAlias("t-b"))}
	_, _, _ = svc.Create(ctx, p, grant(poolA, poolB), "laptop", "t-a")
	info, _, _, _ := svc.lite.GetUser(ctx, "kc-abc")
	if _, err := svc.SyncKeys(ctx, info, grant(poolA, poolB), nil); err != nil {
		t.Fatal(err)
	}
	if f.keys["ws-old"] != nil || f.team("ws-b") != "t-b" || len(f.keys) != 2 {
		t.Fatalf("after the sync: %v", f.keys)
	}
}
