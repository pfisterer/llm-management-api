package keys

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// ChatKey is the LiteLLM key the chat uses on a person's behalf. It is DERIVED
// (HMAC of the user id under the master key), so nobody has to store it: the
// service can recompute it for every request. Bit-for-bit the derivation of the
// former Node broker, so chat keys created there remain valid. A new master key
// yields new chat keys; the stale ones are removed on the next creation.
func ChatKey(masterKey, userID string) string {
	mac := hmac.New(sha256.New, []byte(masterKey))
	mac.Write([]byte("librechat:" + userID))
	return "sk-lc-" + hex.EncodeToString(mac.Sum(nil))[:48]
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
	cur, found, err := s.lite.KeyInfo(ctx, key)
	if err != nil {
		return "", err
	}
	if found {
		if cur.Team() != pool.Slug {
			if err := s.moveKey(ctx, uid, key, pool); err != nil {
				return "", fmt.Errorf("move chat key: %w", err)
			}
		}
		cur.Token = key // /key/info may answer with the hashed token
		if _, err := s.applyKeyBudget(ctx, cur, pool); err != nil {
			return "", fmt.Errorf("chat key budget: %w", err)
		}
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
	if _, err := s.ensureTeam(ctx, pool); err != nil {
		return "", err
	}
	meta := map[string]any{"created_by": "llm-management-api", "purpose": "chat"}
	if err := s.lite.GenerateKeyWithValue(ctx, uid, pool.Slug, ownAlias(uid, chatAlias), key, s.keyBudget(pool), meta); err != nil {
		return "", err
	}
	return key, nil
}
