package access

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// TokenResolver returns the role-provider tokens of a person. Implementations
// must always include "user:<email>", even when the role-provider is down, so
// that rules written for a person keep working (degraded, never wider).
type TokenResolver interface {
	UserTokens(ctx context.Context, email string) []string
}

var ErrUnknownTier = errors.New("unknown tier")
var ErrUnknownGPUTier = errors.New("unknown GPU tier")
var ErrBootstrapRule = errors.New("rules from the configuration cannot be changed here")

// Service combines the stored access list with the bootstrap admins from the
// configuration and resolves a person's role.
type Service struct {
	store     Store
	resolver  TokenResolver
	tiers     map[string]struct{}
	gpuTiers  map[string]struct{}
	bootstrap []Rule

	cacheTTL time.Duration
	mu       sync.Mutex
	cache    map[string]cachedTokens
}

type cachedTokens struct {
	tokens  []string
	expires time.Time
}

// Config for NewService.
type Config struct {
	// Tier names that exist (from the quota configuration). Rules may only use these.
	Tiers []string
	// E-mail addresses that are admins regardless of the stored list.
	BootstrapAdmins []string
	// Tier for bootstrap admins.
	BootstrapAdminTier string
	// GPU tier names that exist (GPU part, may be empty). Rules may only use these.
	GPUTiers []string
	// GPU tier for bootstrap admins (empty: none).
	BootstrapAdminGPUTier string
	// How long resolved role-provider tokens are reused. Short: a membership
	// change should take effect within a minute, not at the next restart.
	TokenCacheTTL time.Duration
}

func NewService(store Store, resolver TokenResolver, cfg Config) (*Service, error) {
	tiers := map[string]struct{}{}
	for _, t := range cfg.Tiers {
		tiers[strings.TrimSpace(t)] = struct{}{}
	}
	gpuTiers := map[string]struct{}{}
	for _, t := range cfg.GPUTiers {
		gpuTiers[strings.TrimSpace(t)] = struct{}{}
	}
	s := &Service{store: store, resolver: resolver, tiers: tiers, gpuTiers: gpuTiers, cacheTTL: cfg.TokenCacheTTL, cache: map[string]cachedTokens{}}
	if len(cfg.BootstrapAdmins) > 0 {
		if _, ok := tiers[cfg.BootstrapAdminTier]; !ok {
			return nil, fmt.Errorf("bootstrap admin tier %q: %w", cfg.BootstrapAdminTier, ErrUnknownTier)
		}
		if _, ok := gpuTiers[cfg.BootstrapAdminGPUTier]; cfg.BootstrapAdminGPUTier != "" && !ok {
			return nil, fmt.Errorf("bootstrap admin GPU tier %q: %w", cfg.BootstrapAdminGPUTier, ErrUnknownGPUTier)
		}
	}
	for _, email := range cfg.BootstrapAdmins {
		tok, err := NormaliseToken("user:" + email)
		if err != nil {
			return nil, fmt.Errorf("bootstrap admin %q: %w", email, err)
		}
		s.bootstrap = append(s.bootstrap, Rule{Token: tok, Role: RoleAdmin, Tier: cfg.BootstrapAdminTier, GPUTier: cfg.BootstrapAdminGPUTier,
			Comment: "aus der Konfiguration", Bootstrap: true})
	}
	return s, nil
}

// Tiers lists the tier names rules may use.
func (s *Service) Tiers() []string {
	out := make([]string, 0, len(s.tiers))
	for t := range s.tiers {
		out = append(out, t)
	}
	return out
}

// GPUTiers lists the GPU tier names rules may use (empty without the GPU part).
func (s *Service) GPUTiers() []string {
	out := make([]string, 0, len(s.gpuTiers))
	for t := range s.gpuTiers {
		out = append(out, t)
	}
	return out
}

// Rules returns the bootstrap rules followed by the stored ones.
func (s *Service) Rules(ctx context.Context) ([]Rule, error) {
	stored, err := s.store.List(ctx)
	if err != nil {
		return nil, err
	}
	return append(append([]Rule{}, s.bootstrap...), stored...), nil
}

// Decide resolves the person's tokens and applies the access list.
func (s *Service) Decide(ctx context.Context, email string) (Decision, []string, error) {
	tokens := s.tokens(ctx, email)
	rules, err := s.Rules(ctx)
	if err != nil {
		return Decision{}, tokens, err
	}
	return Decide(rules, tokens), tokens, nil
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

func (s *Service) validate(r *Rule) error {
	tok, err := NormaliseToken(r.Token)
	if err != nil {
		return err
	}
	r.Token = tok
	if _, err := ParseRole(string(r.Role)); err != nil {
		return err
	}
	if _, ok := s.tiers[r.Tier]; !ok {
		return fmt.Errorf("%w %q", ErrUnknownTier, r.Tier)
	}
	r.GPUTier = strings.TrimSpace(r.GPUTier)
	if _, ok := s.gpuTiers[r.GPUTier]; r.GPUTier != "" && !ok {
		return fmt.Errorf("%w %q", ErrUnknownGPUTier, r.GPUTier)
	}
	for _, b := range s.bootstrap {
		if b.Token == r.Token {
			return ErrBootstrapRule
		}
	}
	return nil
}

// Create adds a rule after validation.
func (s *Service) Create(ctx context.Context, r Rule, by string) (Rule, error) {
	if err := s.validate(&r); err != nil {
		return Rule{}, err
	}
	r.UpdatedBy = by
	return s.store.Create(ctx, r)
}

// Update replaces a stored rule after validation.
func (s *Service) Update(ctx context.Context, r Rule, by string) (Rule, error) {
	if err := s.validate(&r); err != nil {
		return Rule{}, err
	}
	r.UpdatedBy = by
	return s.store.Update(ctx, r)
}

// Delete removes a stored rule.
func (s *Service) Delete(ctx context.Context, id uint) error { return s.store.Delete(ctx, id) }
