package webserver

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/llm-management-api/internal/fleet"
	"go.uber.org/zap"
)

// MetricsRouter serves GET /metrics for the in-cluster Prometheus agent: the fleet view (fleet.Service.Metrics).
//
// Its own listener, without a token: no ingress points at it, and the NetworkPolicy admits only the monitoring namespace on its port. The series carry machine names, locations and states, nothing that grants access, but they are no business of the internet either.
func MetricsRouter(svc *fleet.Service, log *zap.SugaredLogger, devMode bool) *gin.Engine {
	if !devMode {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.New()
	r.Use(gin.Recovery())
	r.GET("/metrics", func(c *gin.Context) {
		body, err := svc.Metrics(c.Request.Context())
		if err != nil {
			// A failed scrape (up == 0) rather than a partial page: missing series would look like machines that went away.
			log.Warnw("metrics: fleet not readable", "error", err)
			c.String(http.StatusInternalServerError, "fleet not readable\n")
			return
		}
		c.Data(http.StatusOK, "text/plain; version=0.0.4; charset=utf-8", body)
	})
	return r
}
