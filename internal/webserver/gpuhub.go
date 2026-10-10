package webserver

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/cloud-self-service-golib/authn"
	"github.com/pfisterer/llm-management-api/internal/access"
	"github.com/pfisterer/llm-management-api/internal/gpu"
	"go.uber.org/zap"
)

// GPUHubOptions wires the listener JupyterHub asks for a person's GPU access.
type GPUHubOptions struct {
	Access *access.Service
	GPU    *gpu.Service
	Token  string
	Log    *zap.SugaredLogger
}

// GPUHubAccessResponse is the access decision for one person, for JupyterHub:
// whether they may log in, and how many GPUs they may use at the same time.
type GPUHubAccessResponse struct {
	Allowed bool   `json:"allowed"`
	GPUTier string `json:"gpu_tier,omitempty"`
	MaxGPUs int    `json:"max_gpus"`
	Admin   bool   `json:"admin"`
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
		raw, ok := authn.CutBearerPrefix(c.GetHeader("Authorization"))
		if !ok || o.Token == "" || subtle.ConstantTimeCompare([]byte(raw), []byte(o.Token)) != 1 {
			abort(c, http.StatusUnauthorized, "unauthenticated", "Token fehlt oder ist falsch.")
			return
		}
		email := strings.ToLower(strings.TrimSpace(c.Query("user")))
		if email == "" {
			abort(c, http.StatusBadRequest, "invalid", "user fehlt.")
			return
		}
		d, _, err := o.Access.Decide(c.Request.Context(), email)
		if err != nil {
			o.Log.Errorw("gpu hub access decision failed", "email", email, "error", err)
			abort(c, http.StatusInternalServerError, "internal", "Zugriff konnte nicht geprüft werden.")
			return
		}
		resp := GPUHubAccessResponse{Allowed: d.GPUTier != "", GPUTier: d.GPUTier, Admin: d.Role.AtLeast(access.RoleAdmin)}
		if resp.Allowed {
			resp.MaxGPUs = o.GPU.Tier(d.GPUTier).MaxGPUs
		}
		c.JSON(http.StatusOK, resp)
	})
	return r
}
