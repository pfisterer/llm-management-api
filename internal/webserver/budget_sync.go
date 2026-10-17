package webserver

import (
	"context"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/pfisterer/llm-management-api/internal/access"
	"github.com/pfisterer/llm-management-api/internal/keys"
	"github.com/pfisterer/llm-management-api/internal/litellm"
	"github.com/pfisterer/llm-management-api/internal/promtext"
)

// BudgetSync keeps LiteLLM in step with the budget tree: one team per project with the project's LLM tokens as its budget (0 once it ended or is gone) and its rate limits and models, every person's quota (budget 0, see keys.SyncQuota), and every person's keys in pools they may draw from, with their pool's short-term budget. Without it this only happens when the person asks this service for something (keys, usage, chat): somebody who left a project would go on spending its pool with the keys they made. Runs every Interval and right after the tree changed a project (Nudge).
type BudgetSync struct {
	Lite     *litellm.Client
	Keys     *keys.Service
	Access   *access.Service
	Quota    LLMQuota
	Interval time.Duration
	Log      *zap.SugaredLogger

	nudge chan struct{}
	// last is what the last complete pass counted, for the metrics (WriteMetrics); made by NewBudgetSync.
	last *lastPass
}

type lastPass struct {
	mu     sync.Mutex
	counts *syncCounts
}

// syncCounts is what one pass saw: keys by kind and state, people by whether they have LLM access, when it finished.
type syncCounts struct {
	keys     keys.KeyTally
	people   map[string]int
	finished time.Time
}

func NewBudgetSync(b BudgetSync) *BudgetSync {
	b.nudge = make(chan struct{}, 1)
	b.last = &lastPass{}
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
			b.Log.Infow("budget sync: LiteLLM brought in step with the tree", "writes", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-b.nudge:
		}
	}
}

// Pass first brings every project's team in step, then goes through every LiteLLM user that is a person (has an email): writes their quota (budget 0) and moves their keys into pools they may draw from (keys.SyncKeys). A person whose access or projects cannot be read right now is skipped, never stopped. Returns how many writes it made (teams, quotas, keys).
func (b *BudgetSync) Pass(ctx context.Context) (int, error) {
	if b.Quota.Projects == nil {
		return 0, nil
	}
	pools, err := b.Quota.Projects.LLMPools(ctx)
	if err != nil {
		return 0, err
	}
	written, err := b.Keys.SyncTeams(ctx, b.Quota.toPools(pools))
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
	counts := &syncCounts{people: map[string]int{"with_access": 0, "without_access": 0}}
	for _, u := range users {
		email := strings.ToLower(strings.TrimSpace(u.Email))
		if email == "" {
			continue // service users (e.g. the RAG key's) belong to no person
		}
		g, ok := b.grantFor(ctx, email)
		if !ok {
			continue
		}
		if len(g.Pools) > 0 {
			counts.people["with_access"]++
		} else {
			counts.people["without_access"]++
		}
		wrote, err := b.Keys.SyncQuota(ctx, u, g)
		if err != nil {
			b.Log.Warnw("budget sync: write failed", "email", email, "error", err)
			continue
		}
		if wrote {
			b.Log.Infow("budget sync: quota set", "email", email, "projects", len(g.Pools))
			written++
		}
		moved, err := b.Keys.SyncKeys(ctx, u, g, &counts.keys)
		written += moved
		if err != nil {
			b.Log.Warnw("budget sync: keys not in step", "email", email, "error", err)
		} else if moved > 0 {
			b.Log.Infow("budget sync: keys brought in step (project, short-term budget, limits)", "email", email, "writes", moved, "projects", len(g.Pools))
		}
	}
	counts.finished = time.Now()
	if b.last != nil {
		b.last.mu.Lock()
		b.last.counts = counts
		b.last.mu.Unlock()
	}
	return written, nil
}

// WriteMetrics writes what the last complete pass counted: the LiteLLM keys of people by kind and state, the people by whether they have LLM access, and when it finished. Nothing before the first pass. A person whose access could not be decided in that pass is missing from the counts.
func (b *BudgetSync) WriteMetrics(w *promtext.Writer) {
	if b.last == nil {
		return
	}
	b.last.mu.Lock()
	last := b.last.counts
	b.last.mu.Unlock()
	if last == nil {
		return
	}
	w.Header("aiaas_llm_keys", "gauge", "LiteLLM keys of people at the last budget sync, by kind (own, chat, workspace) and state (pool: in a project's pool the person may draw from; none: outside every pool, cannot spend).")
	for _, k := range keys.KeyKinds {
		for _, st := range keys.KeyStates {
			w.Sample("aiaas_llm_keys", float64(last.keys.Count(k, st)), "kind", k, "state", st)
		}
	}
	w.Header("aiaas_llm_people", "gauge", "People known to LiteLLM at the last budget sync, by whether a project gives them LLM tokens (with_access) or not (without_access).")
	for _, st := range []string{"with_access", "without_access"} {
		w.Sample("aiaas_llm_people", float64(last.people[st]), "access", st)
	}
	w.Header("aiaas_llm_budget_sync_timestamp_seconds", "gauge", "Unix time the last complete budget sync (teams, quotas and keys in step with the tree) finished.")
	w.Sample("aiaas_llm_budget_sync_timestamp_seconds", float64(last.finished.Unix()))
}

// grantFor is the person's pools, none for somebody without LLM tokens; false when it cannot be decided now.
func (b *BudgetSync) grantFor(ctx context.Context, email string) (keys.Grant, bool) {
	pools, status, msg := b.Quota.PoolsFor(ctx, b.Access.Who(ctx, email).Tokens)
	if status != 0 {
		b.Log.Warnw("budget sync: pools not decided", "email", email, "reason", msg)
		return keys.Grant{}, false
	}
	return keys.Grant{Pools: pools}, true
}
