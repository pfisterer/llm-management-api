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
	"github.com/pfisterer/llm-management-api/internal/keys"
	"github.com/pfisterer/llm-management-api/internal/roleprovider"
	"go.uber.org/zap"
)

// TokenVerifier is the part of oidcauth.Verifier the middleware needs.
type TokenVerifier interface {
	Verify(ctx context.Context, raw string) (*authn.Claims, error)
}

// Options wires the server's dependencies.
type Options struct {
	DevMode      bool
	Version      string
	SwaggerJSON  string
	ChatURL      string
	APIURL       string
	AdminUIURL   string
	Verifier     TokenVerifier // nil only in development mode
	Access       *access.Service
	Keys         *keys.Service
	Tiers        map[string]keys.Tier
	Fleet        *fleet.Service
	Health       fleet.HealthSource
	Material     fleet.Material
	RoleProvider roleprovider.Provider
	Log          *zap.SugaredLogger
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
	fleet    *fleet.Service
	health   fleet.HealthSource
	material fleet.Material
	roles    roleprovider.Provider
	log      *zap.SugaredLogger
}

func New(o Options) *Server {
	return &Server{devMode: o.DevMode, version: o.Version, swagger: o.SwaggerJSON, chatURL: o.ChatURL, apiURL: o.APIURL, adminUI: o.AdminUIURL,
		verifier: o.Verifier, access: o.Access, keys: o.Keys, tiers: o.Tiers,
		fleet: o.Fleet, health: o.Health, material: o.Material, roles: o.RoleProvider, log: o.Log}
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
		fa.GET("/profile", s.getFleetProfile)
		fa.GET("/package", s.getFleetPackage)
		fa.DELETE("/package", require(access.RoleAdmin), s.deleteFleetPackage)
		fa.GET("/readme", s.getFleetReadme)
		fa.POST("/:serial/block", s.blockMachine)
		fa.POST("/:serial/unblock", s.unblockMachine)
		fa.DELETE("/:serial", s.forgetMachine)
	}

	admin := v1.Group("", require(access.RoleAdmin))
	admin.GET("/access-rules", s.listRules)
	admin.POST("/access-rules", s.createRule)
	admin.PUT("/access-rules/:id", s.updateRule)
	admin.DELETE("/access-rules/:id", s.deleteRule)
	admin.GET("/tiers", s.listTiers)
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
