package webserver

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/llm-management-api/internal/keys"
)

// CreateKeyRequest names the new key and the project whose LLM tokens it spends.
type CreateKeyRequest struct {
	Name string `json:"name" binding:"required" example:"vscode"`
	// Project is the slug of one of the caller's projects with LLM tokens; empty = the caller's default project.
	Project string `json:"project" example:"t-7841d99e-68a2-47b7-88a5-1ab55180f4f5"`
}

// CreateKeyResponse carries the secret — the only time it is ever shown.
type CreateKeyResponse struct {
	Key         string `json:"key" example:"sk-…"`
	Name        string `json:"name" example:"vscode"`
	Project     string `json:"project" example:"t-7841d99e-68a2-47b7-88a5-1ab55180f4f5"`
	ProjectName string `json:"project_name" example:"Machine Learning WWI23SEB"`
}

// DefaultProjectRequest names the caller's default project.
type DefaultProjectRequest struct {
	Project string `json:"project" binding:"required" example:"t-7841d99e-68a2-47b7-88a5-1ab55180f4f5"`
}

// DefaultProjectResponse is the caller's default project after the change.
type DefaultProjectResponse struct {
	Project     string `json:"project" example:"t-7841d99e-68a2-47b7-88a5-1ab55180f4f5"`
	ProjectName string `json:"project_name" example:"Machine Learning WWI23SEB"`
}

// person and grant (pools) of the caller; false (and an answer) when they may not use the LLM.
func (s *Server) personAndGrant(c *gin.Context) (keys.Person, keys.Grant, bool) {
	cl := caller(c)
	g, status, msg := s.quota.GrantFor(c.Request.Context(), cl.Tokens)
	if status != 0 {
		code := map[int]string{http.StatusForbidden: "no_quota", http.StatusServiceUnavailable: "unavailable"}[status]
		abort(c, status, code, msg)
		return keys.Person{}, keys.Grant{}, false
	}
	return keys.Person{Subject: cl.Subject, Email: cl.Email}, g, true
}

// notEligible answers a project the caller may not draw LLM tokens from.
func notEligible(c *gin.Context) {
	abort(c, http.StatusForbidden, "project_not_allowed", "Für dieses Projekt stehen keine LLM-Tokens zur Verfügung: Es gehört nicht zu den eigenen Projekten, ist abgelaufen oder hat keine LLM-Tokens.")
}

// getUsage godoc
//
//	@ID			getUsage
//	@Summary		Quota, limits and keys of the caller
//	@Description	The projects whose LLM tokens the caller may use — each one pool shared by all its members, with its spend, budget and window from LiteLLM, which one is the caller's default, its requests and tokens per minute and models for all its members together, and the short-term budget of each key in it —, and the caller's keys.
//	@Tags			keys
//	@Produce		json
//	@Security		BearerAuth
//	@Success		200	{object}	keys.Usage
//	@Failure		403	{object}	ErrorResponse
//	@Router			/v1/usage [get]
func (s *Server) getUsage(c *gin.Context) {
	p, g, ok := s.personAndGrant(c)
	if !ok {
		return
	}
	u, err := s.keys.Usage(c.Request.Context(), p, g)
	if err != nil {
		s.upstreamError(c, "usage", err)
		return
	}
	c.JSON(http.StatusOK, u)
}

// listKeys godoc
//
//	@ID			listKeys
//	@Summary	The caller's API keys
//	@Tags		keys
//	@Produce	json
//	@Security	BearerAuth
//	@Success	200	{array}		keys.KeyView
//	@Failure	403	{object}	ErrorResponse
//	@Router		/v1/keys [get]
func (s *Server) listKeys(c *gin.Context) {
	p, g, ok := s.personAndGrant(c)
	if !ok {
		return
	}
	u, err := s.keys.Usage(c.Request.Context(), p, g)
	if err != nil {
		s.upstreamError(c, "list keys", err)
		return
	}
	c.JSON(http.StatusOK, u.Keys)
}

// createKey godoc
//
//	@ID			createKey
//	@Summary		Create an API key
//	@Description	The name is required and unique per person. The key spends the LLM tokens of the given project (one of the caller's, see getUsage), of the caller's default project when none is given. The secret is returned exactly once.
//	@Tags			keys
//	@Accept			json
//	@Produce		json
//	@Security		BearerAuth
//	@Param			key	body		CreateKeyRequest	true	"Key"
//	@Success		201	{object}	CreateKeyResponse
//	@Failure		400	{object}	ErrorResponse
//	@Failure		403	{object}	ErrorResponse
//	@Failure		409	{object}	ErrorResponse
//	@Router			/v1/keys [post]
func (s *Server) createKey(c *gin.Context) {
	p, g, ok := s.personAndGrant(c)
	if !ok {
		return
	}
	var req CreateKeyRequest
	_ = c.ShouldBindJSON(&req)
	secret, v, err := s.keys.Create(c.Request.Context(), p, g, req.Name, req.Project)
	switch {
	case errors.Is(err, keys.ErrNameRequired):
		abort(c, http.StatusBadRequest, "name_required", "Bitte einen Namen für den API Key angeben, z. B. „laptop“ oder „vscode“.")
	case errors.Is(err, keys.ErrNotEligible):
		notEligible(c)
	case errors.Is(err, keys.ErrTooManyKeys):
		abort(c, http.StatusConflict, "too_many_keys", "Höchstzahl an API Keys erreicht. Bitte zuerst einen alten löschen.")
	case errors.Is(err, keys.ErrDuplicate):
		abort(c, http.StatusConflict, "duplicate", "Diese Bezeichnung ist schon für einen anderen eigenen Key vergeben.")
	case err != nil:
		s.upstreamError(c, "create key", err)
	default:
		c.JSON(http.StatusCreated, CreateKeyResponse{Key: secret, Name: v.Name, Project: v.Project, ProjectName: v.ProjectName})
	}
}

// setDefaultProject godoc
//
//	@ID			setDefaultProject
//	@Summary		Set the caller's default project
//	@Description	The project whose LLM tokens the chat and new API keys use when no project is named; one of the caller's projects with LLM tokens. The chat moves to it right away.
//	@Tags			keys
//	@Accept			json
//	@Produce		json
//	@Security		BearerAuth
//	@Param			project	body		DefaultProjectRequest	true	"Project"
//	@Success		200		{object}	DefaultProjectResponse
//	@Failure		400		{object}	ErrorResponse
//	@Failure		403		{object}	ErrorResponse
//	@Router			/v1/keys/default-project [put]
func (s *Server) setDefaultProject(c *gin.Context) {
	p, g, ok := s.personAndGrant(c)
	if !ok {
		return
	}
	var req DefaultProjectRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abort(c, http.StatusBadRequest, "project_required", "Bitte ein Projekt angeben.")
		return
	}
	pool, err := s.keys.SetDefaultProject(c.Request.Context(), p, g, req.Project)
	switch {
	case errors.Is(err, keys.ErrNotEligible):
		notEligible(c)
	case err != nil:
		s.upstreamError(c, "set default project", err)
	default:
		c.JSON(http.StatusOK, DefaultProjectResponse{Project: pool.Slug, ProjectName: pool.Name})
	}
}

// deleteKey godoc
//
//	@ID			deleteKey
//	@Summary	Delete one of the caller's API keys
//	@Tags		keys
//	@Security	BearerAuth
//	@Param		id	path	string	true	"Key id (from the key list, not the secret)"
//	@Success	204
//	@Failure	404	{object}	ErrorResponse
//	@Router		/v1/keys/{id} [delete]
func (s *Server) deleteKey(c *gin.Context) {
	cl := caller(c)
	err := s.keys.Delete(c.Request.Context(), keys.Person{Subject: cl.Subject, Email: cl.Email}, c.Param("id"))
	switch {
	case errors.Is(err, keys.ErrNotFound):
		abort(c, http.StatusNotFound, "not_found", "API Key nicht gefunden.")
	case err != nil:
		s.upstreamError(c, "delete key", err)
	default:
		c.Status(http.StatusNoContent)
	}
}

func (s *Server) upstreamError(c *gin.Context, what string, err error) {
	s.log.Errorw(what+" failed", "error", err)
	abort(c, http.StatusBadGateway, "litellm", "Das Gateway ist gerade nicht erreichbar.")
}
