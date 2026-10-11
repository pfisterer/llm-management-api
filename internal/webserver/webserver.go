// Package webserver exposes the HTTP API.
package webserver

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/cloud-self-service-golib/authn"
	"github.com/pfisterer/cloud-self-service-golib/ginweb"
	"github.com/pfisterer/llm-management-api/internal/access"
	"github.com/pfisterer/llm-management-api/internal/fleet"
	"github.com/pfisterer/llm-management-api/internal/gpu"
	"github.com/pfisterer/llm-management-api/internal/keys"
	"github.com/pfisterer/llm-management-api/internal/roleprovider"
	"github.com/pfisterer/llm-management-api/internal/usage"
	"github.com/pfisterer/llm-management-api/internal/workspace"
	"go.uber.org/zap"
)

// TokenVerifier is the part of oidcauth.Verifier the middleware needs.
type TokenVerifier interface {
	Verify(ctx context.Context, raw string) (*authn.Claims, error)
}

// Options wires the server's dependencies.
type Options struct {
	DevMode     bool
	Version     string
	SwaggerJSON string
	ChatURL     string
	APIURL      string
	AdminUIURL  string
	Verifier    TokenVerifier // nil only in development mode
	Access      *access.Service
	Keys        *keys.Service
	Tiers       map[string]keys.Tier
	// Quota decides a person's LLM tier (from the access rule, or with the budget from the tree); zero = from Tiers.
	Quota LLMQuota
	// TreeToken lets the budget tree (resource-management-api) manage its projects under /v1/tree; empty = off.
	TreeToken    string
	Fleet        *fleet.Service
	Health       fleet.HealthSource
	Material     fleet.Material
	RoleProvider roleprovider.Provider
	// GPU part; nil when disabled (no /v1/gpu routes).
	GPU *gpu.Service
	// Workspace usage (docs/bauplan.md, Stufe 4); nil without the GPU part.
	Usage usage.Store
	// Workspace projects; nil without the GPU part.
	Workspaces *workspace.Service
	Log        *zap.SugaredLogger
}

type Server struct {
	devMode  bool
	version  string
	swagger  string
	chatURL  string
	apiURL   string
	adminUI  string
	verifier TokenVerifier
	access   *access.Service
	keys     *keys.Service
	tiers    map[string]keys.Tier
	quota    LLMQuota
	treeTok  string
	fleet    *fleet.Service
	health   fleet.HealthSource
	material fleet.Material
	roles    roleprovider.Provider
	gpu      *gpu.Service
	usage    usage.Store
	ws       *workspace.Service
	log      *zap.SugaredLogger
}

func New(o Options) *Server {
	srv := &Server{devMode: o.DevMode, version: o.Version, swagger: o.SwaggerJSON, chatURL: o.ChatURL, apiURL: o.APIURL, adminUI: o.AdminUIURL,
		verifier: o.Verifier, access: o.Access, keys: o.Keys, tiers: o.Tiers, quota: o.Quota, treeTok: o.TreeToken,
		fleet: o.Fleet, health: o.Health, material: o.Material, roles: o.RoleProvider, gpu: o.GPU, usage: o.Usage, ws: o.Workspaces, log: o.Log}
	if srv.quota.Tiers == nil {
		srv.quota.Tiers = o.Tiers
	}
	return srv
}

// Router builds the gin engine with all routes.
func (s *Server) Router() *gin.Engine {
	if !s.devMode {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.New()
	r.Use(gin.Recovery(), s.requestLog())

	r.GET("/health", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
	r.GET("/config.json", s.getConfig)
	r.GET("/swagger.json", func(c *gin.Context) { c.Data(http.StatusOK, "application/json", []byte(s.swagger)) })

	v1 := r.Group("/v1", ginweb.DisableCaching(), s.withCaller)
	v1.GET("/me", s.getMe)

	user := v1.Group("", require(access.RoleUser))
	user.GET("/usage", s.getUsage)
	user.GET("/keys", s.listKeys)
	user.POST("/keys", s.createKey)
	user.DELETE("/keys/:id", s.deleteKey)

	if s.fleet != nil {
		fa := v1.Group("/fleet", require(access.RoleFleetAdmin))
		fa.GET("", s.getFleet)
		fa.GET("/inventory.csv", s.getFleetCSV)
		fa.GET("/inference", s.getFleetInference)
		fa.GET("/profile", s.getFleetProfile)
		fa.GET("/package", s.getFleetPackage)
		fa.DELETE("/package", require(access.RoleAdmin), s.deleteFleetPackage)
		fa.GET("/readme", s.getFleetReadme)
		fa.POST("/:serial/block", s.blockMachine)
		fa.POST("/:serial/unblock", s.unblockMachine)
		fa.DELETE("/:serial", s.forgetMachine)
	}

	if s.ws != nil && s.treeTok != "" {
		// The budget tree manages its projects with a token of its own, not as a person.
		t := r.Group("/v1/tree", ginweb.DisableCaching(), s.requireTreeToken)
		t.GET("/projects", s.listTreeProjects)
		t.PUT("/projects/:node", s.putTreeProject)
		t.DELETE("/projects/:node", s.deleteTreeProject)
		t.GET("/catalog", s.getTreeCatalog)
	}

	if s.gpu != nil && s.usage != nil && s.ws != nil {
		w := v1.Group("/workspaces")
		w.GET("/usage/me", s.requireWorkspace, s.getMyWorkspaceUsage)
		// Admins: everything; project managers: their project (?project=).
		w.GET("/usage", s.getWorkspaceUsage)
		w.GET("/catalog", s.requireWorkspace, s.getWorkspaceCatalog)
		w.GET("/projects", s.listWorkspaceProjects)
		w.POST("/projects", require(access.RoleAdmin), s.createWorkspaceProject)
		w.PUT("/projects/:slug", s.updateWorkspaceProject)
		w.DELETE("/projects/:slug", require(access.RoleAdmin), s.deleteWorkspaceProject)
	}

	if s.gpu != nil {
		g := v1.Group("/gpu", s.requireWorkspace)
		g.GET("/environments", s.listEnvironments)
		g.POST("/environments", s.createEnvironment)
		g.GET("/environments/:id", s.getEnvironment)
		g.DELETE("/environments/:id", s.deleteEnvironment)
		g.GET("/environments/:id/log", s.getEnvironmentLog)
		g.POST("/environments/:id/rebuild", s.rebuildEnvironment)
		g.POST("/environments/:id/start", s.startEnvironment)
		g.GET("/servers", s.listServers)
		g.DELETE("/servers/:name", s.stopServer)
	}

	admin := v1.Group("", require(access.RoleAdmin))
	admin.GET("/access-rules", s.listRules)
	admin.POST("/access-rules", s.createRule)
	admin.PUT("/access-rules/:id", s.updateRule)
	admin.DELETE("/access-rules/:id", s.deleteRule)
	admin.GET("/tiers", s.listTiers)
	admin.GET("/gpu-tiers", s.listGPUTiers)
	admin.GET("/principals/search", s.searchPrincipals)
	return r
}

// requestLog writes one line per request — without query strings, which may
// carry search terms, and never with headers or bodies.
func (s *Server) requestLog() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		if c.Request.URL.Path == "/health" {
			return
		}
		s.log.Infow("request", "method", c.Request.Method, "path", c.FullPath(),
			"status", c.Writer.Status(), "ms", time.Since(start).Milliseconds())
	}
}

// ConfigResponse is served at /config.json (version for the UI footer).
type ConfigResponse struct {
	Name    string `json:"name" example:"llm-management-api"`
	Version string `json:"version" example:"0.1.0"`
}

func (s *Server) getConfig(c *gin.Context) {
	c.JSON(http.StatusOK, ConfigResponse{Name: "llm-management-api", Version: s.version})
}
