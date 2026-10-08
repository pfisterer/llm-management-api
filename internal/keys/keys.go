// Package keys manages a person's LiteLLM user, API keys and usage.
//
// Carried over from the Node key broker, including the lessons learned there:
//   - Quotas are written when the user is created or changes tier — never on
//     every call, which would silently revert adjustments made in the LiteLLM
//     admin UI. Spend is never touched.
//   - Key aliases are unique across ALL users in LiteLLM, so every alias gets a
//     short, stable tag of its owner ("laptop@3f9a12c4"); it is shown without.
//   - The chat key ("librechat@…") is created by the chat exchange, not by the
//     person, and does not count against the per-person limit.
package keys

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"maps"
	"regexp"
	"strings"

	"github.com/pfisterer/llm-management-api/internal/litellm"
)

// Tier is the quota class a person's access rule assigns.
type Tier struct {
	Name               string
	KeyBudget          float64
	KeyBudgetDuration  string
	UserBudget         float64
	UserBudgetDuration string
	RPM                int
	TPM                int
	Models             []string
}

// Person is who the keys belong to.
type Person struct {
	Subject string // Keycloak sub; the LiteLLM user id is "kc-<sub>"
	Email   string
}

// UserID is the LiteLLM user id. The Keycloak sub, not the e-mail: e-mails
// change with names, subs do not — and it is the same id the old portal and the
// chat exchange use, so existing keys and spend stay with the person.
func (p Person) UserID() string { return "kc-" + p.Subject }

var (
	ErrNameRequired = errors.New("name required")
	ErrTooManyKeys  = errors.New("too many keys")
	ErrDuplicate    = errors.New("name already used")
	ErrNotFound     = errors.New("key not found")
)

const chatAlias = "librechat"

var tagSuffix = regexp.MustCompile(`@[0-9a-f]{8}$`)
var unsafeChars = regexp.MustCompile(`[^\w.-]`)

func aliasTag(userID string) string {
	h := sha256.Sum256([]byte(userID))
	return hex.EncodeToString(h[:])[:8]
}

func ownAlias(userID, name string) string { return name + "@" + aliasTag(userID) }

// shownName strips the owner tag for display.
func shownName(alias string) string { return tagSuffix.ReplaceAllString(alias, "") }

// isChatAlias also matches the very first chat key, created before tagging.
func isChatAlias(alias string) bool {
	return alias == chatAlias || strings.HasPrefix(alias, chatAlias+"@")
}

// KeyView is one key as the UI shows it.
type KeyView struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Chat           bool     `json:"chat"`
	Spend          float64  `json:"spend"`
	Budget         *float64 `json:"budget"`
	BudgetDuration *string  `json:"budget_duration"`
	ResetAt        *string  `json:"reset_at"`
}

// Usage is the person's quota state.
type Usage struct {
	Spend          float64   `json:"spend"`
	Budget         *float64  `json:"budget"`
	BudgetDuration *string   `json:"budget_duration"`
	ResetAt        *string   `json:"reset_at"`
	RPM            *int      `json:"rpm"`
	TPM            *int      `json:"tpm"`
	Models         []string  `json:"models"`
	Keys           []KeyView `json:"keys"`
	MaxKeys        int       `json:"max_keys"`
	OwnKeys        int       `json:"own_keys"`
}

// Service implements the key operations on top of LiteLLM.
type Service struct {
	lite    *litellm.Client
	maxKeys int
}

func NewService(lite *litellm.Client, maxKeys int) *Service {
	return &Service{lite: lite, maxKeys: maxKeys}
}

func (t Tier) userQuota() litellm.Quota {
	return litellm.Quota{MaxBudget: t.UserBudget, BudgetDuration: t.UserBudgetDuration, TPMLimit: t.TPM, RPMLimit: t.RPM, Models: t.Models}
}

func (t Tier) keyQuota() litellm.Quota {
	return litellm.Quota{MaxBudget: t.KeyBudget, BudgetDuration: t.KeyBudgetDuration, TPMLimit: t.TPM, RPMLimit: t.RPM, Models: t.Models}
}

// reconcile creates the LiteLLM user or brings their quota to the tier — the
// latter only when the tier changed or no budget is set yet.
func (s *Service) reconcile(ctx context.Context, p Person, tier Tier) (litellm.UserInfo, []litellm.Key, error) {
	uid := p.UserID()
	info, keys, found, err := s.lite.GetUser(ctx, uid)
	if err != nil {
		return litellm.UserInfo{}, nil, err
	}
	if !found {
		meta := map[string]any{"tier": tier.Name, "source": "llm-management-api"}
		if err := s.lite.NewUser(ctx, uid, p.Email, tier.userQuota(), meta); err != nil {
			return litellm.UserInfo{}, nil, err
		}
		info, keys, _, err = s.lite.GetUser(ctx, uid)
		return info, keys, err
	}
	meta := map[string]any{}
	maps.Copy(meta, info.Metadata)
	previous, _ := meta["tier"].(string)
	changed := previous != tier.Name || info.MaxBudget == nil
	meta["tier"], meta["source"] = tier.Name, "llm-management-api"
	if !changed && strings.EqualFold(info.Email, p.Email) {
		return info, keys, nil // nothing to write
	}
	var q *litellm.Quota
	if changed {
		uq := tier.userQuota()
		q = &uq
	}
	if err := s.lite.UpdateUser(ctx, uid, p.Email, q, meta); err != nil {
		return litellm.UserInfo{}, nil, err
	}
	if !changed {
		return info, keys, nil
	}
	info, keys, _, err = s.lite.GetUser(ctx, uid)
	return info, keys, err
}

func view(keys []litellm.Key) ([]KeyView, int) {
	out := make([]KeyView, 0, len(keys))
	own := 0
	for _, k := range keys {
		alias := ""
		if k.Alias != nil {
			alias = *k.Alias
		}
		v := KeyView{ID: k.Token, Name: shownName(alias), Chat: isChatAlias(alias), Spend: k.Spend,
			Budget: k.MaxBudget, BudgetDuration: k.BudgetDuration, ResetAt: k.BudgetResetAt}
		if v.Chat {
			v.Name = "DHBW-Chat (automatisch)"
		} else {
			own++
		}
		out = append(out, v)
	}
	return out, own
}

// Usage returns quota, limits and keys.
func (s *Service) Usage(ctx context.Context, p Person, tier Tier) (Usage, error) {
	info, keys, err := s.reconcile(ctx, p, tier)
	if err != nil {
		return Usage{}, err
	}
	kv, own := view(keys)
	return Usage{Spend: info.Spend, Budget: info.MaxBudget, BudgetDuration: info.BudgetDuration, ResetAt: info.BudgetResetAt,
		RPM: info.RPMLimit, TPM: info.TPMLimit, Models: info.Models, Keys: kv, MaxKeys: s.maxKeys, OwnKeys: own}, nil
}

// Create makes a new key and returns its secret (shown exactly once).
func (s *Service) Create(ctx context.Context, p Person, tier Tier, name string) (string, KeyView, error) {
	name = strings.TrimSpace(name)
	if len([]rune(name)) > 40 {
		name = string([]rune(name)[:40])
	}
	safe := strings.Trim(unsafeChars.ReplaceAllString(name, "-"), "-.")
	if safe == "" || !regexp.MustCompile(`[A-Za-z0-9]`).MatchString(safe) {
		return "", KeyView{}, ErrNameRequired
	}
	_, keys, err := s.reconcile(ctx, p, tier)
	if err != nil {
		return "", KeyView{}, err
	}
	views, own := view(keys)
	if own >= s.maxKeys {
		return "", KeyView{}, ErrTooManyKeys
	}
	for _, v := range views {
		if !v.Chat && v.Name == safe {
			return "", KeyView{}, ErrDuplicate
		}
	}
	alias := ownAlias(p.UserID(), safe)
	secret, err := s.lite.GenerateKey(ctx, p.UserID(), alias, tier.keyQuota(),
		map[string]any{"created_by": "llm-management-api", "tier": tier.Name})
	if litellm.IsStatus(err, 400) && strings.Contains(err.Error(), "already exists") {
		return "", KeyView{}, ErrDuplicate
	}
	if err != nil {
		return "", KeyView{}, err
	}
	return secret, KeyView{Name: safe}, nil
}

// Delete removes one of the person's keys; another person's id is "not found".
func (s *Service) Delete(ctx context.Context, p Person, id string) error {
	_, keys, found, err := s.lite.GetUser(ctx, p.UserID())
	if err != nil {
		return err
	}
	if !found {
		return ErrNotFound
	}
	for _, k := range keys {
		if k.Token == id {
			return s.lite.DeleteKeys(ctx, id)
		}
	}
	return ErrNotFound
}
