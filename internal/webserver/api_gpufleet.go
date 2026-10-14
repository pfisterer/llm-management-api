package webserver

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/llm-management-api/internal/gpu"
)

// GPUFleetResponse is the GPU administration at a glance: the GPU nodes with their assignment, the pools, the tiers and the classes (pool and tier combinations) with their GPUs.
type GPUFleetResponse struct {
	Nodes   []gpu.GPUNode   `json:"nodes"`
	Pools   []gpu.Pool      `json:"pools"`
	Tiers   []gpu.Tier      `json:"tiers"`
	Classes []gpu.ClassInfo `json:"classes"`
	// Models are the gap filler models a node can run (keys), TierModels the default of each tier.
	Models     []string          `json:"models"`
	TierModels map[string]string `json:"tier_models"`
}

// getGPUFleet godoc
//
//	@ID				getGPUFleet
//	@Summary		GPU administration
//	@Description	For admins: the GPU nodes of the cluster (type, GPU memory, ready) with their pool and tier, the pools, the tiers and the GPU classes. A node without pool runs no workspaces and no jobs. The tree grants classes (ws-gpus-<class>, ws-gpu-hours-<class>), so new nodes of an existing class need nothing there.
//	@Tags			gpu
//	@Produce		json
//	@Security		BearerAuth
//	@Success		200	{object}	GPUFleetResponse
//	@Failure		403	{object}	ErrorResponse
//	@Router			/v1/gpu/fleet [get]
func (s *Server) getGPUFleet(c *gin.Context) {
	ctx := c.Request.Context()
	nodes, err := s.gpu.GPUNodes(ctx)
	if err == nil {
		var pools []gpu.Pool
		pools, err = s.gpu.Pools(ctx)
		if err == nil {
			var classes []gpu.ClassInfo
			classes, err = s.gpu.GPUClasses(ctx)
			if err == nil {
				c.JSON(http.StatusOK, GPUFleetResponse{Nodes: orEmpty(nodes), Pools: orEmpty(pools), Tiers: gpu.Tiers, Classes: orEmpty(classes), Models: orEmpty(s.gpu.GapModels()), TierModels: s.gpu.TierModels()})
				return
			}
		}
	}
	s.log.Errorw("gpu fleet failed", "error", err)
	abort(c, http.StatusInternalServerError, "internal", "GPU-Verwaltung konnte nicht gelesen werden.")
}

func orEmpty[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}

// GPUNodeRequest assigns a GPU node; an empty pool takes it out (no workspaces, no jobs).
type GPUNodeRequest struct {
	Pool           string     `json:"pool" example:"allgemein"`
	Tier           string     `json:"tier" example:"mittel"`
	AvailableUntil *time.Time `json:"available_until,omitempty"`
	// GapModel: inference gap filler; "" = the tier's default, "-" = none.
	GapModel string `json:"gap_model"`
}

// putGPUNode godoc
//
//	@ID				putGPUNode
//	@Summary		Assign a GPU node
//	@Description	For admins: puts a GPU node into a pool and tier (the class is created on first use) or, with an empty pool, takes it out. The cluster follows within a minute: node labels and the Kueue queues of the classes. Running pods stay where they are.
//	@Tags			gpu
//	@Accept			json
//	@Produce		json
//	@Security		BearerAuth
//	@Param			name	path		string			true	"node name"
//	@Param			body	body		GPUNodeRequest	true	"assignment"
//	@Success		200		{object}	GPUFleetResponse
//	@Failure		400		{object}	ErrorResponse
//	@Failure		403		{object}	ErrorResponse
//	@Router			/v1/gpu/fleet/nodes/{name} [put]
func (s *Server) putGPUNode(c *gin.Context) {
	var req GPUNodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abort(c, http.StatusBadRequest, "invalid", "Ungültige Anfrage.")
		return
	}
	a := gpu.NodeAssignment{Node: c.Param("name"), Pool: strings.TrimSpace(req.Pool), Tier: req.Tier, AvailableUntil: req.AvailableUntil, GapModel: strings.TrimSpace(req.GapModel), UpdatedBy: caller(c).Email}
	if !s.fleetDone(c, s.gpu.AssignNode(c.Request.Context(), a)) {
		return
	}
	s.reconcileSoon(c)
	if err := s.gpu.ReconcileGapFillers(c.Request.Context()); err != nil {
		s.log.Warnw("gpu gap fillers after change", "error", err)
	}
	s.getGPUFleet(c)
}

// GPUPoolRequest names a pool.
type GPUPoolRequest struct {
	Name string `json:"name" example:"Wirtschaftsinformatik"`
	Lend bool   `json:"lend"`
}

// putGPUPool godoc
//
//	@ID				putGPUPool
//	@Summary		Create or rename a GPU pool
//	@Description	For admins: a pool is who GPUs are for — "allgemein", or GPUs of one group, granted in the tree only to that group's budgets. The id is fixed once created (it becomes part of the tree resources).
//	@Tags			gpu
//	@Accept			json
//	@Produce		json
//	@Security		BearerAuth
//	@Param			id		path		string			true	"pool id (2–20 lower-case letters or digits)"
//	@Param			body	body		GPUPoolRequest	true	"pool"
//	@Success		200		{object}	GPUFleetResponse
//	@Failure		400		{object}	ErrorResponse
//	@Failure		403		{object}	ErrorResponse
//	@Router			/v1/gpu/fleet/pools/{id} [put]
func (s *Server) putGPUPool(c *gin.Context) {
	var req GPUPoolRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abort(c, http.StatusBadRequest, "invalid", "Ungültige Anfrage.")
		return
	}
	if !s.fleetDone(c, s.gpu.SavePool(c.Request.Context(), gpu.Pool{ID: c.Param("id"), Name: req.Name, Lend: req.Lend})) {
		return
	}
	s.getGPUFleet(c)
}

// deleteGPUPool godoc
//
//	@ID				deleteGPUPool
//	@Summary		Delete a GPU pool
//	@Description	For admins: only a pool without classes (never used by a node) can go.
//	@Tags			gpu
//	@Produce		json
//	@Security		BearerAuth
//	@Param			id	path		string	true	"pool id"
//	@Success		200	{object}	GPUFleetResponse
//	@Failure		400	{object}	ErrorResponse
//	@Failure		403	{object}	ErrorResponse
//	@Router			/v1/gpu/fleet/pools/{id} [delete]
func (s *Server) deleteGPUPool(c *gin.Context) {
	if !s.fleetDone(c, s.gpu.DeletePool(c.Request.Context(), c.Param("id"))) {
		return
	}
	s.getGPUFleet(c)
}

// fleetDone answers a refused or failed change; true when there was none.
func (s *Server) fleetDone(c *gin.Context, err error) bool {
	var refused *gpu.ErrFleet
	switch {
	case err == nil:
		return true
	case errors.As(err, &refused):
		abort(c, http.StatusBadRequest, "invalid", refused.Reason)
	default:
		s.log.Errorw("gpu fleet change failed", "error", err)
		abort(c, http.StatusInternalServerError, "internal", "Änderung fehlgeschlagen.")
	}
	return false
}

// reconcileSoon brings the cluster to a change right away instead of at the next minute.
func (s *Server) reconcileSoon(c *gin.Context) {
	if err := s.gpu.ReconcileFleet(c.Request.Context()); err != nil {
		s.log.Warnw("gpu fleet reconcile after change", "error", err)
	}
}
