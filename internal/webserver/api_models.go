package webserver

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/llm-management-api/internal/gpu"
)

// getModelCatalog godoc
//
//	@ID				getModelCatalog
//	@Summary		Catalogue of the platform's own models
//	@Description	For admins: the models the platform runs itself on its GPUs (the inference gap fillers), with their vLLM arguments, resources and needed GPU memory, the default model of each GPU tier, and how their replicas are doing (ready, starting, waiting for a GPU, restarts and the last problem).
//	@Tags			llm
//	@Produce		json
//	@Security		BearerAuth
//	@Success		200	{object}	gpu.ModelCatalog
//	@Failure		403	{object}	ErrorResponse
//	@Router			/v1/inference/models [get]
func (s *Server) getModelCatalog(c *gin.Context) {
	cat, err := s.gpu.ModelCatalog(c.Request.Context())
	if err != nil {
		s.internalError(c, "model catalogue", err)
		return
	}
	c.JSON(http.StatusOK, cat)
}

// putCatalogModel godoc
//
//	@ID				putCatalogModel
//	@Summary		Add or change a model
//	@Description	For admins: a model of the catalogue. The gap fillers that run it are recreated with it within a minute.
//	@Tags			llm
//	@Accept			json
//	@Produce		json
//	@Security		BearerAuth
//	@Param			key		path		string				true	"model key (lower case, digits, dot, dash)"
//	@Param			body	body		gpu.CatalogModel	true	"model"
//	@Success		200		{object}	gpu.ModelCatalog
//	@Failure		400		{object}	ErrorResponse
//	@Failure		403		{object}	ErrorResponse
//	@Router			/v1/inference/models/{key} [put]
func (s *Server) putCatalogModel(c *gin.Context) {
	var m gpu.CatalogModel
	if err := c.ShouldBindJSON(&m); err != nil {
		abort(c, http.StatusBadRequest, "invalid", "Ungültige Anfrage.")
		return
	}
	m.Key, m.UpdatedBy = c.Param("key"), caller(c).Email
	if !s.fleetDone(c, s.gpu.SaveModel(c.Request.Context(), m)) {
		return
	}
	s.modelsChanged(c)
}

// deleteCatalogModel godoc
//
//	@ID				deleteCatalogModel
//	@Summary		Delete a model
//	@Description	For admins: only a model no tier has as default and no node has chosen.
//	@Tags			llm
//	@Produce		json
//	@Security		BearerAuth
//	@Param			key	path		string	true	"model key"
//	@Success		200	{object}	gpu.ModelCatalog
//	@Failure		400	{object}	ErrorResponse
//	@Failure		403	{object}	ErrorResponse
//	@Router			/v1/inference/models/{key} [delete]
func (s *Server) deleteCatalogModel(c *gin.Context) {
	if !s.fleetDone(c, s.gpu.DeleteModel(c.Request.Context(), c.Param("key"))) {
		return
	}
	s.modelsChanged(c)
}

// TierModelRequest is the default model of a GPU tier; empty = none.
type TierModelRequest struct {
	Model string `json:"model" example:"qwen3-8b-awq"`
}

// putTierModel godoc
//
//	@ID				putTierModel
//	@Summary		Default model of a GPU tier
//	@Description	For admins: the gap filler model of every node of the tier that has not chosen another; empty = none.
//	@Tags			llm
//	@Accept			json
//	@Produce		json
//	@Security		BearerAuth
//	@Param			tier	path		string				true	"tier (klein, mittel, gross)"
//	@Param			body	body		TierModelRequest	true	"model"
//	@Success		200		{object}	gpu.ModelCatalog
//	@Failure		400		{object}	ErrorResponse
//	@Failure		403		{object}	ErrorResponse
//	@Router			/v1/inference/tiers/{tier} [put]
func (s *Server) putTierModel(c *gin.Context) {
	var req TierModelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abort(c, http.StatusBadRequest, "invalid", "Ungültige Anfrage.")
		return
	}
	if !s.fleetDone(c, s.gpu.SetTierModel(c.Request.Context(), c.Param("tier"), req.Model)) {
		return
	}
	s.modelsChanged(c)
}

// modelsChanged brings the gap fillers to the catalogue at once and answers with it.
func (s *Server) modelsChanged(c *gin.Context) {
	if err := s.gpu.ReconcileGapFillers(c.Request.Context()); err != nil {
		s.log.Warnw("gap fillers after a catalogue change", "error", err)
	}
	s.getModelCatalog(c)
}
