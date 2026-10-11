package webserver

import (
	"crypto/subtle"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/cloud-self-service-golib/authn"
	"github.com/pfisterer/llm-management-api/internal/access"
	"github.com/pfisterer/llm-management-api/internal/gpu"
	"github.com/pfisterer/llm-management-api/internal/usage"
	"go.uber.org/zap"
)

// GPUHubOptions wires the listener JupyterHub asks for a person's GPU access.
type GPUHubOptions struct {
	Access *access.Service
	GPU    *gpu.Service
	Token  string
	// Workspace usage: the daily home sizes arrive here (POST /workspaces/homes). nil = route off.
	Usage   usage.Store
	Project usage.ProjectOf
	Log     *zap.SugaredLogger
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
		if !hubToken(c, o.Token) {
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
	if o.Usage != nil && o.Project != nil {
		// The output of `juicefs quota list` from the daily CronJob workspace-home-usage (tasks/juicefs.yaml), as text: parsing stays here, the job stays a one-liner.
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
	return r
}

func hubToken(c *gin.Context, token string) bool {
	raw, ok := authn.CutBearerPrefix(c.GetHeader("Authorization"))
	if !ok || token == "" || subtle.ConstantTimeCompare([]byte(raw), []byte(token)) != 1 {
		abort(c, http.StatusUnauthorized, "unauthenticated", "Token fehlt oder ist falsch.")
		return false
	}
	return true
}
