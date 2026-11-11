package keys

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// derivedKey is a LiteLLM key that nobody stores: prefix plus the first 48 hex digits of the HMAC-SHA256 of context under the master key. The service recomputes it whenever it needs it. Bit for bit what ChatKey (the former Node broker's derivation) and WorkspaceKey have always computed (TestDerivedKeysGolden): a change would orphan every key in LiteLLM and in running notebooks.
func derivedKey(masterKey, prefix, context string) string {
	mac := hmac.New(sha256.New, []byte(masterKey))
	mac.Write([]byte(context))
	return prefix + hex.EncodeToString(mac.Sum(nil))[:48]
}

// keepDerivedKey brings a derived key LiteLLM already has into pool (moving it when it is in another one) and gives it the pool's key budget; false when LiteLLM has no such key. kind names the key in errors ("chat", "workspace").
func (s *Service) keepDerivedKey(ctx context.Context, uid, key string, pool Pool, kind string) (bool, error) {
	cur, found, err := s.lite.KeyInfo(ctx, key)
	if err != nil || !found {
		return false, err
	}
	if cur.Team() != pool.Slug {
		if err := s.moveKey(ctx, uid, key, pool); err != nil {
			return true, fmt.Errorf("move %s key: %w", kind, err)
		}
	}
	cur.Token = key // /key/info may answer with the hashed token
	if _, err := s.applyKeyBudget(ctx, cur, pool); err != nil {
		return true, fmt.Errorf("%s key budget: %w", kind, err)
	}
	return true, nil
}

// createDerivedKey makes the derived key in pool (its team first, if need be) with the pool's key budget, the person's alias and meta.
func (s *Service) createDerivedKey(ctx context.Context, uid, key string, pool Pool, alias string, meta map[string]any) error {
	if _, err := s.ensureTeam(ctx, pool); err != nil {
		return err
	}
	return s.lite.GenerateKeyWithValue(ctx, uid, pool.Slug, ownAlias(uid, alias), key, s.keyBudget(pool), meta)
}
