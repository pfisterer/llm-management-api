package webserver

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/cloud-self-service-golib/msg"
	"github.com/pfisterer/llm-management-api/internal/access"
	"github.com/pfisterer/llm-management-api/internal/cluster/gpu"
	"github.com/pfisterer/llm-management-api/internal/llm/keys"
	"github.com/pfisterer/llm-management-api/internal/llm/litellm"
	"github.com/pfisterer/llm-management-api/internal/llm/modeldesc"
)

// The names a person can use, each with what stands behind it (/v1/models/usable): kind, the projects it comes through, the models with their descriptions, and per device class the configuration. Only device classes leave the service, never a site, host or address.

// Device classes of a deployment.
const (
	DeviceGPU   = "gpu"   // a GPU node of the cluster (inference pool, vLLM)
	DeviceGB10  = "gb10"  // a GB10 of the cluster (inference pool, Strata)
	DeviceMac   = "mac"   // a Mac of the fleet (Ollama or Strata)
	DeviceOther = "other" // a site of the inventory outside the cluster
)

// Engines as the view names them; vllm and strata as in the GPU catalogue.
const engineOllama = "ollama"

// Kinds of a usable name.
const (
	KindAlias = "alias" // a stable name; the model behind it may change
	KindModel = "model" // a model under its own name; fixed as long as it is offered
)

// deployFacts is what the view keeps of one deployment.
type deployFacts struct {
	model, device, engine string
	in, out               int
	healthy               bool
}

// deviceOf classifies a deployment by what the discovery job wrote on it: a Mac of the fleet (Ollama reports names with a tag, name:tag, Strata without), a replica of the inference pool (site gpu-…; a model the GPU catalogue runs with Strata runs on a GB10, everything else with vLLM on a GPU node) or another site.
func deviceOf(d litellm.Deployment, served string, engines map[string]string) (device, engine string) {
	switch {
	case d.Info.Fleet:
		if strings.Contains(served, ":") {
			return DeviceMac, engineOllama
		}
		return DeviceMac, gpu.EngineStrata
	case strings.HasPrefix(d.Info.Site, gpu.InferenceSitePrefix):
		if engines[served] == gpu.EngineStrata {
			return DeviceGB10, gpu.EngineStrata
		}
		return DeviceGPU, gpu.EngineVLLM
	default:
		return DeviceOther, ""
	}
}

// LimitRange is the smallest and the largest limits the deployments behind a name declare (each field on its own; 0 = none declares one). The smallest is what every deployment takes, the largest what the best one takes.
type LimitRange struct {
	Min ModelLimit `json:"min"`
	Max ModelLimit `json:"max"`
}

// widen takes one deployment's limits into the range.
func (r *LimitRange) widen(in, out int) {
	lower := func(cur, v int) int {
		if v > 0 && (cur == 0 || v < cur) {
			return v
		}
		return cur
	}
	r.Min.MaxInputTokens, r.Min.MaxOutputTokens = lower(r.Min.MaxInputTokens, in), lower(r.Min.MaxOutputTokens, out)
	r.Max.MaxInputTokens, r.Max.MaxOutputTokens = max(r.Max.MaxInputTokens, in), max(r.Max.MaxOutputTokens, out)
}

// DeviceConfig is how a model runs on one class of device: engine (empty = unknown), limits, and how many deployments serve it there and passed the last health check.
type DeviceConfig struct {
	Device      string     `json:"device" enums:"gpu,gb10,mac,other" example:"mac"`
	Engine      string     `json:"engine" enums:"vllm,strata,ollama," example:"ollama"`
	Limits      LimitRange `json:"limits"`
	Deployments int        `json:"deployments" example:"28"`
	Healthy     int        `json:"healthy" example:"27"`
}

// UsableModel is one model behind a usable name, with its description (null = none yet) and its configuration per device class.
type UsableModel struct {
	Model       string                 `json:"model" example:"qwen3.6:35b-a3b-coding-nvfp4"`
	Deployments int                    `json:"deployments" example:"28"`
	Healthy     int                    `json:"healthy" example:"27"`
	Description *modeldesc.Description `json:"description" extensions:"x-nullable"`
	Devices     []DeviceConfig         `json:"devices"`
}

// UsableProject is a project of the person through which a name is available.
type UsableProject struct {
	Slug string `json:"slug" example:"p-ki-labor"`
	Name string `json:"name" example:"KI-Labor"`
}

// UsableName is one name the person can write into a request: an alias or a model under its own name. Base names are there through every project of the person (base true, projects empty); a granted name through the projects listed.
type UsableName struct {
	Name        string          `json:"name" example:"chat-default"`
	Kind        string          `json:"kind" enums:"alias,model" example:"alias"`
	Base        bool            `json:"base"`
	Projects    []UsableProject `json:"projects"`
	Deployments int             `json:"deployments" example:"28"`
	Healthy     int             `json:"healthy" example:"27"`
	Limits      LimitRange      `json:"limits"`
	Models      []UsableModel   `json:"models"`
}

// UsableModelsResponse lists the names the person can use. HealthKnown is false when LiteLLM's health check could not be read; healthy is 0 everywhere then.
type UsableModelsResponse struct {
	Names       []UsableName `json:"names"`
	HealthKnown bool         `json:"health_known"`
}

// Usable is the view of the base names and those the person's pools were granted, with the descriptions of the models behind them.
func (m *ModelAliases) Usable(ctx context.Context, pools []keys.Pool) (UsableModelsResponse, error) {
	view, err := m.view.Get(ctx, m.load)
	if err != nil {
		return UsableModelsResponse{}, err
	}
	descs := map[string]modeldesc.Description{}
	if m.descs != nil {
		list, err := m.descs.List(ctx)
		if err != nil {
			// The names stay usable without their texts.
			m.log.Warnw("model descriptions not readable", "error", err)
		}
		descs = modeldesc.ByModel(list)
	}

	names := slices.Clone(m.aliases)
	via := map[string][]UsableProject{}
	for _, p := range pools {
		for _, g := range p.Models {
			if g == "" || g == allProxyModels || slices.Contains(m.aliases, g) {
				continue
			}
			if !slices.Contains(names, g) {
				names = append(names, g)
			}
			via[g] = append(via[g], UsableProject{Slug: p.Slug, Name: p.Name})
		}
	}

	out := UsableModelsResponse{Names: make([]UsableName, 0, len(names)), HealthKnown: view.healthKnown}
	for _, n := range names {
		u := UsableName{Name: n, Kind: KindAlias, Base: slices.Contains(m.aliases, n), Projects: via[n], Models: []UsableModel{}}
		if u.Projects == nil {
			u.Projects = []UsableProject{}
		}
		facts := view.deps[n]
		byModel := map[string]*UsableModel{}
		byDevice := map[string]map[string]*DeviceConfig{}
		for _, f := range facts {
			if !u.Base && f.model == n {
				u.Kind = KindModel
			}
			u.Deployments++
			if f.healthy {
				u.Healthy++
			}
			u.Limits.widen(f.in, f.out)
			um := byModel[f.model]
			if um == nil {
				um = &UsableModel{Model: f.model, Devices: []DeviceConfig{}}
				if d, ok := descs[f.model]; ok {
					d.UpdatedBy = "" // who edited it is for admins
					um.Description = &d
				}
				byModel[f.model] = um
				byDevice[f.model] = map[string]*DeviceConfig{}
			}
			um.Deployments++
			key := f.device + "|" + f.engine
			dc := byDevice[f.model][key]
			if dc == nil {
				dc = &DeviceConfig{Device: f.device, Engine: f.engine}
				byDevice[f.model][key] = dc
			}
			dc.Deployments++
			dc.Limits.widen(f.in, f.out)
			if f.healthy {
				um.Healthy++
				dc.Healthy++
			}
		}
		// A granted name nothing serves right now is a model when it has a description of its own.
		if _, ok := descs[n]; ok && !u.Base && len(facts) == 0 {
			u.Kind = KindModel
		}
		for model, um := range byModel {
			for _, dc := range byDevice[model] {
				um.Devices = append(um.Devices, *dc)
			}
			slices.SortFunc(um.Devices, func(x, y DeviceConfig) int {
				if c := deviceRank(x.Device) - deviceRank(y.Device); c != 0 {
					return c
				}
				return strings.Compare(x.Engine, y.Engine)
			})
			u.Models = append(u.Models, *um)
		}
		slices.SortFunc(u.Models, func(x, y UsableModel) int {
			if x.Deployments != y.Deployments {
				return y.Deployments - x.Deployments
			}
			return strings.Compare(x.Model, y.Model)
		})
		out.Names = append(out.Names, u)
	}
	return out, nil
}

// deviceRank orders the device classes: the cluster's first, the fleet, then the rest.
func deviceRank(d string) int {
	switch d {
	case DeviceGPU:
		return 0
	case DeviceGB10:
		return 1
	case DeviceMac:
		return 2
	default:
		return 3
	}
}

// servedModels lists every model name the deployments serve, sorted.
func (m *ModelAliases) servedModels(ctx context.Context) ([]string, error) {
	view, err := m.view.Get(ctx, m.load)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, facts := range view.deps {
		for _, f := range facts {
			seen[f.model] = true
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	slices.Sort(out)
	return out, nil
}

// getUsableModels godoc
//
//	@ID				getUsableModels
//	@Summary		The model names the caller can use
//	@Description	For everybody with LLM tokens (and admins): every name the caller can write into a request, the base aliases (through every project) and the names their projects were granted (through the projects listed). Per name its kind (alias: stays, the model behind it may change; model: a model under its own name, fixed as long as it is offered), the deployments and how many passed the last health check, the smallest and the largest prompt and answer its deployments take, and the models behind it, each with its description (null = none yet) and per device class (gpu, gb10, mac, other) the engine, limits and counts. Read from LiteLLM, at most a minute old; no addresses, hosts or sites. all-proxy-models opens every model and is no name of its own; it is not listed.
//	@Tags			llm
//	@Produce		json
//	@Security		BearerAuth
//	@Success		200	{object}	UsableModelsResponse
//	@Failure		403	{object}	ErrorResponse
//	@Failure		502	{object}	ErrorResponse
//	@Router			/v1/models/usable [get]
func (s *Server) getUsableModels(c *gin.Context) {
	cl := caller(c)
	grant, status, why := s.quota.GrantFor(c.Request.Context(), cl.Tokens)
	if status != 0 && !cl.Role.AtLeast(access.RoleFleetAdmin) {
		code := map[int]string{http.StatusForbidden: "no_quota", http.StatusServiceUnavailable: "unavailable"}[status]
		abort(c, status, code, why)
		return
	}
	v, err := s.aliases.Usable(c.Request.Context(), grant.Pools)
	if err != nil {
		s.fail(c, gatewayErrors, "usable models", err)
		return
	}
	c.JSON(http.StatusOK, v)
}

// ModelDescriptionsResponse is every description, and the model names the deployments serve right now (empty when LiteLLM is not readable), so a model without a description stands out.
type ModelDescriptionsResponse struct {
	Descriptions []modeldesc.Description `json:"descriptions"`
	Served       []string                `json:"served"`
}

// getModelDescriptions godoc
//
//	@ID				getModelDescriptions
//	@Summary		Descriptions of the served models
//	@Description	For admins: the description of each served model (GPU catalogue and Mac fleet, by the name the deployments serve it under), and the names served right now.
//	@Tags			llm
//	@Produce		json
//	@Security		BearerAuth
//	@Success		200	{object}	ModelDescriptionsResponse
//	@Failure		403	{object}	ErrorResponse
//	@Router			/v1/models/descriptions [get]
func (s *Server) getModelDescriptions(c *gin.Context) {
	list, err := s.aliases.descs.List(c.Request.Context())
	if err != nil {
		s.fail(c, internalErrors, "model descriptions", err)
		return
	}
	served, err := s.aliases.servedModels(c.Request.Context())
	if err != nil {
		s.log.Warnw("model descriptions: served models not readable", "error", err)
	}
	if served == nil {
		served = []string{}
	}
	if list == nil {
		list = []modeldesc.Description{}
	}
	c.JSON(http.StatusOK, ModelDescriptionsResponse{Descriptions: list, Served: served})
}

// putModelDescription godoc
//
//	@ID				putModelDescription
//	@Summary		Add or change a model description
//	@Description	For admins: the description of the model named in the body (model). description is Markdown; source_url must be http or https. Answers with every description.
//	@Tags			llm
//	@Accept			json
//	@Produce		json
//	@Security		BearerAuth
//	@Param			body	body		modeldesc.Description	true	"description"
//	@Success		200		{object}	ModelDescriptionsResponse
//	@Failure		400		{object}	ErrorResponse
//	@Failure		403		{object}	ErrorResponse
//	@Router			/v1/models/descriptions [put]
func (s *Server) putModelDescription(c *gin.Context) {
	var d modeldesc.Description
	if err := c.ShouldBindJSON(&d); err != nil {
		abort(c, http.StatusBadRequest, "invalid", msg.T("Ungültige Anfrage.", "Invalid request."))
		return
	}
	d, err := modeldesc.Normalize(d)
	var invalid *modeldesc.Invalid
	if errors.As(err, &invalid) {
		abort(c, http.StatusBadRequest, "invalid", invalid.Reason)
		return
	}
	d.UpdatedAt, d.UpdatedBy = time.Now().UTC(), caller(c).Email
	if s.fail(c, internalErrors, "save model description", s.aliases.descs.Save(c.Request.Context(), d)) {
		return
	}
	s.getModelDescriptions(c)
}

// deleteModelDescription godoc
//
//	@ID				deleteModelDescription
//	@Summary		Delete a model description
//	@Description	For admins: the model is then shown without a description. Answers with every description.
//	@Tags			llm
//	@Produce		json
//	@Security		BearerAuth
//	@Param			model	query		string	true	"served model name"
//	@Success		200		{object}	ModelDescriptionsResponse
//	@Failure		400		{object}	ErrorResponse
//	@Failure		403		{object}	ErrorResponse
//	@Router			/v1/models/descriptions [delete]
func (s *Server) deleteModelDescription(c *gin.Context) {
	model := strings.TrimSpace(c.Query("model"))
	if model == "" {
		abort(c, http.StatusBadRequest, "invalid", msg.T("Der Modellname fehlt.", "The model name is missing."))
		return
	}
	if s.fail(c, internalErrors, "delete model description", s.aliases.descs.Delete(c.Request.Context(), model)) {
		return
	}
	s.getModelDescriptions(c)
}
