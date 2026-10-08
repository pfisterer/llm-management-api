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

// EnsureChatKey makes sure the person exists in LiteLLM with the tier's quota
// and has their chat key, and returns it. Quotas follow reconcile's rule: written
// for a new user or a changed tier only.
func (s *Service) EnsureChatKey(ctx context.Context, p Person, tier Tier, masterKey string) (string, error) {
	uid := p.UserID()
	key := ChatKey(masterKey, uid)
	_, existing, err := s.reconcile(ctx, p, tier)
	if err != nil {
		return "", err
	}
	ok, err := s.lite.KeyExists(ctx, key)
	if err != nil {
		return "", err
	}
	if ok {
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
	meta := map[string]any{"created_by": "llm-management-api", "tier": tier.Name, "purpose": "chat"}
	if err := s.lite.GenerateKeyWithValue(ctx, uid, ownAlias(uid, chatAlias), key, tier.keyQuota(), meta); err != nil {
		return "", err
	}
	return key, nil
}
