package webserver

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/llm-management-api/internal/fleet"
	"github.com/pfisterer/llm-management-api/internal/gpu"
	"github.com/pfisterer/llm-management-api/internal/promtext"
	"github.com/pfisterer/llm-management-api/internal/workspace"
	"go.uber.org/zap"
)

// MetricsOptions are the parts of GET /metrics. Fleet is always there; the others are nil when the service runs without them (no GPU part, no projects).
type MetricsOptions struct {
	Fleet *fleet.Service
	// Workspaces: start decisions (counted in memory) and the projects' state from the database (cached).
	Workspaces *workspace.Service
	// Pods: running servers, start durations, builds and prepull from the cluster (cached).
	Pods *gpu.WorkspaceMetrics
	// Budgets: keys and people as the last budget sync counted them.
	Budgets *BudgetSync
	// CacheTTL is how long the parts read from the database and the cluster are kept; 0 = 30 seconds.
	CacheTTL time.Duration
	Log      *zap.SugaredLogger
	DevMode  bool
}

// MetricsRouter serves GET /metrics for the in-cluster Prometheus agent: the fleet view (fleet.Service.Metrics), then the workspaces, the cluster's workspace pods and the LLM keys where the service has them.
//
// Its own listener, without a token: no ingress points at it, and the NetworkPolicy admits only the monitoring namespace on its port. The series carry machine names, locations and states, project slugs, profiles and GPU classes, nothing that grants access and nothing about a person, but they are no business of the internet either.
//
// The parts read from the database or the cluster are cached for CacheTTL, so a scrape (or several at once) costs at most one round of reads per TTL. A failing part other than the fleet is left out and reported as aiaas_metrics_part_up 0; the fleet fails the whole scrape, as before.
func MetricsRouter(o MetricsOptions) *gin.Engine {
	if !o.DevMode {
		gin.SetMode(gin.ReleaseMode)
	}
	ttl := o.CacheTTL
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	type part struct {
		name  string
		cache *promtext.Cache
	}
	var parts []part
	if o.Workspaces != nil {
		parts = append(parts, part{"workspaces", promtext.NewCache(ttl, o.Workspaces.Metrics)})
	}
	if o.Pods != nil {
		parts = append(parts, part{"workspace_pods", promtext.NewCache(ttl, o.Pods.Render)})
	}
	render := func(ctx context.Context) ([]byte, error) {
		body, err := o.Fleet.Metrics(ctx)
		if err != nil {
			return nil, err
		}
		var w promtext.Writer
		w.Write(body)
		if o.Workspaces != nil {
			o.Workspaces.WriteStartMetrics(&w)
		}
		up := map[string]bool{}
		for _, p := range parts {
			b, err := p.cache.Get(ctx)
			if err != nil {
				o.Log.Warnw("metrics: part not readable", "part", p.name, "error", err)
				continue
			}
			up[p.name] = true
			w.Write(b)
		}
		if o.Budgets != nil {
			o.Budgets.WriteMetrics(&w)
		}
		if len(parts) > 0 {
			w.Header("aiaas_metrics_part_up", "gauge", "1 when a part of this page (workspaces: the projects from the database; workspace_pods: the workspace pods from the cluster) could be read at its last refresh; its series are missing while 0.")
			for _, p := range parts {
				v := 0.0
				if up[p.name] {
					v = 1
				}
				w.Sample("aiaas_metrics_part_up", v, "part", p.name)
			}
		}
		return w.Bytes(), nil
	}
	r := gin.New()
	r.Use(gin.Recovery())
	r.GET("/metrics", func(c *gin.Context) {
		body, err := render(c.Request.Context())
		if err != nil {
			// A failed scrape (up == 0) rather than a partial page: missing series would look like machines that went away.
			o.Log.Warnw("metrics: fleet not readable", "error", err)
			c.String(http.StatusInternalServerError, "fleet not readable\n")
			return
		}
		c.Data(http.StatusOK, "text/plain; version=0.0.4; charset=utf-8", body)
	})
	return r
}
