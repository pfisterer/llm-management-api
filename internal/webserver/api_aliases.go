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

// AliasModels is one alias with its models, the most deployed first.
type AliasModels struct {
	Alias  string       `json:"alias" example:"chat-default"`
	Models []AliasModel `json:"models"`
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

// ModelAliases answers which models stand behind the aliases, from LiteLLM's model list, kept for a minute in the process: every signed-in page asks, the model list changes with the discovery run every few minutes.
type ModelAliases struct {
	src     AliasSource
	aliases []string
	ttl     time.Duration
	now     func() time.Time
	log     *zap.SugaredLogger

	mu     sync.Mutex
	cached *ModelAliasesResponse
	until  time.Time
}

// NewModelAliases shows the given aliases (LLM_BASE_MODELS), in this order.
func NewModelAliases(src AliasSource, aliases []string, log *zap.SugaredLogger) *ModelAliases {
	return &ModelAliases{src: src, aliases: slices.Clone(aliases), ttl: time.Minute, now: time.Now, log: log}
}

// Get returns the view, from the cache when it is fresh. When LiteLLM fails and an older view exists, that one is served for another minute instead of an error.
func (m *ModelAliases) Get(ctx context.Context) (ModelAliasesResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cached != nil && m.now().Before(m.until) {
		return *m.cached, nil
	}
	v, err := m.build(ctx)
	if err != nil {
		if m.cached == nil {
			return ModelAliasesResponse{}, err
		}
		m.log.Warnw("model aliases: serving the previous view", "error", err)
		m.until = m.now().Add(m.ttl)
		return *m.cached, nil
	}
	m.cached, m.until = &v, m.now().Add(m.ttl)
	return v, nil
}

func (m *ModelAliases) build(ctx context.Context) (ModelAliasesResponse, error) {
	deps, err := m.src.ModelInfo(ctx)
	if err != nil {
		return ModelAliasesResponse{}, err
	}
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
	for _, a := range m.aliases {
		byAlias[a] = map[string]*AliasModel{}
	}
	for _, d := range deps {
		models, ok := byAlias[d.ModelName]
		if !ok {
			continue
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

	out := ModelAliasesResponse{Aliases: make([]AliasModels, 0, len(m.aliases)), HealthKnown: herr == nil}
	for _, a := range m.aliases {
		list := make([]AliasModel, 0, len(byAlias[a]))
		for _, am := range byAlias[a] {
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
		out.Aliases = append(out.Aliases, AliasModels{Alias: a, Models: list})
	}
	return out, nil
}

// getModelAliases godoc
//
//	@ID				getModelAliases
//	@Summary		Models behind the aliases
//	@Description	For everybody with LLM tokens (and admins): which models currently stand behind each alias (chat-default, chat-fast, …), with the number of deployments serving each under the alias and how many of them passed the last health check, the most deployed first. Which one answers a request depends on load and reachability; the response header x-litellm-model-name of the gateway names it. Read from LiteLLM, at most a minute old; no addresses or sites.
//	@Tags			llm
//	@Produce		json
//	@Security		BearerAuth
//	@Success		200	{object}	ModelAliasesResponse
//	@Failure		403	{object}	ErrorResponse
//	@Failure		502	{object}	ErrorResponse
//	@Router			/v1/models/aliases [get]
func (s *Server) getModelAliases(c *gin.Context) {
	if cl := caller(c); !cl.Role.AtLeast(access.RoleFleetAdmin) {
		if _, status, msg := s.quota.GrantFor(c.Request.Context(), cl.Tokens); status != 0 {
			code := map[int]string{http.StatusForbidden: "no_quota", http.StatusServiceUnavailable: "unavailable"}[status]
			abort(c, status, code, msg)
			return
		}
	}
	v, err := s.aliases.Get(c.Request.Context())
	if err != nil {
		s.upstreamError(c, "model aliases", err)
		return
	}
	c.JSON(http.StatusOK, v)
}
