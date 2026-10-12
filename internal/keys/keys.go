// Package keys manages a person's LiteLLM user, API keys and usage.
//
// LLM tokens belong to projects, not to people: every project with tokens is one pool, a LiteLLM team (team id = the project's slug) whose budget all its members share. Every key is in the pool of one project the person may draw from; the person's own LiteLLM budget is 0, so a key outside every pool cannot spend anything. Who may draw from which pool is decided here (project membership, including groups), not by LiteLLM's team members.
//
// Carried over from the Node key broker, including the lessons learned there:
//   - A person's quota in LiteLLM is the tier's (rate limits, models, budget 0), the teams' budgets are the projects': kept in step on every request and by SyncQuota/SyncTeams/SyncKeys, so a change made in the LiteLLM admin UI does not last — limits are set in the tiers and the tree. Spend is never touched.
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
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"strings"

	"github.com/pfisterer/llm-management-api/internal/litellm"
)

// Tier is the quota class a person's access rule assigns: rate limits, models and the short-term budget per key. The monthly budget is not the tier's but the pools' (Pool).
type Tier struct {
	Name              string
	KeyBudget         float64
	KeyBudgetDuration string
	RPM               int
	TPM               int
	Models            []string
}

// DollarsPerMTokens is what a million tokens cost in LiteLLM's budgets here: every model is priced at 1e-7 per token, so a budget in "dollars" is a token budget.
const DollarsPerMTokens = 0.1

// PoolWindow is the window of a pool's budget; the team's spend resets after it.
const PoolWindow = "30d"

// Pool is a project's LLM tokens, shared by all its members: a LiteLLM team whose id is the project's slug.
type Pool struct {
	Slug    string
	Name    string
	MTokens int // per PoolWindow; 0 = the project ended or has no tokens
}

// Budget is the pool's team budget in LiteLLM's dollars.
func (p Pool) Budget() float64 { return float64(p.MTokens) * DollarsPerMTokens }

// Grant is what a person may use of the LLM: the tier's limits and the pools they may draw from, sorted (see workspace.LLMPoolsFor); empty pools = no LLM.
type Grant struct {
	Tier  Tier
	Pools []Pool
}

func (g Grant) pool(slug string) (Pool, bool) {
	for _, p := range g.Pools {
		if p.Slug == slug {
			return p, true
		}
	}
	return Pool{}, false
}

// defaultPool is the pool stored as the person's default in their LiteLLM metadata, or the first one when none is stored or it is no longer theirs. ok=false without pools.
func (g Grant) defaultPool(info litellm.UserInfo) (Pool, bool) {
	if slug, _ := info.Metadata[defaultProjectKey].(string); slug != "" {
		if p, ok := g.pool(slug); ok {
			return p, true
		}
	}
	if len(g.Pools) == 0 {
		return Pool{}, false
	}
	return g.Pools[0], true
}

// defaultProjectKey is the LiteLLM user metadata key with the slug of the person's default project.
const defaultProjectKey = "default_project"

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
	// ErrNotEligible: the project is not one whose pool the person may draw from.
	ErrNotEligible = errors.New("project not eligible")
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

// KeyView is one key as the UI shows it. Project is the slug of the pool the key spends from ("" for a key outside every pool, which cannot spend anything); ProjectName is empty when that project is not one of the person's.
type KeyView struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Chat           bool     `json:"chat"`
	Project        string   `json:"project" example:"t-7841d99e-68a2-47b7-88a5-1ab55180f4f5"`
	ProjectName    string   `json:"project_name" example:"Machine Learning WWI23SEB"`
	Spend          float64  `json:"spend"`
	Budget         *float64 `json:"budget"`
	BudgetDuration *string  `json:"budget_duration"`
	ResetAt        *string  `json:"reset_at"`
}

// ProjectUsage is one pool the person may draw from, as LiteLLM's team reports it: Spend and Budget in LiteLLM's dollars (DollarsPerMTokens), shared by all members of the project.
type ProjectUsage struct {
	Slug           string  `json:"slug" example:"t-7841d99e-68a2-47b7-88a5-1ab55180f4f5"`
	Name           string  `json:"name" example:"Machine Learning WWI23SEB"`
	MTokens        int     `json:"mtokens" example:"100"`
	Spend          float64 `json:"spend" example:"1.25"`
	Budget         float64 `json:"budget" example:"10"`
	BudgetDuration string  `json:"budget_duration" example:"30d"`
	ResetAt        *string `json:"reset_at"`
	Default        bool    `json:"default"`
}

// Usage is the person's quota state: the pools they draw from, the tier's limits and their keys.
type Usage struct {
	Projects []ProjectUsage `json:"projects"`
	RPM      *int           `json:"rpm"`
	TPM      *int           `json:"tpm"`
	Models   []string       `json:"models"`
	Keys     []KeyView      `json:"keys"`
	MaxKeys  int            `json:"max_keys"`
	OwnKeys  int            `json:"own_keys"`
}

// Service implements the key operations on top of LiteLLM.
type Service struct {
	lite    *litellm.Client
	maxKeys int
}

func NewService(lite *litellm.Client, maxKeys int) *Service {
	return &Service{lite: lite, maxKeys: maxKeys}
}

// userQuota is the tier's with a budget of 0: a person's own LiteLLM budget is always 0, so a key outside every pool (a personal key) cannot spend anything; keys in a pool spend the pool's team budget (LiteLLM needs general_settings.skip_user_budget_on_team_key for that, or the user's 0 would stop them too).
func (t Tier) userQuota() litellm.Quota {
	return litellm.Quota{MaxBudget: 0, BudgetDuration: PoolWindow, TPMLimit: t.TPM, RPMLimit: t.RPM, Models: t.Models}
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
	changed := previous != tier.Name || quotaDiffers(info, tier)
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

// quotaDiffers: the user's quota in LiteLLM is not the tier's (with a budget of 0): budget, its window, rate limits or models.
func quotaDiffers(info litellm.UserInfo, t Tier) bool {
	q := t.userQuota()
	if info.MaxBudget == nil || math.Abs(*info.MaxBudget-q.MaxBudget) > 1e-9 {
		return true
	}
	if info.BudgetDuration == nil || *info.BudgetDuration != q.BudgetDuration {
		return true
	}
	if t.RPM > 0 && (info.RPMLimit == nil || *info.RPMLimit != t.RPM) {
		return true
	}
	if t.TPM > 0 && (info.TPMLimit == nil || *info.TPMLimit != t.TPM) {
		return true
	}
	return len(t.Models) > 0 && !slices.Equal(info.Models, t.Models)
}

// SyncQuota brings a LiteLLM user's quota in step with the tier (budget 0, see userQuota), so a new tier applies without the person asking for anything and a change made in LiteLLM's UI does not last. Reports whether it wrote.
func (s *Service) SyncQuota(ctx context.Context, info litellm.UserInfo, tier Tier) (bool, error) {
	if !quotaDiffers(info, tier) {
		return false, nil
	}
	q := tier.userQuota()
	meta := map[string]any{}
	maps.Copy(meta, info.Metadata)
	meta["tier"], meta["source"] = tier.Name, "llm-management-api"
	return true, s.lite.UpdateUser(ctx, info.UserID, info.Email, &q, meta)
}

func view(keys []litellm.Key, pools []Pool) ([]KeyView, int) {
	names := map[string]string{}
	for _, p := range pools {
		names[p.Slug] = p.Name
	}
	out := make([]KeyView, 0, len(keys))
	own := 0
	for _, k := range keys {
		alias := ""
		if k.Alias != nil {
			alias = *k.Alias
		}
		v := KeyView{ID: k.Token, Name: shownName(alias), Chat: isChatAlias(alias), Project: k.Team(), ProjectName: names[k.Team()], Spend: k.Spend,
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

// Usage returns the pools, limits and keys.
func (s *Service) Usage(ctx context.Context, p Person, g Grant) (Usage, error) {
	info, keys, err := s.reconcile(ctx, p, g.Tier)
	if err != nil {
		return Usage{}, err
	}
	def, _ := g.defaultPool(info)
	projects := make([]ProjectUsage, 0, len(g.Pools))
	for _, pool := range g.Pools {
		pu := ProjectUsage{Slug: pool.Slug, Name: pool.Name, MTokens: pool.MTokens, Budget: pool.Budget(), BudgetDuration: PoolWindow, Default: pool.Slug == def.Slug}
		team, found, err := s.lite.GetTeam(ctx, pool.Slug)
		if err != nil {
			return Usage{}, err
		}
		if found {
			pu.Spend, pu.ResetAt = team.Spend, team.BudgetResetAt
			if team.MaxBudget != nil {
				pu.Budget = *team.MaxBudget
			}
			if team.BudgetDuration != nil {
				pu.BudgetDuration = *team.BudgetDuration
			}
		}
		projects = append(projects, pu)
	}
	kv, own := view(keys, g.Pools)
	return Usage{Projects: projects, RPM: info.RPMLimit, TPM: info.TPMLimit, Models: info.Models, Keys: kv, MaxKeys: s.maxKeys, OwnKeys: own}, nil
}

// poolFor is the pool a key goes to: the named project when the person may draw from it, the default project when none is named.
func (g Grant) poolFor(info litellm.UserInfo, slug string) (Pool, error) {
	if slug == "" {
		if def, ok := g.defaultPool(info); ok {
			return def, nil
		}
		return Pool{}, ErrNotEligible
	}
	if p, ok := g.pool(slug); ok {
		return p, nil
	}
	return Pool{}, ErrNotEligible
}

// Create makes a new key in a project's pool (the default project's when project is empty) and returns its secret (shown exactly once) and the key.
func (s *Service) Create(ctx context.Context, p Person, g Grant, name, project string) (string, KeyView, error) {
	name = strings.TrimSpace(name)
	if len([]rune(name)) > 40 {
		name = string([]rune(name)[:40])
	}
	safe := strings.Trim(unsafeChars.ReplaceAllString(name, "-"), "-.")
	if safe == "" || !regexp.MustCompile(`[A-Za-z0-9]`).MatchString(safe) {
		return "", KeyView{}, ErrNameRequired
	}
	info, keys, err := s.reconcile(ctx, p, g.Tier)
	if err != nil {
		return "", KeyView{}, err
	}
	pool, err := g.poolFor(info, strings.TrimSpace(project))
	if err != nil {
		return "", KeyView{}, err
	}
	views, own := view(keys, g.Pools)
	if own >= s.maxKeys {
		return "", KeyView{}, ErrTooManyKeys
	}
	for _, v := range views {
		if !v.Chat && v.Name == safe {
			return "", KeyView{}, ErrDuplicate
		}
	}
	if _, err := s.ensureTeam(ctx, pool); err != nil {
		return "", KeyView{}, err
	}
	alias := ownAlias(p.UserID(), safe)
	secret, err := s.lite.GenerateKey(ctx, p.UserID(), pool.Slug, alias, g.Tier.keyQuota(),
		map[string]any{"created_by": "llm-management-api", "tier": g.Tier.Name})
	if litellm.IsStatus(err, 400) && strings.Contains(err.Error(), "already exists") {
		return "", KeyView{}, ErrDuplicate
	}
	if err != nil {
		return "", KeyView{}, err
	}
	return secret, KeyView{Name: safe, Project: pool.Slug, ProjectName: pool.Name}, nil
}

// SetDefaultProject stores the project whose pool the person's chat and new keys use by default, and moves the chat key there right away.
func (s *Service) SetDefaultProject(ctx context.Context, p Person, g Grant, project string) (Pool, error) {
	pool, ok := g.pool(strings.TrimSpace(project))
	if !ok {
		return Pool{}, ErrNotEligible
	}
	info, keys, err := s.reconcile(ctx, p, g.Tier)
	if err != nil {
		return Pool{}, err
	}
	if cur, _ := info.Metadata[defaultProjectKey].(string); cur != pool.Slug {
		meta := map[string]any{}
		maps.Copy(meta, info.Metadata)
		meta[defaultProjectKey] = pool.Slug
		if err := s.lite.UpdateUser(ctx, p.UserID(), p.Email, nil, meta); err != nil {
			return Pool{}, err
		}
	}
	for _, k := range keys {
		if k.Alias != nil && isChatAlias(*k.Alias) && k.Team() != pool.Slug {
			if err := s.moveKey(ctx, p.UserID(), k.Token, pool); err != nil {
				return Pool{}, err
			}
		}
	}
	return pool, nil
}

// teamSpec is the team a pool should be.
func teamSpec(p Pool) litellm.TeamSpec {
	return litellm.TeamSpec{TeamID: p.Slug, Alias: p.Name, MaxBudget: p.Budget(), BudgetDuration: PoolWindow,
		Metadata: map[string]any{"source": "llm-management-api", "project": p.Slug}}
}

// syncTeam brings an existing team in step with the pool; reports whether it wrote. The window is sent only when it differs, as LiteLLM restarts it with every write.
func (s *Service) syncTeam(ctx context.Context, t litellm.Team, p Pool) (bool, error) {
	want := teamSpec(p)
	alias := ""
	if t.Alias != nil {
		alias = *t.Alias
	}
	durationOK := t.BudgetDuration != nil && *t.BudgetDuration == want.BudgetDuration
	if alias == want.Alias && durationOK && t.MaxBudget != nil && math.Abs(*t.MaxBudget-want.MaxBudget) < 1e-9 {
		return false, nil
	}
	if durationOK {
		want.BudgetDuration = ""
	}
	meta := map[string]any{}
	maps.Copy(meta, t.Metadata)
	maps.Copy(meta, want.Metadata)
	want.Metadata = meta
	return true, s.lite.UpdateTeam(ctx, want)
}

// ensureTeam makes sure the pool's team exists with the pool's budget, before a key goes into it: a key in a team LiteLLM does not know would have no team budget at all.
func (s *Service) ensureTeam(ctx context.Context, p Pool) (litellm.Team, error) {
	t, found, err := s.lite.GetTeam(ctx, p.Slug)
	if err != nil {
		return litellm.Team{}, err
	}
	if !found {
		if err := s.lite.NewTeam(ctx, teamSpec(p)); err != nil {
			return litellm.Team{}, err
		}
		return litellm.Team{TeamID: p.Slug}, nil
	}
	_, err = s.syncTeam(ctx, t, p)
	return t, err
}

// SyncTeams keeps a team per pool: created for a pool with tokens, its budget and name kept in step; a pool without tokens (ended) keeps its team with a budget of 0, and so does a team of this service whose project is gone. Teams are never deleted, so their spend stays. Returns how many teams were written.
func (s *Service) SyncTeams(ctx context.Context, pools []Pool) (int, error) {
	teams, err := s.lite.ListTeams(ctx)
	if err != nil {
		return 0, err
	}
	byID := map[string]litellm.Team{}
	for _, t := range teams {
		byID[t.TeamID] = t
	}
	written := 0
	var errs []error
	known := map[string]bool{}
	for _, p := range pools {
		known[p.Slug] = true
		t, found := byID[p.Slug]
		var wrote bool
		var err error
		switch {
		case found:
			wrote, err = s.syncTeam(ctx, t, p)
		case p.MTokens > 0:
			wrote, err = true, s.lite.NewTeam(ctx, teamSpec(p))
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("team %s: %w", p.Slug, err))
		} else if wrote {
			written++
		}
	}
	for _, t := range teams {
		if known[t.TeamID] || t.Metadata["source"] != "llm-management-api" {
			continue
		}
		alias := t.TeamID
		if t.Alias != nil {
			alias = *t.Alias
		}
		wrote, err := s.syncTeam(ctx, t, Pool{Slug: t.TeamID, Name: alias})
		if err != nil {
			errs = append(errs, fmt.Errorf("team %s: %w", t.TeamID, err))
		} else if wrote {
			written++
		}
	}
	return written, errors.Join(errs...)
}

// UsedMTokens is what the pools used in their current windows, in millions of tokens (the teams' spend); a pool without a team is missing. A few pools are asked one by one, more with one list of all teams.
func (s *Service) UsedMTokens(ctx context.Context, slugs []string) (map[string]float64, error) {
	out := map[string]float64{}
	if len(slugs) <= 3 {
		for _, slug := range slugs {
			t, found, err := s.lite.GetTeam(ctx, slug)
			if err != nil {
				return nil, err
			}
			if found {
				out[slug] = t.Spend / DollarsPerMTokens
			}
		}
		return out, nil
	}
	teams, err := s.lite.ListTeams(ctx)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, slug := range slugs {
		want[slug] = true
	}
	for _, t := range teams {
		if want[t.TeamID] {
			out[t.TeamID] = t.Spend / DollarsPerMTokens
		}
	}
	return out, nil
}

// moveKey puts a key into a pool. LiteLLM moves a key into a team only when the key's user is a member of it (validate_key_team_change, even for the master key), so the person becomes a plain member first. That membership grants nothing: members may not make or change team keys by default, and who may draw from a pool is decided here (the sync takes the keys out again), not by LiteLLM's members.
func (s *Service) moveKey(ctx context.Context, userID, token string, p Pool) error {
	t, err := s.ensureTeam(ctx, p)
	if err != nil {
		return err
	}
	if !t.HasMember(userID) {
		if err := s.lite.AddTeamMember(ctx, p.Slug, userID); err != nil {
			return err
		}
	}
	return s.lite.SetKeyTeam(ctx, token, p.Slug)
}

// SyncKeys keeps the keys of a LiteLLM user in their pools: a key outside the person's pools (in a project they lost, or in none) moves to their default project. Without any pool, keys are taken out of their team instead (team_id null): they are then personal keys, and the user's budget of 0 stops them; when the person gets a project again, the next pass moves them in. Returns how many keys it moved.
func (s *Service) SyncKeys(ctx context.Context, info litellm.UserInfo, g Grant) (int, error) {
	_, keys, found, err := s.lite.GetUser(ctx, info.UserID)
	if err != nil || !found {
		return 0, err
	}
	def, hasPool := g.defaultPool(info)
	moved := 0
	for _, k := range keys {
		if _, ok := g.pool(k.Team()); ok {
			continue
		}
		switch {
		case hasPool:
			err = s.moveKey(ctx, info.UserID, k.Token, def)
		case k.Team() != "":
			err = s.lite.SetKeyTeam(ctx, k.Token, "")
		default:
			continue // already outside every team
		}
		if err != nil {
			return moved, err
		}
		moved++
	}
	return moved, nil
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
