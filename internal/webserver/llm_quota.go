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

// LLMQuota decides what a person may use of the LLM. With FromTree the budget per person comes from the budget tree — the sum of the LLM tokens of their projects — instead of their access rule's tier; the tier still gives rate limits, models and the short-term budget per key.
type LLMQuota struct {
	Tiers    map[string]keys.Tier
	FromTree bool
	// TreeTier is the tier whose limits apply to people whose rule names none (or an unknown one) when the budget comes from the tree.
	TreeTier string
	Projects *workspace.Service
	// SelfServiceURL is where a person without LLM tokens gets a project.
	SelfServiceURL string
}

// TierFor is the person's tier; a status and message when they may not use the LLM.
func (q LLMQuota) TierFor(ctx context.Context, d access.Decision, tokens []string) (keys.Tier, int, string) {
	t, ok := q.Tiers[d.Tier]
	if !q.FromTree {
		if !ok {
			return keys.Tier{}, http.StatusInternalServerError, "Kontingentklasse nicht konfiguriert."
		}
		return t, 0, ""
	}
	if !ok {
		if t, ok = q.Tiers[q.TreeTier]; !ok {
			return keys.Tier{}, http.StatusInternalServerError, "Kontingentklasse für den Budgetbaum nicht konfiguriert."
		}
	}
	if q.Projects == nil {
		return keys.Tier{}, http.StatusServiceUnavailable, "Projekte sind hier nicht verfügbar."
	}
	mtokens, err := q.Projects.LLMMTokensFor(ctx, tokens)
	if err != nil {
		return keys.Tier{}, http.StatusServiceUnavailable, "Projekte konnten gerade nicht gelesen werden."
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
