package keys

import (
	"context"
	"fmt"
)

// ChatKey is the LiteLLM key the chat uses on a person's behalf. It is DERIVED
// (HMAC of the user id under the master key), so nobody has to store it: the
// service can recompute it for every request. Bit-for-bit the derivation of the
// former Node broker, so chat keys created there remain valid. A new master key
// yields new chat keys; the stale ones are removed on the next creation.
func ChatKey(masterKey, userID string) string {
	return derivedKey(masterKey, "sk-lc-", "librechat:"+userID)
}

// EnsureChatKey makes sure the person exists in LiteLLM with their quota (see userQuota) and has their chat key in the pool of their default project, with that project's key budget, and returns it. Quotas are written only when they differ. A chat key in another pool (the default changed, or the project ended) is moved, not made anew.
func (s *Service) EnsureChatKey(ctx context.Context, p Person, g Grant, masterKey string) (string, error) {
	uid := p.UserID()
	key := ChatKey(masterKey, uid)
	info, existing, err := s.reconcile(ctx, p, g)
	if err != nil {
		return "", err
	}
	pool, ok := g.defaultPool(info)
	if !ok {
		return "", ErrNotEligible
	}
	if found, err := s.keepDerivedKey(ctx, uid, key, pool, "chat"); err != nil {
		return "", err
	} else if found {
		return key, nil
	}
	// Orphaned chat keys (from an earlier master key) would otherwise show up
	// in the person's key list.
	var stale []string
	for _, k := range existing {
		if k.Alias != nil && isChatAlias(*k.Alias) {
			stale = append(stale, k.Token)
		}
	}
	if len(stale) > 0 {
		if err := s.lite.DeleteKeys(ctx, stale...); err != nil {
			return "", fmt.Errorf("remove stale chat keys: %w", err)
		}
	}
	meta := map[string]any{"created_by": "llm-management-api", "purpose": "chat"}
	if err := s.createDerivedKey(ctx, uid, key, pool, chatAlias, meta); err != nil {
		return "", err
	}
	return key, nil
}
