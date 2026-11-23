package access

import (
	"context"
	"testing"
	"time"
)

func TestRoleOf(t *testing.T) {
	roles, err := NewRoles([]string{"user:Root@DHBW.de", "group:it-leitung"}, []string{"group:it", " "})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		tokens []string
		want   Role
	}{
		{"nothing configured for them", []string{"user:x@dhbw.de", "group:other"}, RoleUser},
		{"no tokens at all", nil, RoleUser},
		{"admin by user token, case-insensitive", []string{"USER:root@dhbw.de"}, RoleAdmin},
		{"admin by group token", []string{"user:x@dhbw.de", "group:it-leitung"}, RoleAdmin},
		{"fleet-admin by group token", []string{"user:x@dhbw.de", "group:it"}, RoleFleetAdmin},
		{"admin wins over fleet-admin", []string{"group:it", "group:it-leitung"}, RoleAdmin},
	}
	for _, c := range cases {
		if got := roles.RoleOf(c.tokens); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	if _, err := NewRoles([]string{"root@dhbw.de"}, nil); err == nil {
		t.Error("a token without kind must be refused")
	}
	if _, err := NewRoles(nil, []string{"role:x"}); err == nil {
		t.Error("an unknown kind must be refused")
	}
}

func TestNormaliseToken(t *testing.T) {
	good := map[string]string{" User:A@DHBW.de ": "user:a@dhbw.de", "group:WWI23SEB#dozent": "group:wwi23seb#dozent"}
	for in, want := range good {
		if got, err := NormaliseToken(in); err != nil || got != want {
			t.Errorf("%q: got %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "a@dhbw.de", "user:nomail", "group:", "role:x", "group:a b"} {
		if _, err := NormaliseToken(bad); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}

func TestRoleAtLeast(t *testing.T) {
	if !RoleAdmin.AtLeast(RoleFleetAdmin) || RoleUser.AtLeast(RoleFleetAdmin) || !RoleFleetAdmin.AtLeast(RoleUser) || RoleFleetAdmin.AtLeast(RoleAdmin) {
		t.Fatal("role ordering broken")
	}
}

type groupResolver map[string][]string

func (g groupResolver) UserTokens(_ context.Context, email string) ([]string, bool) {
	return append([]string{"user:" + email}, g[email]...), true
}

// flakyResolver answers degraded (user token only) while down is set, completely otherwise.
type flakyResolver struct {
	down   bool
	groups []string
	calls  int
}

func (f *flakyResolver) UserTokens(_ context.Context, email string) ([]string, bool) {
	f.calls++
	if f.down {
		return []string{"user:" + email}, false
	}
	return append([]string{"user:" + email}, f.groups...), true
}

// A degraded answer is not cached: once the role-provider is back, the next request has the groups again; a complete answer is.
func TestServiceDoesNotCacheDegradedTokens(t *testing.T) {
	roles, err := NewRoles([]string{"group:root_uni"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := &flakyResolver{down: true, groups: []string{"group:root_uni"}}
	svc := NewService(r, roles, time.Hour)
	if p := svc.Who(context.Background(), "boss@dhbw.de"); p.Role != RoleUser {
		t.Fatalf("while down: %+v, want user", p)
	}
	r.down = false
	if p := svc.Who(context.Background(), "boss@dhbw.de"); p.Role != RoleAdmin {
		t.Fatalf("after recovery: %+v, want admin", p)
	}
	r.down = true
	if p := svc.Who(context.Background(), "boss@dhbw.de"); p.Role != RoleAdmin || r.calls != 2 {
		t.Fatalf("complete answer not served from cache: %+v, calls %d", p, r.calls)
	}
}

// The role comes from the person's groups as the role provider resolves them.
func TestServiceWho(t *testing.T) {
	roles, err := NewRoles([]string{"group:root_uni"}, []string{"group:it"})
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(groupResolver{"boss@dhbw.de": {"group:root_uni"}, "it@dhbw.de": {"group:it"}}, roles, 0)
	for email, want := range map[string]Role{"boss@dhbw.de": RoleAdmin, "it@dhbw.de": RoleFleetAdmin, "a@dhbw.de": RoleUser} {
		if p := svc.Who(context.Background(), email); p.Role != want || p.Tokens[0] != "user:"+email {
			t.Errorf("%s: %+v, want %q", email, p, want)
		}
	}
}
