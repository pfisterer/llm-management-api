package access

import (
	"context"
	"errors"
	"testing"
)

func TestDecide(t *testing.T) {
	rules := []Rule{
		{Token: "group:mitarbeitende", Role: RoleUser, Tier: "staff"},
		{Token: "group:wwi23seb", Role: RoleUser, Tier: "student"},
		{Token: "group:it", Role: RoleFleetAdmin, Tier: "staff"},
		{Token: "user:a@dhbw.de", Role: RoleUser, Tier: "special"},
		{Token: "user:root@dhbw.de", Role: RoleAdmin, Tier: "staff"},
	}
	cases := []struct {
		name   string
		tokens []string
		want   Decision
	}{
		{"no match", []string{"user:x@dhbw.de", "group:other"}, Decision{}},
		{"group", []string{"user:x@dhbw.de", "group:wwi23seb"}, Decision{Role: RoleUser, Tier: "student", MatchedToken: "group:wwi23seb"}},
		{"higher role wins", []string{"group:wwi23seb", "group:it"}, Decision{Role: RoleFleetAdmin, Tier: "staff", MatchedToken: "group:it"}},
		{"user rule beats group rule of same role", []string{"user:a@dhbw.de", "group:mitarbeitende"}, Decision{Role: RoleUser, Tier: "special", MatchedToken: "user:a@dhbw.de"}},
		{"ties broken by token, not order", []string{"group:wwi23seb", "group:mitarbeitende"}, Decision{Role: RoleUser, Tier: "staff", MatchedToken: "group:mitarbeitende"}},
		{"case-insensitive tokens", []string{"USER:Root@DHBW.de"}, Decision{Role: RoleAdmin, Tier: "staff", MatchedToken: "user:root@dhbw.de"}},
	}
	for _, c := range cases {
		if got := Decide(rules, c.tokens); got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
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
	if !RoleAdmin.AtLeast(RoleFleetAdmin) || RoleUser.AtLeast(RoleFleetAdmin) || RoleNone.AtLeast(RoleNone) {
		t.Fatal("role ordering broken")
	}
}

type fixedTokens []string

func (f fixedTokens) UserTokens(context.Context, string) []string { return f }

func TestServiceBootstrapAndValidation(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(NewMemoryStore(), fixedTokens{"user:boss@dhbw.de"}, Config{
		Tiers: []string{"student", "staff"}, BootstrapAdmins: []string{"Boss@dhbw.de"}, BootstrapAdminTier: "staff",
	})
	if err != nil {
		t.Fatal(err)
	}
	d, _, err := svc.Decide(ctx, "boss@dhbw.de")
	if err != nil || d.Role != RoleAdmin {
		t.Fatalf("bootstrap admin: %+v %v", d, err)
	}
	if _, err := svc.Create(ctx, Rule{Token: "user:boss@dhbw.de", Role: RoleUser, Tier: "staff"}, "x"); !errors.Is(err, ErrBootstrapRule) {
		t.Fatalf("overriding a bootstrap rule must fail, got %v", err)
	}
	if _, err := svc.Create(ctx, Rule{Token: "group:x", Role: RoleUser, Tier: "gold"}, "x"); !errors.Is(err, ErrUnknownTier) {
		t.Fatalf("unknown tier must fail, got %v", err)
	}
	r, err := svc.Create(ctx, Rule{Token: "Group:X", Role: RoleUser, Tier: "student"}, "boss@dhbw.de")
	if err != nil || r.Token != "group:x" || r.UpdatedBy != "boss@dhbw.de" {
		t.Fatalf("create: %+v %v", r, err)
	}
	if _, err := svc.Create(ctx, Rule{Token: "group:x", Role: RoleAdmin, Tier: "staff"}, "x"); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate must fail, got %v", err)
	}
	if _, err := NewService(NewMemoryStore(), fixedTokens{}, Config{Tiers: []string{"staff"}, BootstrapAdmins: []string{"a@b.de"}, BootstrapAdminTier: "nope"}); !errors.Is(err, ErrUnknownTier) {
		t.Fatalf("unknown bootstrap tier must fail, got %v", err)
	}
}

func TestDecideGPUTierFromAnyRule(t *testing.T) {
	rules := []Rule{
		{Token: "user:a@dhbw.de", Role: RoleAdmin, Tier: "staff"},
		{Token: "group:kurs-ki", Role: RoleUser, Tier: "student", GPUTier: "student-gpu"},
	}
	d := Decide(rules, []string{"user:a@dhbw.de", "group:kurs-ki"})
	if d.Role != RoleAdmin || d.Tier != "staff" || d.GPUTier != "student-gpu" || d.GPUTierMatchedToken != "group:kurs-ki" {
		t.Fatalf("got %+v", d)
	}
	if d := Decide(rules[:1], []string{"user:a@dhbw.de"}); d.GPUTier != "" {
		t.Fatalf("no GPU rule, got %+v", d)
	}
}
