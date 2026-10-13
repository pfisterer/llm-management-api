// Package access tells who a person is to this service: their role-provider tokens ("user:a@dhbw.de", "group:wwi23seb", "group:dhbw-ma#dozent") and the role those tokens give them (user, fleet-admin, admin). The service keeps no rights of its own: the roles come from the configuration (ADMIN_TOKENS, FLEET_ADMIN_TOKENS), and whether somebody may use the LLM or workspaces at all, and with which limits, comes from the projects of the budget tree.
package access

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Role is what a person may do. Ordered: a higher role includes the lower ones.
type Role string

const (
	RoleUser       Role = "user"
	RoleFleetAdmin Role = "fleet-admin"
	RoleAdmin      Role = "admin"
)

func (r Role) rank() int {
	switch r {
	case RoleFleetAdmin:
		return 2
	case RoleAdmin:
		return 3
	default:
		return 1
	}
}

// AtLeast reports whether r includes the rights of min.
func (r Role) AtLeast(min Role) bool { return r.rank() >= min.rank() }

var ErrInvalidToken = errors.New("token must look like user:<email> or group:<id>")

// NormaliseToken trims and lowercases a token and checks its shape. Group ids and e-mail addresses are case-insensitive in the role-provider as well.
func NormaliseToken(raw string) (string, error) {
	t := strings.ToLower(strings.TrimSpace(raw))
	kind, rest, ok := strings.Cut(t, ":")
	if !ok || rest == "" || strings.ContainsAny(rest, " \t") {
		return "", ErrInvalidToken
	}
	switch kind {
	case "user":
		if !strings.Contains(rest, "@") {
			return "", ErrInvalidToken
		}
	case "group":
	default:
		return "", ErrInvalidToken
	}
	return t, nil
}

// Roles maps role-provider tokens to the two roles above user. Admin includes fleet-admin; everybody else is a plain user.
type Roles struct {
	admin, fleetAdmin map[string]struct{}
}

// NewRoles checks and normalises the configured tokens.
func NewRoles(adminTokens, fleetAdminTokens []string) (Roles, error) {
	set := func(name string, raw []string) (map[string]struct{}, error) {
		out := map[string]struct{}{}
		for _, r := range raw {
			if strings.TrimSpace(r) == "" {
				continue
			}
			t, err := NormaliseToken(r)
			if err != nil {
				return nil, fmt.Errorf("%s %q: %w", name, r, err)
			}
			out[t] = struct{}{}
		}
		return out, nil
	}
	admin, err := set("ADMIN_TOKENS", adminTokens)
	if err != nil {
		return Roles{}, err
	}
	fleet, err := set("FLEET_ADMIN_TOKENS", fleetAdminTokens)
	if err != nil {
		return Roles{}, err
	}
	return Roles{admin: admin, fleetAdmin: fleet}, nil
}

// RoleOf is the highest role any of the tokens gives.
func (r Roles) RoleOf(tokens []string) Role {
	role := RoleUser
	for _, raw := range tokens {
		t := strings.ToLower(strings.TrimSpace(raw))
		if _, ok := r.admin[t]; ok {
			return RoleAdmin
		}
		if _, ok := r.fleetAdmin[t]; ok {
			role = RoleFleetAdmin
		}
	}
	return role
}

// TokenResolver returns the role-provider tokens of a person. Implementations must always include "user:<email>", even when the role-provider is down, so that what is written for a person keeps working (degraded, never wider).
type TokenResolver interface {
	UserTokens(ctx context.Context, email string) []string
}

// Person is who somebody is to this service.
type Person struct {
	Role   Role
	Tokens []string
}

// Service resolves a person's tokens (cached briefly) and their role.
type Service struct {
	resolver TokenResolver
	roles    Roles

	cacheTTL time.Duration
	mu       sync.Mutex
	cache    map[string]cachedTokens
}

type cachedTokens struct {
	tokens  []string
	expires time.Time
}

// NewService resolves tokens through resolver and reuses them for cacheTTL (short: a membership change should take effect within a minute, not at the next restart; 0 = no cache).
func NewService(resolver TokenResolver, roles Roles, cacheTTL time.Duration) *Service {
	return &Service{resolver: resolver, roles: roles, cacheTTL: cacheTTL, cache: map[string]cachedTokens{}}
}

// Who resolves the person's tokens and their role.
func (s *Service) Who(ctx context.Context, email string) Person {
	tokens := s.tokens(ctx, email)
	return Person{Role: s.roles.RoleOf(tokens), Tokens: tokens}
}

func (s *Service) tokens(ctx context.Context, email string) []string {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return nil
	}
	now := time.Now()
	s.mu.Lock()
	if c, ok := s.cache[email]; ok && now.Before(c.expires) {
		s.mu.Unlock()
		return c.tokens
	}
	s.mu.Unlock()

	tokens := s.resolver.UserTokens(ctx, email)
	if s.cacheTTL > 0 {
		s.mu.Lock()
		s.cache[email] = cachedTokens{tokens: tokens, expires: now.Add(s.cacheTTL)}
		s.mu.Unlock()
	}
	return tokens
}
