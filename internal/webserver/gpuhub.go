package webserver

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/cloud-self-service-golib/authn"
	"github.com/pfisterer/llm-management-api/internal/access"
	"github.com/pfisterer/llm-management-api/internal/gpu"
	"github.com/pfisterer/llm-management-api/internal/keys"
	"github.com/pfisterer/llm-management-api/internal/usage"
	"github.com/pfisterer/llm-management-api/internal/workspace"
	"go.uber.org/zap"
)

// GPUHubOptions wires the listener JupyterHub asks for a person's GPU access.
type GPUHubOptions struct {
	Access *access.Service
	Token  string
	// Workspace usage: the daily home sizes arrive here (POST /workspaces/homes). nil = route off.
	Usage   usage.Store
	Project usage.ProjectOf
	// Projects: what the hub offers at start (access answer) and checks before a start (spawn-check). nil = projects off.
	Workspaces *workspace.Service
	// SelfServiceURL is where people get a project; passed on to the hub for those without one.
	SelfServiceURL string
	// Workspace keys (POST /gpu/hub/workspace-key): the LLM key a server gets for its project. Keys nil = route off.
	Keys      *keys.Service
	Quota     LLMQuota
	MasterKey string
	Gateway   string // LiteLLM inference gateway inside the cluster, asked for the key's models
	APIURL    string // the LLM API as people reach it, e.g. https://api.llm.services.dhbw.cloud/v1
	// Limits gives the longest prompt and answer per model name (ModelAliases.Limits), for VS Code's maxInputTokens and the agents' configuration in the workspace. nil or unreadable = the answer carries none and the hub uses its defaults.
	Limits interface {
		Limits(ctx context.Context) (map[string]ModelLimit, error)
	}
	// GPU kernels (/gpu/kernels): the notebook servers of "CPU + GPU-Kernel" ask here for their kernel pods, with their own hub token instead of the shared one. nil = routes off.
	Kernels *gpu.Service
	Log     *zap.SugaredLogger
}

// HubProject is a project as the hub needs it for its profile list.
type HubProject struct {
	Slug     string   `json:"slug"`
	Name     string   `json:"name"`
	Profiles []string `json:"profiles"`
	// ProjectImage is what the project's servers run in every profile (its environment); ImageDerived = built on a base (JupyterLab, VS Code, GPU kernel), else it runs as it is, without "CPU + GPU-Kernel"; ImageLabel names it in the start form.
	ProjectImage string `json:"project_image,omitempty"`
	ImageDerived bool   `json:"image_derived"`
	ImageLabel   string `json:"image_label,omitempty"`
	// StorageGB is the size of the project's volume (PVC ws-<slug>) the hub creates and grows; 0 = no volume.
	StorageGB int `json:"storage_gb"`
	// GPUClasses the project may use, for the choice at the start (only offered with more than one).
	GPUClasses []HubGPUClass `json:"gpu_classes"`
	// SharedReadOnly: the person only reads the project's shared folder (role reader); the hub mounts it read-only, the GPU kernel pod follows the server.
	SharedReadOnly bool `json:"shared_read_only"`
	// EndsAt and DeleteAt: the project's last day and the day its files are deleted, for the warning in the start form; nil = none.
	EndsAt   *time.Time `json:"ends_at,omitempty"`
	DeleteAt *time.Time `json:"delete_at,omitempty"`
	// ImagePending: the environment is built from Git and has no finished image yet; starts are refused until it has. ImageBuild is the build's state (building, failed with its message), when there is one.
	ImagePending bool                  `json:"image_pending,omitempty"`
	ImageBuild   *workspace.ImageBuild `json:"image_build,omitempty"`
	// MaterialGitURL and MaterialGitRef are the project's course material, which the hub checks out into ~/projects/<repo> (empty ref = default branch); empty = none.
	MaterialGitURL string `json:"material_git_url,omitempty"`
	MaterialGitRef string `json:"material_git_ref,omitempty"`
	// ImageArchs are the architectures the environment runs on (amd64, arm64): the hub keeps servers on nodes of these.
	ImageArchs []string `json:"image_archs"`
	// PullSecret and GitSecret are the project's secrets for private images and repositories (credentials); empty = none.
	PullSecret string `json:"pull_secret,omitempty"`
	GitSecret  string `json:"git_secret,omitempty"`
	// Release is what the project's servers (per profile) and GPU kernels enforce, in seconds (docs/todo.md idle_release): the hub sets the servers' idle times and upper bound, the bases' extension dhbw_gpu culls by them.
	Release workspace.EffectiveRelease `json:"release"`
	// ScratchGB is the local storage in GB the project's servers and GPU kernels may write outside the home (workspace.ScratchGB: the budget's or the default): the hub sets it as the notebook container's ephemeral-storage limit, the start page shows it.
	ScratchGB int `json:"scratch_gb" example:"1"`
}

// HubLLMProject is a project with LLM tokens, as the start form offers it.
type HubLLMProject struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// llmProjects are the pools of the person's grant that have tokens, in the grant's order; none when the grant cannot be read.
func llmProjects(ctx context.Context, q LLMQuota, tokens []string) []HubLLMProject {
	out := []HubLLMProject{}
	g, status, _ := q.GrantFor(ctx, tokens)
	if status != 0 {
		return out
	}
	for _, p := range g.Pools {
		if p.MTokens > 0 {
			out = append(out, HubLLMProject{Slug: p.Slug, Name: p.Name})
		}
	}
	return out
}

// llmProjectFor is the project whose tokens a workspace key draws from: the chosen one, else the workspace project itself, else the first LLM project; "" when the person has none. A chosen project that is no LLM project of theirs gives "" and false.
func llmProjectFor(llm []HubLLMProject, chosen, project string) (string, bool) {
	has := func(slug string) bool {
		return slices.ContainsFunc(llm, func(p HubLLMProject) bool { return p.Slug == slug })
	}
	switch {
	case chosen != "":
		return chosen, has(chosen)
	case has(project):
		return project, true
	case len(llm) > 0:
		return llm[0].Slug, true
	}
	return "", true
}

// HubGPUClass is a GPU class in the start form: its name, and whether it has GPUs right now.
type HubGPUClass struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Available bool   `json:"available"`
	// Hardware says what a GPU of the class is (e.g. "L4-3Q · 3 GB"), shown dimmed next to the name.
	Hardware string `json:"hardware,omitempty"`
}

// GPUHubAccessResponse is the access decision for one person, for JupyterHub: whether they may log in (a project, or admin), and how many GPUs they may use at the same time (the most any of their projects allows per person).
type GPUHubAccessResponse struct {
	Allowed  bool         `json:"allowed"`
	MaxGPUs  int          `json:"max_gpus"`
	Admin    bool         `json:"admin"`
	Projects []HubProject `json:"projects"`
	// LLMProjects are the person's projects with LLM tokens: the choice at the start of whose tokens the server's key draws from (VS Code's chat, the agents, OPENAI_API_KEY); empty = no LLM in the workspaces, the choice is not offered.
	LLMProjects []HubLLMProject `json:"llm_projects"`
	// GetProjectURL is where somebody without a project gets one (self-service start page); set when not allowed.
	GetProjectURL string `json:"get_project_url,omitempty"`
}

// hubProjects is how a start sees the person's projects: what the hub offers and the self-service UI's start page (GET /v1/gpu/start-options).
func hubProjects(ctx context.Context, ws *workspace.Service, mine []workspace.Mine) []HubProject {
	classes := ws.GPUClasses(ctx)
	out := []HubProject{}
	for _, m := range mine {
		hubClasses := []HubGPUClass{}
		archs := ws.EffectiveArchs(m.Project)
		for _, id := range m.GPUClasses {
			hc := HubGPUClass{ID: id, Name: id, Available: classes == nil}
			fits := true
			for _, cl := range classes {
				if cl.ID == id {
					hc.Name, hc.Available, hc.Hardware = cl.Name, cl.GPUs > 0, cl.Hardware
					// A class whose nodes the environment has no image for (an amd64 image, GB10 nodes) is not offered.
					fits = len(cl.Archs) == 0 || slices.ContainsFunc(cl.Archs, func(a string) bool { return slices.Contains(archs, a) })
				}
			}
			if fits {
				hubClasses = append(hubClasses, hc)
			}
		}
		pull, git := "", ""
		if ws.HasCredentials(ctx, m.Slug) {
			pull, git = workspace.PullSecretName(m.Slug), workspace.GitSecretName(m.Slug)
		}
		out = append(out, HubProject{PullSecret: pull, GitSecret: git, GPUClasses: hubClasses, Slug: m.Slug, Name: m.Name, Profiles: offeredProfiles(m), ProjectImage: ws.EffectiveImage(m.Project), ImageDerived: m.Derived(), ImageLabel: ws.ImageLabel(m.Project), StorageGB: m.StorageGB, SharedReadOnly: m.Role == workspace.RoleReader, EndsAt: m.EndsAt, DeleteAt: m.DeleteAt, ImagePending: m.ImagePending(), ImageBuild: m.ImageBuild, MaterialGitURL: m.MaterialGitURL, MaterialGitRef: m.MaterialGitRef, ImageArchs: archs, Release: m.ReleaseEffective, ScratchGB: m.ScratchEffectiveGB})
	}
	return out
}

// offeredProfiles are the profiles a project's start offers: "CPU + GPU kernel" needs an environment built on one of our bases (Derived), as the start checks, so a project with a free image is not offered it.
func offeredProfiles(m workspace.Mine) []string {
	if m.Derived() {
		return m.Profiles
	}
	return slices.DeleteFunc(slices.Clone(m.Profiles), func(p string) bool { return p == workspace.SplitProfile })
}

// GPUHubRouter serves GET /gpu/hub/access?user=<email> for JupyterHub in the
// GPU cluster. Its own listener, reached only through the WireGuard link (a
// Service with the link address as external IP), and a shared bearer token.
func GPUHubRouter(o GPUHubOptions, devMode bool) *gin.Engine {
	if !devMode {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.New()
	r.Use(gin.Recovery())
	r.GET("/health", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
	r.GET("/gpu/hub/access", func(c *gin.Context) {
		if !hubToken(c, o.Token) {
			return
		}
		email := strings.ToLower(strings.TrimSpace(c.Query("user")))
		if email == "" {
			abort(c, http.StatusBadRequest, "invalid", "user fehlt.")
			return
		}
		who := o.Access.Who(c.Request.Context(), email)
		resp := GPUHubAccessResponse{Admin: who.Role.AtLeast(access.RoleAdmin), Projects: []HubProject{}}
		if o.Workspaces != nil {
			mine, err := o.Workspaces.ForPerson(c.Request.Context(), who.Tokens, "", false)
			if err != nil {
				o.Log.Errorw("gpu hub projects failed", "email", email, "error", err)
				abort(c, http.StatusInternalServerError, "internal", "Projekte konnten nicht gelesen werden.")
				return
			}
			resp.Projects = hubProjects(c.Request.Context(), o.Workspaces, mine)
			for _, m := range mine {
				resp.MaxGPUs = max(resp.MaxGPUs, m.Limits.GPUsPerPerson)
			}
		}
		resp.LLMProjects = []HubLLMProject{}
		if o.Quota.Projects != nil {
			resp.LLMProjects = llmProjects(c.Request.Context(), o.Quota, who.Tokens)
		}
		resp.Allowed = len(resp.Projects) > 0 || resp.Admin
		if !resp.Allowed {
			resp.GetProjectURL = o.SelfServiceURL
		}
		c.JSON(http.StatusOK, resp)
	})
	if o.Workspaces != nil {
		// Before every start: may the person start this profile in this project now? A refusal is 200 with allowed=false and the reason JupyterHub shows.
		r.POST("/gpu/hub/spawn-check", func(c *gin.Context) {
			if !hubToken(c, o.Token) {
				return
			}
			var req workspace.StartRequest
			if err := c.ShouldBindJSON(&req); err != nil || req.Person == "" {
				abort(c, http.StatusBadRequest, "invalid", "user, project und profile fehlen.")
				return
			}
			req.Person = strings.ToLower(strings.TrimSpace(req.Person))
			err := o.Workspaces.CheckStart(c.Request.Context(), o.Access.Who(c.Request.Context(), req.Person).Tokens, req)
			var refusal *workspace.Refusal
			switch {
			case err == nil:
				c.JSON(http.StatusOK, gin.H{"allowed": true})
			case errors.As(err, &refusal):
				c.JSON(http.StatusOK, gin.H{"allowed": false, "reason": refusal.Reason})
			default:
				o.Log.Errorw("spawn check failed", "user", req.Person, "error", err)
				abort(c, http.StatusInternalServerError, "internal", "Start konnte nicht geprüft werden.")
			}
		})
	}
	if o.Kernels != nil {
		kernelRoutes(r, o)
	}
	if o.Usage != nil && o.Project != nil {
		// The output of `juicefs quota list` from the hourly CronJob workspace-storage-usage (tasks/juicefs.yaml), as text: parsing stays here, the job stays a one-liner. Booked as the day's home sizes (the largest of the day counts) and kept for the admins' storage view.
		r.POST("/workspaces/homes", func(c *gin.Context) {
			if !hubToken(c, o.Token) {
				return
			}
			raw, err := io.ReadAll(io.LimitReader(c.Request.Body, 4<<20))
			if err != nil {
				abort(c, http.StatusBadRequest, "invalid", "Body nicht lesbar.")
				return
			}
			used, err := usage.ParseQuotaList(string(raw))
			if err != nil {
				abort(c, http.StatusBadRequest, "invalid", err.Error())
				return
			}
			if o.Workspaces != nil {
				if dirs, ok := workspace.ParseQuotaTable(string(raw)); ok {
					if err := o.Workspaces.SetStorageDirs(c.Request.Context(), dirs); err != nil {
						o.Log.Warnw("storage report not saved", "error", err)
					}
				}
			}
			rows, err := usage.HomeRows(c.Request.Context(), o.Usage, used, o.Project, time.Now())
			if err == nil {
				err = o.Usage.Add(c.Request.Context(), rows)
			}
			if err != nil {
				o.Log.Errorw("home usage failed", "error", err)
				abort(c, http.StatusInternalServerError, "internal", "Speichern fehlgeschlagen.")
				return
			}
			c.JSON(http.StatusOK, gin.H{"homes": len(rows)})
		})
	}
	if o.Keys != nil {
		// Before a start: the LLM key of the person's workspaces in the project (keys.WorkspaceKey), with the API address and the models it may use, for VS Code's chat and OPENAI_API_KEY. The key draws only from the project's own LLM tokens and only for its members. A person who is no member gets 403; a project without LLM tokens (or a person the LLM does not know yet) gets 200 with an empty key and the reason: the server starts without, and the hub removes a provider "DHBW" left in VS Code's settings. Only that answer means "definitely no key": anything that could not be found out (LiteLLM, projects not readable) is an error status, on which the hub keeps the settings as they are.
		r.POST("/gpu/hub/workspace-key", func(c *gin.Context) {
			if !hubToken(c, o.Token) {
				return
			}
			var req WorkspaceKeyRequest
			if err := c.ShouldBindJSON(&req); err != nil || req.User == "" || req.Project == "" {
				abort(c, http.StatusBadRequest, "invalid", "user und project fehlen.")
				return
			}
			ctx := c.Request.Context()
			email := strings.ToLower(strings.TrimSpace(req.User))
			tokens := o.Access.Who(ctx, email).Tokens
			if o.Workspaces == nil {
				abort(c, http.StatusForbidden, "forbidden", "Projekte sind nicht eingerichtet.")
				return
			}
			proj, err := o.Workspaces.Get(ctx, req.Project)
			if err != nil && !errors.Is(err, workspace.ErrNotFound) {
				o.Log.Errorw("workspace key: project not read", "project", req.Project, "error", err)
				abort(c, http.StatusInternalServerError, "internal", "Projekt nicht lesbar.")
				return
			}
			if err != nil || proj.RoleOf(tokens) == "" {
				abort(c, http.StatusForbidden, "forbidden", "Kein Mitglied des Projekts.")
				return
			}
			sub := strings.TrimSpace(req.Subject)
			if sub == "" {
				found, ok, err := o.Keys.SubjectOf(ctx, email)
				if err != nil {
					o.Log.Errorw("workspace key: person not looked up", "user", email, "error", err)
					abort(c, http.StatusBadGateway, "upstream", "LiteLLM nicht erreichbar.")
					return
				}
				if !ok {
					c.JSON(http.StatusOK, WorkspaceKeyResponse{Reason: "person unknown to the LLM service"})
					return
				}
				sub = found
			}
			g, status, msg := o.Quota.GrantFor(ctx, tokens)
			if status >= 500 {
				// Projects not readable: an error, not an answer. The hub keeps VS Code's settings as they are (only a 200 with a reason removes the provider "DHBW").
				abort(c, http.StatusServiceUnavailable, "upstream", msg)
				return
			}
			if status != 0 {
				c.JSON(http.StatusOK, WorkspaceKeyResponse{Reason: msg})
				return
			}
			var llm []HubLLMProject
			for _, p := range g.Pools {
				if p.MTokens > 0 {
					llm = append(llm, HubLLMProject{Slug: p.Slug, Name: p.Name})
				}
			}
			target, ok := llmProjectFor(llm, strings.TrimSpace(req.LLMProject), req.Project)
			switch {
			case !ok:
				c.JSON(http.StatusOK, WorkspaceKeyResponse{Reason: "the chosen LLM project is none of the person's projects with LLM tokens"})
				return
			case target == "":
				c.JSON(http.StatusOK, WorkspaceKeyResponse{Reason: "no project with LLM tokens"})
				return
			}
			key, err := o.Keys.EnsureWorkspaceKey(ctx, keys.Person{Subject: sub, Email: email}, g, o.MasterKey, target)
			if errors.Is(err, keys.ErrNotEligible) {
				c.JSON(http.StatusOK, WorkspaceKeyResponse{Reason: "project has no LLM tokens"})
				return
			}
			if err != nil {
				o.Log.Errorw("workspace key failed", "user", email, "project", req.Project, "error", err)
				abort(c, http.StatusBadGateway, "upstream", "Key konnte nicht angelegt werden.")
				return
			}
			models, err := keys.ModelsOf(ctx, o.Gateway, key)
			if err != nil {
				o.Log.Warnw("workspace key: models not listed", "project", req.Project, "error", err)
			}
			resp := WorkspaceKeyResponse{Key: key, BaseURL: o.APIURL, Models: models, LLMProject: target}
			if o.Limits != nil {
				if all, err := o.Limits.Limits(ctx); err != nil {
					o.Log.Warnw("workspace key: model limits not readable", "error", err)
				} else {
					for _, m := range models {
						if l, ok := all[m]; ok {
							if resp.Limits == nil {
								resp.Limits = map[string]ModelLimit{}
							}
							resp.Limits[m] = l
						}
					}
				}
			}
			if slices.Contains(models, DefaultWorkspaceModel) {
				resp.DefaultModel = DefaultWorkspaceModel
			}
			c.JSON(http.StatusOK, resp)
		})
	}
	if o.Workspaces != nil {
		// INFO memory of the Valkey with the JuiceFS metadata, from the same CronJob: for the admins' storage view.
		r.POST("/workspaces/valkey", func(c *gin.Context) {
			if !hubToken(c, o.Token) {
				return
			}
			raw, err := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20))
			if err != nil {
				abort(c, http.StatusBadRequest, "invalid", "Body nicht lesbar.")
				return
			}
			m, ok := workspace.ParseValkeyInfo(string(raw))
			if !ok {
				abort(c, http.StatusBadRequest, "invalid", "Keine Ausgabe von INFO memory.")
				return
			}
			if err := o.Workspaces.SetValkeyMemory(c.Request.Context(), m); err != nil {
				o.Log.Warnw("valkey report not saved", "error", err)
			}
			c.JSON(http.StatusOK, gin.H{"used_bytes": m.UsedBytes})
		})
	}
	return r
}

// DefaultWorkspaceModel is the model VS Code's chat starts with, when the key may use it.
const DefaultWorkspaceModel = "code"

// WorkspaceKeyRequest asks for the workspace key of a person in a project. Subject is the Keycloak sub from the hub's login (auth state); without it the person is looked up by e-mail.
type WorkspaceKeyRequest struct {
	User    string `json:"user"`
	Subject string `json:"sub"`
	Project string `json:"project"`
	// LLMProject is the project whose LLM tokens the key draws from, as chosen at the start; empty = the project itself, else the person's first LLM project.
	LLMProject string `json:"llm_project"`
}

// WorkspaceKeyResponse is the key with where and what it may ask; Key empty with a Reason when there is none.
type WorkspaceKeyResponse struct {
	Key          string   `json:"key,omitempty"`
	BaseURL      string   `json:"base_url,omitempty"`
	// LLMProject is the project the key draws from.
	LLMProject string `json:"llm_project,omitempty"`
	Models       []string `json:"models,omitempty"`
	DefaultModel string   `json:"default_model,omitempty"`
	// Limits per model of Models, where its deployments declare them; a model without one is missing.
	Limits map[string]ModelLimit `json:"limits,omitempty"`
	Reason string                `json:"reason,omitempty"`
}

func hubToken(c *gin.Context, token string) bool {
	raw, ok := authn.CutBearerPrefix(c.GetHeader("Authorization"))
	if !ok || token == "" || subtle.ConstantTimeCompare([]byte(raw), []byte(token)) != 1 {
		abort(c, http.StatusUnauthorized, "unauthenticated", "Token fehlt oder ist falsch.")
		return false
	}
	return true
}
