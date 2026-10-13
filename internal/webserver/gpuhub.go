package webserver

import (
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
	Log       *zap.SugaredLogger
}

// HubProject is a project as the hub needs it for its profile list.
type HubProject struct {
	Slug       string            `json:"slug"`
	Name       string            `json:"name"`
	Profiles   []string          `json:"profiles"`
	Images     []workspace.Image `json:"images"`
	FreeImages bool              `json:"free_images"`
	// StorageGB is the size of the project's volume (PVC ws-<slug>) the hub creates and grows; 0 = no volume.
	StorageGB int `json:"storage_gb"`
	// EndsAt and DeleteAt: the project's last day and the day its files are deleted, for the warning in the start form; nil = none.
	EndsAt   *time.Time `json:"ends_at,omitempty"`
	DeleteAt *time.Time `json:"delete_at,omitempty"`
}

// GPUHubAccessResponse is the access decision for one person, for JupyterHub: whether they may log in (a project, or admin), and how many GPUs they may use at the same time (the most any of their projects allows per person).
type GPUHubAccessResponse struct {
	Allowed  bool         `json:"allowed"`
	MaxGPUs  int          `json:"max_gpus"`
	Admin    bool         `json:"admin"`
	Projects []HubProject `json:"projects"`
	// GetProjectURL is where somebody without a project gets one (self-service start page); set when not allowed.
	GetProjectURL string `json:"get_project_url,omitempty"`
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
			for _, m := range mine {
				resp.Projects = append(resp.Projects, HubProject{Slug: m.Slug, Name: m.Name, Profiles: m.Profiles, Images: m.Images, FreeImages: m.FreeImages, StorageGB: m.StorageGB, EndsAt: m.EndsAt, DeleteAt: m.DeleteAt})
				resp.MaxGPUs = max(resp.MaxGPUs, m.Limits.GPUsPerPerson)
			}
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
		// Before a start: the LLM key of the person's workspaces in the project (keys.WorkspaceKey), with the API address and the models it may use, for VS Code's chat and OPENAI_API_KEY. A project without LLM tokens (or a person the LLM does not know yet) answers 200 with an empty key and the reason: the server starts without.
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
			g, status, msg := o.Quota.GrantFor(ctx, o.Access.Who(ctx, email).Tokens)
			if status != 0 {
				c.JSON(http.StatusOK, WorkspaceKeyResponse{Reason: msg})
				return
			}
			key, err := o.Keys.EnsureWorkspaceKey(ctx, keys.Person{Subject: sub, Email: email}, g, o.MasterKey, req.Project)
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
			resp := WorkspaceKeyResponse{Key: key, BaseURL: o.APIURL, Models: models}
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
}

// WorkspaceKeyResponse is the key with where and what it may ask; Key empty with a Reason when there is none.
type WorkspaceKeyResponse struct {
	Key          string   `json:"key,omitempty"`
	BaseURL      string   `json:"base_url,omitempty"`
	Models       []string `json:"models,omitempty"`
	DefaultModel string   `json:"default_model,omitempty"`
	Reason       string   `json:"reason,omitempty"`
}

func hubToken(c *gin.Context, token string) bool {
	raw, ok := authn.CutBearerPrefix(c.GetHeader("Authorization"))
	if !ok || token == "" || subtle.ConstantTimeCompare([]byte(raw), []byte(token)) != 1 {
		abort(c, http.StatusUnauthorized, "unauthenticated", "Token fehlt oder ist falsch.")
		return false
	}
	return true
}
