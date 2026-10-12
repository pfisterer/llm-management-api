package webserver

import (
	"context"
	"net/http"

	"github.com/pfisterer/llm-management-api/internal/access"
	"github.com/pfisterer/llm-management-api/internal/keys"
	"github.com/pfisterer/llm-management-api/internal/workspace"
)

// LLMQuota decides what a person may use of the LLM: the projects whose token pools they may draw from come from the budget tree — every project with LLM tokens is one pool shared by its members, no project no access — and the tier gives rate limits, models and the short-term budget per key.
type LLMQuota struct {
	Tiers map[string]keys.Tier
	// DefaultTier is the tier of people whose access rule names none (or an unknown one) and of people without a rule.
	DefaultTier string
	Projects    *workspace.Service
	// SelfServiceURL is where a person without LLM tokens gets a project.
	SelfServiceURL string
}

// GrantFor is the person's tier with the pools they may draw from; a status and message when they may not use the LLM (no tier configured, projects not readable, no project with tokens).
func (q LLMQuota) GrantFor(ctx context.Context, d access.Decision, tokens []string) (keys.Grant, int, string) {
	t, ok := q.Tiers[q.TierName(d)]
	if !ok {
		return keys.Grant{}, http.StatusInternalServerError, "Standard-Kontingentklasse nicht konfiguriert."
	}
	pools, status, msg := q.PoolsFor(ctx, tokens)
	if status != 0 {
		return keys.Grant{}, status, msg
	}
	if len(pools) == 0 {
		msg := "Kein Projekt mit LLM-Tokens."
		if q.SelfServiceURL != "" {
			msg += " Ein Projekt gibt es unter " + q.SelfServiceURL
		}
		return keys.Grant{}, http.StatusForbidden, msg
	}
	return keys.Grant{Tier: t, Pools: pools}, 0, ""
}

// PoolsFor lists the pools the person may draw from (see workspace.LLMPoolsFor); a status and message when the projects cannot be read.
func (q LLMQuota) PoolsFor(ctx context.Context, tokens []string) ([]keys.Pool, int, string) {
	if q.Projects == nil {
		return nil, http.StatusServiceUnavailable, "Projekte sind hier nicht verfügbar."
	}
	list, err := q.Projects.LLMPoolsFor(ctx, tokens)
	if err != nil {
		return nil, http.StatusServiceUnavailable, "Projekte konnten gerade nicht gelesen werden."
	}
	return toPools(list), 0, ""
}

func toPools(list []workspace.LLMPool) []keys.Pool {
	out := make([]keys.Pool, 0, len(list))
	for _, p := range list {
		out = append(out, keys.Pool{Slug: p.Slug, Name: p.Name, MTokens: p.MTokens})
	}
	return out
}

// TierName is the tier GrantFor uses: the rule's if it is configured, else the default tier.
func (q LLMQuota) TierName(d access.Decision) string {
	if _, ok := q.Tiers[d.Tier]; ok {
		return d.Tier
	}
	return q.DefaultTier
}
