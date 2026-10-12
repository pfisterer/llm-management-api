package webserver

import (
	"context"
	"net/http"

	"github.com/pfisterer/llm-management-api/internal/access"
	"github.com/pfisterer/llm-management-api/internal/keys"
	"github.com/pfisterer/llm-management-api/internal/workspace"
)

// DollarsPerMTokens is what a million tokens cost in LiteLLM's budgets here: every model is priced at 1e-7 per token, so a budget in "dollars" is a token budget.
const DollarsPerMTokens = 0.1

// LLMQuota decides what a person may use of the LLM: the budget per person comes from the budget tree — the sum of the LLM tokens of their projects, no tokens no access — and the tier gives rate limits, models and the short-term budget per key.
type LLMQuota struct {
	Tiers map[string]keys.Tier
	// DefaultTier is the tier of people whose access rule names none (or an unknown one) and of people without a rule.
	DefaultTier string
	Projects    *workspace.Service
	// SelfServiceURL is where a person without LLM tokens gets a project.
	SelfServiceURL string
}

// TierFor is the person's tier with the monthly budget from their projects; a status and message when they may not use the LLM.
func (q LLMQuota) TierFor(ctx context.Context, d access.Decision, tokens []string) (keys.Tier, int, string) {
	t, ok := q.Tiers[q.TierName(d)]
	if !ok {
		return keys.Tier{}, http.StatusInternalServerError, "Standard-Kontingentklasse nicht konfiguriert."
	}
	mtokens, status, msg := q.MTokensFor(ctx, tokens)
	if status != 0 {
		return keys.Tier{}, status, msg
	}
	if mtokens == 0 {
		msg := "Kein Projekt mit LLM-Tokens."
		if q.SelfServiceURL != "" {
			msg += " Ein Projekt gibt es unter " + q.SelfServiceURL
		}
		return keys.Tier{}, http.StatusForbidden, msg
	}
	t.UserBudget = float64(mtokens) * DollarsPerMTokens
	t.UserBudgetDuration = "30d"
	return t, 0, ""
}

// MTokensFor is the person's LLM budget in millions of tokens per 30 days; a status and message when the projects cannot be read.
func (q LLMQuota) MTokensFor(ctx context.Context, tokens []string) (int, int, string) {
	if q.Projects == nil {
		return 0, http.StatusServiceUnavailable, "Projekte sind hier nicht verfügbar."
	}
	mtokens, err := q.Projects.LLMMTokensFor(ctx, tokens)
	if err != nil {
		return 0, http.StatusServiceUnavailable, "Projekte konnten gerade nicht gelesen werden."
	}
	return mtokens, 0, ""
}

// TierName is the tier TierFor uses: the rule's if it is configured, else the default tier.
func (q LLMQuota) TierName(d access.Decision) string {
	if _, ok := q.Tiers[d.Tier]; ok {
		return d.Tier
	}
	return q.DefaultTier
}
