package webserver

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/llm-management-api/internal/keys"
)

// CreateKeyRequest names the new key.
type CreateKeyRequest struct {
	Name string `json:"name" binding:"required" example:"vscode"`
}

// CreateKeyResponse carries the secret — the only time it is ever shown.
type CreateKeyResponse struct {
	Key  string `json:"key" example:"sk-…"`
	Name string `json:"name" example:"vscode"`
}

// person and tier of the caller; false (and an answer) when the tier is not
// configured, which would be a configuration error.
func (s *Server) personAndTier(c *gin.Context) (keys.Person, keys.Tier, bool) {
	cl := caller(c)
	t, ok := s.tiers[cl.Decision.Tier]
	if !ok {
		s.log.Errorw("access rule names an unknown tier", "tier", cl.Decision.Tier, "email", cl.Email)
		abort(c, http.StatusInternalServerError, "internal", "Kontingentklasse nicht konfiguriert.")
		return keys.Person{}, keys.Tier{}, false
	}
	return keys.Person{Subject: cl.Subject, Email: cl.Email}, t, true
}

// getUsage godoc
//
//	@Summary		Quota, limits and keys of the caller
//	@Description	Monthly budget over all keys, the per-key short-term budgets, rate limits and allowed models.
//	@Tags			keys
//	@Produce		json
//	@Security		BearerAuth
//	@Success		200	{object}	keys.Usage
//	@Failure		403	{object}	ErrorResponse
//	@Router			/usage [get]
func (s *Server) getUsage(c *gin.Context) {
	p, t, ok := s.personAndTier(c)
	if !ok {
		return
	}
	u, err := s.keys.Usage(c.Request.Context(), p, t)
	if err != nil {
		s.upstreamError(c, "usage", err)
		return
	}
	c.JSON(http.StatusOK, u)
}

// listKeys godoc
//
//	@Summary	The caller's API keys
//	@Tags		keys
//	@Produce	json
//	@Security	BearerAuth
//	@Success	200	{array}		keys.KeyView
//	@Failure	403	{object}	ErrorResponse
//	@Router		/keys [get]
func (s *Server) listKeys(c *gin.Context) {
	p, t, ok := s.personAndTier(c)
	if !ok {
		return
	}
	u, err := s.keys.Usage(c.Request.Context(), p, t)
	if err != nil {
		s.upstreamError(c, "list keys", err)
		return
	}
	c.JSON(http.StatusOK, u.Keys)
}

// createKey godoc
//
//	@Summary		Create an API key
//	@Description	The name is required and unique per person. The secret is returned exactly once.
//	@Tags			keys
//	@Accept			json
//	@Produce		json
//	@Security		BearerAuth
//	@Param			key	body		CreateKeyRequest	true	"Key"
//	@Success		201	{object}	CreateKeyResponse
//	@Failure		400	{object}	ErrorResponse
//	@Failure		409	{object}	ErrorResponse
//	@Router			/keys [post]
func (s *Server) createKey(c *gin.Context) {
	p, t, ok := s.personAndTier(c)
	if !ok {
		return
	}
	var req CreateKeyRequest
	_ = c.ShouldBindJSON(&req)
	secret, v, err := s.keys.Create(c.Request.Context(), p, t, req.Name)
	switch {
	case errors.Is(err, keys.ErrNameRequired):
		abort(c, http.StatusBadRequest, "name_required", "Bitte einen Namen für den API Key angeben, z. B. „laptop“ oder „vscode“.")
	case errors.Is(err, keys.ErrTooManyKeys):
		abort(c, http.StatusConflict, "too_many_keys", "Höchstzahl an API Keys erreicht. Bitte zuerst einen alten löschen.")
	case errors.Is(err, keys.ErrDuplicate):
		abort(c, http.StatusConflict, "duplicate", "Diese Bezeichnung ist schon für einen anderen eigenen Key vergeben.")
	case err != nil:
		s.upstreamError(c, "create key", err)
	default:
		c.JSON(http.StatusCreated, CreateKeyResponse{Key: secret, Name: v.Name})
	}
}

// deleteKey godoc
//
//	@Summary	Delete one of the caller's API keys
//	@Tags		keys
//	@Security	BearerAuth
//	@Param		id	path	string	true	"Key id (from the key list, not the secret)"
//	@Success	204
//	@Failure	404	{object}	ErrorResponse
//	@Router		/keys/{id} [delete]
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
