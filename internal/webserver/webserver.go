// Package webserver exposes the HTTP API.
package webserver

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/cloud-self-service-golib/authn"
	"github.com/pfisterer/cloud-self-service-golib/ginweb"
	"github.com/pfisterer/llm-management-api/internal/access"
	"github.com/pfisterer/llm-management-api/internal/fleet"
	"github.com/pfisterer/llm-management-api/internal/gpu"
	"github.com/pfisterer/llm-management-api/internal/keys"
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
	DevMode bool
	// DevAuthUser is who a request without a bearer is in development mode.
	DevAuthUser string
	Version     string
	SwaggerJSON string
	ChatURL     string
	APIURL      string
	AdminUIURL  string
	Verifier    TokenVerifier // nil only in development mode
	Access      *access.Service
	Keys        *keys.Service
	// Quota decides a person's LLM pools and their limits, from the tree's projects and the configured defaults.
	Quota LLMQuota
	// TreeToken lets the budget tree (resource-management-api) manage its projects under /v1/tree; empty = off.
	TreeToken string
	// OnTreeChange runs after the tree created, changed or removed a project (the budget sync's nudge); nil = nothing.
	OnTreeChange func()
	Fleet        *fleet.Service
	Health       fleet.HealthSource
	Material     fleet.Material
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
	devUser  string
	version  string
	swagger  string
	chatURL  string
	apiURL   string
	adminUI  string
	verifier TokenVerifier
	access   *access.Service
	keys     *keys.Service
	quota    LLMQuota
	treeTok  string
	onTree   func()
	fleet    *fleet.Service
	health   fleet.HealthSource
	material fleet.Material
	gpu      *gpu.Service
	usage    usage.Store
	ws       *workspace.Service
	log      *zap.SugaredLogger
}

func New(o Options) *Server {
	return &Server{devMode: o.DevMode, devUser: strings.ToLower(strings.TrimSpace(o.DevAuthUser)), version: o.Version, swagger: o.SwaggerJSON, chatURL: o.ChatURL, apiURL: o.APIURL, adminUI: o.AdminUIURL,
		verifier: o.Verifier, access: o.Access, keys: o.Keys, quota: o.Quota, treeTok: o.TreeToken, onTree: o.OnTreeChange,
		fleet: o.Fleet, health: o.Health, material: o.Material, gpu: o.GPU, usage: o.Usage, ws: o.Workspaces, log: o.Log}
}

// Router builds the gin engine with all routes.
func (s *Server) Router() *gin.Engine {
	if !s.devMode {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.New()
	// Writes from another origin are refused for every route (the self-service UI reaches this API through its BFF, same origin for the browser). The UI's origin is trusted for browsers that send no Sec-Fetch-Site, since the BFF rewrites Host on the way here.
	r.Use(gin.Recovery(), s.requestLog(), ginweb.RejectCrossOriginWrites(ginweb.CrossOriginOptions{TrustedOrigins: originOf(s.adminUI), DevMode: s.devMode, Log: s.log}))

	r.GET("/health", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
	r.GET("/config.json", s.getConfig)
	r.GET("/swagger.json", func(c *gin.Context) { c.Data(http.StatusOK, "application/json", []byte(s.swagger)) })

	v1 := r.Group("/v1", ginweb.DisableCaching(), s.withCaller)
	v1.GET("/me", s.getMe)

	// No role needed: the caller's projects with LLM tokens decide (personAndGrant).
	v1.GET("/usage", s.getUsage)
	v1.GET("/keys", s.listKeys)
	v1.POST("/keys", s.createKey)
	v1.PUT("/keys/default-project", s.setDefaultProject)
	v1.DELETE("/keys/:id", s.deleteKey)

	if s.fleet != nil {
		fa := v1.Group("/fleet", require(access.RoleFleetAdmin))
		fa.GET("", s.getFleet)
		fa.GET("/inventory.csv", s.getFleetCSV)
		fa.GET("/inference", s.getFleetInference)
		fa.GET("/profile", s.getFleetProfile)
		fa.GET("/package", s.getFleetPackage)
		fa.DELETE("/package", require(access.RoleAdmin), s.deleteFleetPackage)
		fa.GET("/readme", s.getFleetReadme)
		fa.PUT("/model", s.setFleetModel)
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
		w.PUT("/projects/:slug", s.updateWorkspaceProject)
		w.POST("/projects/:slug/image/rebuild", s.rebuildProjectImage)
		w.POST("/projects/:slug/image/update", s.updateProjectImage)
		w.GET("/projects/:slug/credentials", s.listProjectCredentials)
		w.PUT("/projects/:slug/credentials", s.setProjectCredential)
		w.DELETE("/projects/:slug/credentials/:host", s.deleteProjectCredential)
		w.GET("/projects/:slug/image/log", s.getProjectImageLog)
		w.DELETE("/projects/:slug", require(access.RoleAdmin), s.deleteWorkspaceProject)
		w.GET("/storage", require(access.RoleAdmin), s.getWorkspaceStorage)
		w.GET("/gpu-hours", s.getGPUHours)
	}

	if s.gpu != nil {
		g := v1.Group("/gpu", s.requireWorkspace)
		g.GET("/servers", s.listServers)
		g.DELETE("/servers/:name", s.stopServer)
		g.GET("/servers/:name/progress", s.getServerProgress)
		if s.ws != nil {
			g.GET("/start-options", s.getStartOptions)
			g.POST("/servers", s.startServer)
		}
	}
	if s.gpu != nil && s.gpu.FleetEnabled() {
		f := v1.Group("/gpu/fleet", require(access.RoleAdmin))
		f.GET("", s.getGPUFleet)
		f.PUT("/nodes/:name", s.putGPUNode)
		f.PUT("/pools/:id", s.putGPUPool)
		f.DELETE("/pools/:id", s.deleteGPUPool)
		// The platform's own models (docs/bauplan.md 10.16).
		m := v1.Group("/inference", require(access.RoleAdmin))
		m.GET("/models", s.getModelCatalog)
		m.PUT("/models/:key", s.putCatalogModel)
		m.DELETE("/models/:key", s.deleteCatalogModel)
		m.PUT("/tiers/:tier", s.putTierModel)
	}
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

// originOf is the scheme and host of a URL, as a one-element list; none when the URL is empty or not absolute.
func originOf(raw string) []string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil
	}
	return []string{u.Scheme + "://" + u.Host}
}
