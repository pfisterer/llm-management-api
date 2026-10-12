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

// BudgetSync keeps LiteLLM in step with the budget tree and the tiers: one team per project with the project's LLM tokens as its budget (0 once it ended or is gone), every person's quota (the tier's, budget 0), and every person's keys in pools they may draw from. Without it this only happens when the person asks this service for something (keys, usage, chat): somebody who left a project would go on spending its pool with the keys they made. Runs every Interval and right after the tree changed a project (Nudge).
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
			b.Log.Infow("budget sync: LiteLLM brought in step with tiers and tree", "writes", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-b.nudge:
		}
	}
}

// Pass first brings every project's team in step, then goes through every LiteLLM user that is a person (has an email): writes their tier (budget 0) and moves their keys into pools they may draw from (keys.SyncKeys). A person whose access or projects cannot be read right now is skipped, never stopped. Returns how many writes it made (teams, quotas, keys).
func (b *BudgetSync) Pass(ctx context.Context) (int, error) {
	if b.Quota.Projects == nil {
		return 0, nil
	}
	pools, err := b.Quota.Projects.LLMPools(ctx)
	if err != nil {
		return 0, err
	}
	written, err := b.Keys.SyncTeams(ctx, toPools(pools))
	if err != nil {
		// A team that could not be written does not keep the people's quotas and keys from being right.
		b.Log.Warnw("budget sync: teams not all in step", "error", err)
	} else if written > 0 {
		b.Log.Infow("budget sync: teams set", "teams", written)
	}
	users, err := b.Lite.ListUsers(ctx)
	if err != nil {
		return written, err
	}
	for _, u := range users {
		email := strings.ToLower(strings.TrimSpace(u.Email))
		if email == "" {
			continue // service users (e.g. the RAG key's) belong to no person
		}
		g, ok := b.grantFor(ctx, email)
		if !ok {
			continue
		}
		wrote, err := b.Keys.SyncQuota(ctx, u, g.Tier)
		if err != nil {
			b.Log.Warnw("budget sync: write failed", "email", email, "error", err)
			continue
		}
		if wrote {
			b.Log.Infow("budget sync: quota set", "email", email, "tier", g.Tier.Name)
			written++
		}
		moved, err := b.Keys.SyncKeys(ctx, u, g)
		written += moved
		if err != nil {
			b.Log.Warnw("budget sync: keys not in step", "email", email, "error", err)
		} else if moved > 0 {
			b.Log.Infow("budget sync: keys moved", "email", email, "keys", moved, "projects", len(g.Pools))
		}
	}
	return written, nil
}

// grantFor is the person's tier with their pools, or with none for somebody without LLM tokens; false when it cannot be decided now.
func (b *BudgetSync) grantFor(ctx context.Context, email string) (keys.Grant, bool) {
	d, tokens, err := b.Access.Decide(ctx, email)
	if err != nil {
		b.Log.Warnw("budget sync: access decision failed", "email", email, "error", err)
		return keys.Grant{}, false
	}
	g, status, msg := b.Quota.GrantFor(ctx, d, tokens)
	switch status {
	case 0:
		return g, true
	case http.StatusForbidden:
		t, ok := b.Quota.Tiers[b.Quota.TierName(d)]
		if !ok {
			return keys.Grant{}, false
		}
		return keys.Grant{Tier: t}, true
	default:
		b.Log.Warnw("budget sync: tier not decided", "email", email, "reason", msg)
		return keys.Grant{}, false
	}
}
