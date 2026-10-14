package webserver

import (
	"crypto/subtle"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/cloud-self-service-golib/authn"
	"github.com/pfisterer/llm-management-api/internal/workspace"
)

// requireTreeToken lets only the budget tree (resource-management-api) through, by its own token.
func (s *Server) requireTreeToken(c *gin.Context) {
	raw, ok := authn.CutBearerPrefix(c.GetHeader("Authorization"))
	if !ok || s.treeTok == "" || subtle.ConstantTimeCompare([]byte(raw), []byte(s.treeTok)) != 1 {
		abort(c, http.StatusUnauthorized, "unauthenticated", "Nur für den Budgetbaum.")
		return
	}
	c.Next()
}

// listTreeProjects godoc
//
//	@ID				listTreeProjects
//	@Summary		Projects of the budget tree
//	@Description	Every project the budget tree manages, with what it used of its tree budgets in the current windows. For resource-management-api, with its own token.
//	@Tags			tree
//	@Produce		json
//	@Security		BearerAuth
//	@Success		200	{array}		workspace.TreeStatus
//	@Failure		401	{object}	ErrorResponse
//	@Router			/v1/tree/projects [get]
func (s *Server) listTreeProjects(c *gin.Context) {
	list, err := s.ws.TreeProjects(c.Request.Context())
	if err != nil {
		s.internalError(c, "tree projects", err)
		return
	}
	c.JSON(http.StatusOK, list)
}

// putTreeProject godoc
//
//	@ID				putTreeProject
//	@Summary		Create or update a project of the budget tree
//	@Description	Sets what the tree owns of the node's project — name, members, end, the project's budgets per window, GPUs per class, LLM tokens, rate limits and models — and creates the project when there is none. What its managers set (shares per person, shorter windows, profiles, images) stays.
//	@Tags			tree
//	@Accept			json
//	@Produce		json
//	@Security		BearerAuth
//	@Param			node	path		string					true	"the project's node in the budget tree"
//	@Param			project	body		workspace.TreeProject	true	"what the tree says"
//	@Success		200		{object}	workspace.TreeStatus
//	@Failure		400		{object}	ErrorResponse
//	@Failure		401		{object}	ErrorResponse
//	@Router			/v1/tree/projects/{node} [put]
func (s *Server) putTreeProject(c *gin.Context) {
	var tp workspace.TreeProject
	if err := c.ShouldBindJSON(&tp); err != nil {
		abort(c, http.StatusBadRequest, "invalid", "Ungültiger Inhalt.")
		return
	}
	st, err := s.ws.UpsertFromTree(c.Request.Context(), c.Param("node"), tp)
	if s.workspaceError(c, err) {
		return
	}
	s.treeChanged()
	c.JSON(http.StatusOK, st)
}

// deleteTreeProject godoc
//
//	@ID				deleteTreeProject
//	@Summary		Delete a project of the budget tree
//	@Description	Removes the node's project; its usage stays booked. Within ten minutes its servers are ended and its workspace storage (the members' homes in the project and the shared folder) is deleted. 404 when there is none.
//	@Tags			tree
//	@Security		BearerAuth
//	@Param			node	path	string	true	"the project's node in the budget tree"
//	@Success		204
//	@Failure		401	{object}	ErrorResponse
//	@Failure		404	{object}	ErrorResponse
//	@Router			/v1/tree/projects/{node} [delete]
func (s *Server) deleteTreeProject(c *gin.Context) {
	err := s.ws.DeleteFromTree(c.Request.Context(), c.Param("node"))
	if errors.Is(err, workspace.ErrNotFound) {
		abort(c, http.StatusNotFound, "not_found", "Projekt nicht gefunden.")
		return
	}
	if s.workspaceError(c, err) {
		return
	}
	s.treeChanged()
	c.Status(http.StatusNoContent)
}

// treeChanged tells whoever listens (the budget sync) that members or LLM tokens, limits or models may have changed.
func (s *Server) treeChanged() {
	if s.onTree != nil {
		s.onTree()
	}
}

// TreeCatalog is what the tree can grant here: the GPU classes and the profiles that use them, and for the LLM the base models every project has (llm_models adds to them) and the requests and tokens per minute of a project for which the tree sets none (0 = no limit).
type TreeCatalog struct {
	workspace.Config
	LLMBaseModels []string `json:"llm_base_models" example:"chat-default,chat-fast,code,embedding"`
	LLMDefaultRPM int      `json:"llm_default_rpm" example:"60"`
	LLMDefaultTPM int      `json:"llm_default_tpm" example:"200000"`
}

// getTreeCatalog godoc
//
//	@ID				getTreeCatalog
//	@Summary		GPU classes, profiles and LLM defaults, for the budget tree
//	@Description	What the tree can grant here: the GPU classes, the profiles that use them, the LLM base models and the default requests and tokens per minute of a project.
//	@Tags			tree
//	@Produce		json
//	@Security		BearerAuth
//	@Success		200	{object}	TreeCatalog
//	@Failure		401	{object}	ErrorResponse
//	@Router			/v1/tree/catalog [get]
func (s *Server) getTreeCatalog(c *gin.Context) {
	base := s.quota.BaseModels
	if base == nil {
		base = []string{}
	}
	c.JSON(http.StatusOK, TreeCatalog{Config: s.ws.Catalog(c.Request.Context()), LLMBaseModels: base, LLMDefaultRPM: s.quota.DefaultRPM, LLMDefaultTPM: s.quota.DefaultTPM})
}
