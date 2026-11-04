package webserver

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/llm-management-api/internal/access"
	"github.com/pfisterer/llm-management-api/internal/litellm"
	"go.uber.org/zap"
)

// AliasModel is one model behind an alias: how many deployments (machines or replicas) serve it under the alias, and how many of them passed LiteLLM's last health check.
type AliasModel struct {
	Model       string `json:"model" example:"qwen3.6:35b-a3b-coding-nvfp4"`
	Deployments int    `json:"deployments" example:"33"`
	Healthy     int    `json:"healthy" example:"31"`
}

// ModelLimit is how long a prompt and an answer may be under a model name so that every deployment behind it takes them: the smallest max_input_tokens and max_output_tokens the discovery job set on them from each machine's or replica's context. 0 = none of them declares one. Tools write it into their configuration (VS Code's maxInputTokens), so they shorten the history themselves instead of the backend cutting it off.
type ModelLimit struct {
	MaxInputTokens  int `json:"max_input_tokens" example:"24576"`
	MaxOutputTokens int `json:"max_output_tokens" example:"8192"`
}

// AliasModels is one alias with its models, the most deployed first, and the limits every deployment behind it takes.
type AliasModels struct {
	Alias  string       `json:"alias" example:"chat-default"`
	Models []AliasModel `json:"models"`
	Limit  ModelLimit   `json:"limit"`
}

// ModelAliasesResponse lists the models behind each alias. HealthKnown is false when LiteLLM's health check could not be read; healthy is 0 everywhere then.
type ModelAliasesResponse struct {
	Aliases     []AliasModels `json:"aliases"`
	HealthKnown bool          `json:"health_known"`
}

// AliasSource is the part of the LiteLLM client the alias view needs.
type AliasSource interface {
	ModelInfo(ctx context.Context) ([]litellm.Deployment, error)
	Health(ctx context.Context) (healthy, unhealthy []litellm.HealthEndpoint, err error)
}

// ModelAliases answers which models stand behind the aliases, from LiteLLM's model list, kept for a minute in the process: every signed-in page asks, the model list changes with the discovery run every few minutes. Everybody sees the base aliases; a model group the tree granted a person's project (chat-large) only that person.
type ModelAliases struct {
	src     AliasSource
	aliases []string
	ttl     time.Duration
	now     func() time.Time
	log     *zap.SugaredLogger

	mu          sync.Mutex
	cached      map[string]AliasModels // every model name of the model list
	healthKnown bool
	limits      map[string]ModelLimit // every model name of the model list, not only the aliases
	until       time.Time
}

// allProxyModels is the group that opens every model under its own name; it is no model name and has no view.
const allProxyModels = "all-proxy-models"

// NewModelAliases shows the given aliases (LLM_BASE_MODELS), in this order.
func NewModelAliases(src AliasSource, aliases []string, log *zap.SugaredLogger) *ModelAliases {
	return &ModelAliases{src: src, aliases: slices.Clone(aliases), ttl: time.Minute, now: time.Now, log: log}
}

// Get returns the view of the base aliases and the given granted ones, from the cache when it is fresh. When LiteLLM fails and an older view exists, that one is served for another minute instead of an error.
func (m *ModelAliases) Get(ctx context.Context, granted []string) (ModelAliasesResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.refresh(ctx); err != nil {
		return ModelAliasesResponse{}, err
	}
	names := slices.Clone(m.aliases)
	for _, g := range granted {
		if g != "" && g != allProxyModels && !slices.Contains(names, g) {
			names = append(names, g)
		}
	}
	out := ModelAliasesResponse{Aliases: make([]AliasModels, 0, len(names)), HealthKnown: m.healthKnown}
	for _, a := range names {
		v, ok := m.cached[a]
		if !ok {
			// An alias nothing serves right now (its replica gave way to a job): listed without models, as LiteLLM knows it not.
			v = AliasModels{Alias: a, Models: []AliasModel{}}
		}
		out.Aliases = append(out.Aliases, v)
	}
	return out, nil
}

// Limits returns the limits of every model name in the model list (aliases and models under their own name), from the same cache as Get. A name without a declared limit is missing.
func (m *ModelAliases) Limits(ctx context.Context) (map[string]ModelLimit, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.refresh(ctx); err != nil {
		return nil, err
	}
	return m.limits, nil
}

// refresh rebuilds the cache when it is stale; m.mu is held.
func (m *ModelAliases) refresh(ctx context.Context) error {
	if m.cached != nil && m.now().Before(m.until) {
		return nil
	}
	v, healthKnown, limits, err := m.build(ctx)
	if err != nil {
		if m.cached == nil {
			return err
		}
		m.log.Warnw("model aliases: serving the previous view", "error", err)
		m.until = m.now().Add(m.ttl)
		return nil
	}
	m.cached, m.healthKnown, m.limits, m.until = v, healthKnown, limits, m.now().Add(m.ttl)
	return nil
}

// smallestLimits is per model name the smallest limits its deployments declare (each field on its own; a deployment without one does not count).
func smallestLimits(deps []litellm.Deployment) map[string]ModelLimit {
	out := map[string]ModelLimit{}
	lower := func(cur int, v float64) int {
		if n := int(v); n > 0 && (cur == 0 || n < cur) {
			return n
		}
		return cur
	}
	for _, d := range deps {
		if d.Info.MaxInputTokens <= 0 && d.Info.MaxOutputTokens <= 0 {
			continue
		}
		l := out[d.ModelName]
		l.MaxInputTokens = lower(l.MaxInputTokens, d.Info.MaxInputTokens)
		l.MaxOutputTokens = lower(l.MaxOutputTokens, d.Info.MaxOutputTokens)
		out[d.ModelName] = l
	}
	return out
}

// build reads per model name of the model list the models behind it, whether health was readable, and the limits.
func (m *ModelAliases) build(ctx context.Context) (map[string]AliasModels, bool, map[string]ModelLimit, error) {
	deps, err := m.src.ModelInfo(ctx)
	if err != nil {
		return nil, false, nil, err
	}
	limits := smallestLimits(deps)
	// Health is matched per deployment id; without one (older LiteLLM) per address and model, which the passthrough and alias deployments of one machine share.
	healthyID, badID, healthyAt, badAt := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	healthy, unhealthy, herr := m.src.Health(ctx)
	if herr != nil {
		m.log.Warnw("model aliases: health check not readable", "error", herr)
	}
	for _, e := range healthy {
		healthyID[e.ModelID], healthyAt[e.APIBase+"|"+e.Model] = true, true
	}
	for _, e := range unhealthy {
		badID[e.ModelID], badAt[e.APIBase+"|"+e.Model] = true, true
	}
	isHealthy := func(d litellm.Deployment) bool {
		if id := d.Info.ID; id != "" && (healthyID[id] || badID[id]) {
			return healthyID[id]
		}
		at := d.Params.APIBase + "|" + d.Params.Model
		return healthyAt[at] && !badAt[at]
	}

	byAlias := map[string]map[string]*AliasModel{}
	for _, d := range deps {
		models, ok := byAlias[d.ModelName]
		if !ok {
			models = map[string]*AliasModel{}
			byAlias[d.ModelName] = models
		}
		// Only the model's name leaves this service: no address, no site.
		name := strings.TrimPrefix(d.Params.Model, "openai/")
		if name == "" {
			continue
		}
		am := models[name]
		if am == nil {
			am = &AliasModel{Model: name}
			models[name] = am
		}
		am.Deployments++
		if herr == nil && isHealthy(d) {
			am.Healthy++
		}
	}

	out := make(map[string]AliasModels, len(byAlias))
	for a, models := range byAlias {
		list := make([]AliasModel, 0, len(models))
		for _, am := range models {
			list = append(list, *am)
		}
		slices.SortFunc(list, func(x, y AliasModel) int {
			if x.Deployments != y.Deployments {
				return y.Deployments - x.Deployments
			}
			if x.Healthy != y.Healthy {
				return y.Healthy - x.Healthy
			}
			return strings.Compare(x.Model, y.Model)
		})
		out[a] = AliasModels{Alias: a, Models: list, Limit: limits[a]}
	}
	return out, herr == nil, limits, nil
}

// getModelAliases godoc
//
//	@ID				getModelAliases
//	@Summary		Models behind the aliases
//	@Description	For everybody with LLM tokens (and admins): which models currently stand behind each alias (chat-default, chat-fast, …; a model group the person's projects were granted on top, like chat-large, only for them), with the number of deployments serving each under the alias and how many of them passed the last health check, the most deployed first, and the longest prompt and answer every deployment of the alias takes (limit, 0 = unknown). Which one answers a request depends on load and reachability; the response header x-litellm-model-name of the gateway names it. Read from LiteLLM, at most a minute old; no addresses or sites.
//	@Tags			llm
//	@Produce		json
//	@Security		BearerAuth
//	@Success		200	{object}	ModelAliasesResponse
//	@Failure		403	{object}	ErrorResponse
//	@Failure		502	{object}	ErrorResponse
//	@Router			/v1/models/aliases [get]
func (s *Server) getModelAliases(c *gin.Context) {
	cl := caller(c)
	grant, status, msg := s.quota.GrantFor(c.Request.Context(), cl.Tokens)
	if status != 0 && !cl.Role.AtLeast(access.RoleFleetAdmin) {
		code := map[int]string{http.StatusForbidden: "no_quota", http.StatusServiceUnavailable: "unavailable"}[status]
		abort(c, status, code, msg)
		return
	}
	// The groups of the person's projects (an admin without a project: the base aliases only).
	var granted []string
	for _, p := range grant.Pools {
		granted = append(granted, p.Models...)
	}
	v, err := s.aliases.Get(c.Request.Context(), granted)
	if err != nil {
		s.upstreamError(c, "model aliases", err)
		return
	}
	c.JSON(http.StatusOK, v)
}
