package webserver

import (
	"context"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/pfisterer/llm-management-api/internal/access"
	"github.com/pfisterer/llm-management-api/internal/keys"
	"github.com/pfisterer/llm-management-api/internal/litellm"
)

// BudgetSync keeps the monthly budget of every person in LiteLLM in step with the budget tree. Without it a budget is only written when the person asks this service for something (keys, usage, chat): keys made earlier would keep their old budget, and somebody whose projects ended would go on using them. Runs every Interval and right after the tree changed a project (Nudge).
type BudgetSync struct {
	Lite     *litellm.Client
	Keys     *keys.Service
	Access   *access.Service
	Quota    LLMQuota
	Interval time.Duration
	Log      *zap.SugaredLogger

	nudge chan struct{}
}

func NewBudgetSync(b BudgetSync) *BudgetSync {
	b.nudge = make(chan struct{}, 1)
	return &b
}

// Nudge asks for a pass soon; several nudges before it runs are one pass.
func (b *BudgetSync) Nudge() {
	select {
	case b.nudge <- struct{}{}:
	default:
	}
}

// Run passes on start, every Interval and after each nudge, until ctx ends.
func (b *BudgetSync) Run(ctx context.Context) {
	t := time.NewTicker(b.Interval)
	defer t.Stop()
	for {
		if n, err := b.Pass(ctx); err != nil {
			b.Log.Warnw("budget sync: pass failed", "error", err)
		} else if n > 0 {
			b.Log.Infow("budget sync: budgets brought in step with the tree", "users", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-b.nudge:
		}
	}
}

// Pass goes through every LiteLLM user that is a person (has an email) and writes the budget the tree gives them: the sum of their projects' LLM tokens, 0 without. A person whose access or projects cannot be read right now is skipped, never set to 0. Returns how many budgets were written.
func (b *BudgetSync) Pass(ctx context.Context) (int, error) {
	users, err := b.Lite.ListUsers(ctx)
	if err != nil {
		return 0, err
	}
	written := 0
	for _, u := range users {
		email := strings.ToLower(strings.TrimSpace(u.Email))
		if email == "" {
			continue // service users (e.g. the RAG key's) belong to no person
		}
		tier, ok := b.tierFor(ctx, email)
		if !ok {
			continue
		}
		wrote, err := b.Keys.SyncBudget(ctx, u, tier)
		if err != nil {
			b.Log.Warnw("budget sync: write failed", "email", email, "error", err)
			continue
		}
		if wrote {
			b.Log.Infow("budget sync: budget set", "email", email, "budget", tier.UserBudget)
			written++
		}
	}
	return written, nil
}

// tierFor is the tier with the tree's budget, or with a budget of 0 for somebody without LLM tokens; false when it cannot be decided now.
func (b *BudgetSync) tierFor(ctx context.Context, email string) (keys.Tier, bool) {
	d, tokens, err := b.Access.Decide(ctx, email)
	if err != nil {
		b.Log.Warnw("budget sync: access decision failed", "email", email, "error", err)
		return keys.Tier{}, false
	}
	tier, status, msg := b.Quota.TierFor(ctx, d, tokens)
	switch status {
	case 0:
		return tier, true
	case http.StatusForbidden:
		t, ok := b.Quota.Tiers[b.Quota.TierName(d)]
		if !ok {
			return keys.Tier{}, false
		}
		t.UserBudget, t.UserBudgetDuration = 0, "30d"
		return t, true
	default:
		b.Log.Warnw("budget sync: tier not decided", "email", email, "reason", msg)
		return keys.Tier{}, false
	}
}
