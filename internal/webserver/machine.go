package webserver

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/cloud-self-service-golib/authn"
	"github.com/pfisterer/llm-management-api/internal/classify"
	"github.com/pfisterer/llm-management-api/internal/fleet"
	"go.uber.org/zap"
)

// MachineRouter is the API for machines: enrolment and script downloads (the
// fleet, via the public enrolment host), the peer list for the WireGuard hub
// and the site list for the discovery job.
//
// A SEPARATE listener on purpose — a security boundary, not tidiness. The
// enrolment host is reachable without Keycloak (a machine cannot log in), so it
// must not be able to reach any person-facing route. Token authentication
// only; no identity header is read here.
func MachineRouter(svc *fleet.Service, classifier *classify.Service, log *zap.SugaredLogger, devMode bool) *gin.Engine {
	if !devMode {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.New()
	r.Use(gin.Recovery())
	r.GET("/healthz", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	cfg := svc.Config()
	enrollAuth := tokenAuth(cfg.EnrollToken)
	adminAuth := tokenAuth(cfg.AdminToken)

	r.POST("/enroll", enrollAuth, func(c *gin.Context) {
		raw, err := io.ReadAll(io.LimitReader(c.Request.Body, 16<<10))
		if err != nil {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "too large"})
			return
		}
		var req fleet.EnrollRequest
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &req); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
				return
			}
		}
		resp, err := svc.Enroll(c.Request.Context(), req)
		var ee *fleet.EnrollError
		switch {
		case errors.As(err, &ee):
			c.JSON(ee.Status, gin.H{"error": ee.Message})
		case err != nil:
			log.Errorw("enrolment failed", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		default:
			c.JSON(http.StatusOK, resp)
		}
	})

	// Scripts use the ENROLMENT token: every machine holds it already, and the
	// admin token (which reads every peer's preshared key) would be the wrong
	// trade for a software update.
	r.GET("/scripts/:name", enrollAuth, func(c *gin.Context) {
		b, err := svc.Scripts().Read(cfg, c.Param("name"))
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		c.Data(http.StatusOK, "text/x-shellscript; charset=utf-8", b)
	})

	r.GET("/fleet/peers", adminAuth, func(c *gin.Context) {
		v, err := svc.HubPeers(c.Request.Context())
		if err != nil {
			log.Errorw("hub peers failed", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
			return
		}
		c.JSON(http.StatusOK, v)
	})
	r.GET("/fleet/sites", adminAuth, func(c *gin.Context) {
		sites, err := svc.Sites(c.Request.Context())
		if err != nil {
			log.Errorw("sites failed", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"sites": sites})
	})
	// Which alias a model is offered under, for the discovery job — it knows the
	// models actually served; this side measures them through the gateway.
	// Admin token, like the peer list. May take minutes for a new model.
	r.POST("/fleet/classify", adminAuth, func(c *gin.Context) {
		var body struct {
			Models []string `json:"models"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
			return
		}
		if len(body.Models) > 100 {
			body.Models = body.Models[:100]
		}
		if classifier == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "classification not configured"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"aliases": classifier.Many(c.Request.Context(), body.Models)})
	})
	r.NoRoute(func(c *gin.Context) { c.JSON(http.StatusNotFound, gin.H{"error": "not found"}) })
	return r
}

// tokenAuth compares the bearer token in constant time. An empty expected
// token refuses everything (unconfigured is not open).
func tokenAuth(expected string) gin.HandlerFunc {
	return func(c *gin.Context) {
		got, ok := authn.CutBearerPrefix(c.GetHeader("Authorization"))
		if expected == "" || !ok || subtle.ConstantTimeCompare([]byte(got), []byte(expected)) != 1 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		c.Next()
	}
}
