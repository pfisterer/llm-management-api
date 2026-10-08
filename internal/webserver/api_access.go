package webserver

import (
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/llm-management-api/internal/access"
	"github.com/pfisterer/llm-management-api/internal/roleprovider"
)

// MeResponse describes the caller. Role is empty without access: the UI then
// hides the LLM section entirely.
type MeResponse struct {
	Email        string      `json:"email" example:"a@dhbw.de"`
	Role         access.Role `json:"role" example:"user" enums:"user,fleet-admin,admin"`
	Tier         string      `json:"tier,omitempty" example:"staff"`
	MatchedToken string      `json:"matched_token,omitempty" example:"group:mitarbeitende"`
	ChatURL      string      `json:"chat_url,omitempty" example:"https://chat.llm.services.dhbw.cloud"`
}

// getMe godoc
//
//	@ID			getMe
//	@Summary		Who am I, and what may I do?
//	@Description	Always 200 for an authenticated caller. Without access, role is empty.
//	@Tags			access
//	@Produce		json
//	@Security		BearerAuth
//	@Success		200	{object}	MeResponse
//	@Failure		401	{object}	ErrorResponse
//	@Router			/v1/me [get]
func (s *Server) getMe(c *gin.Context) {
	cl := caller(c)
	resp := MeResponse{Email: cl.Email, Role: cl.Decision.Role}
	if cl.Decision.Role != access.RoleNone {
		resp.Tier = cl.Decision.Tier
		resp.MatchedToken = cl.Decision.MatchedToken
		resp.ChatURL = s.chatURL
	}
	c.JSON(http.StatusOK, resp)
}

// RuleRequest creates or replaces an access rule.
type RuleRequest struct {
	Token   string      `json:"token" binding:"required" example:"group:wwi23seb"`
	Role    access.Role `json:"role" binding:"required" example:"user" enums:"user,fleet-admin,admin"`
	Tier    string      `json:"tier" binding:"required" example:"student"`
	Comment string      `json:"comment" example:"Kurs WWI23SEB"`
}

// listRules godoc
//
//	@ID			listAccessRules
//	@Summary		List the access rules
//	@Description	Rules from the configuration come first and are marked bootstrap (read-only).
//	@Tags			access
//	@Produce		json
//	@Security		BearerAuth
//	@Success		200	{array}		access.Rule
//	@Failure		403	{object}	ErrorResponse
//	@Router			/v1/access-rules [get]
func (s *Server) listRules(c *gin.Context) {
	rules, err := s.access.Rules(c.Request.Context())
	if err != nil {
		s.internalError(c, "list rules", err)
		return
	}
	c.JSON(http.StatusOK, rules)
}

// createRule godoc
//
//	@ID			createAccessRule
//	@Summary	Add an access rule
//	@Tags		access
//	@Accept		json
//	@Produce	json
//	@Security	BearerAuth
//	@Param		rule	body		RuleRequest	true	"Rule"
//	@Success	201		{object}	access.Rule
//	@Failure	400		{object}	ErrorResponse
//	@Failure	409		{object}	ErrorResponse
//	@Router		/v1/access-rules [post]
func (s *Server) createRule(c *gin.Context) {
	var req RuleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abort(c, http.StatusBadRequest, "invalid", "Token, Rolle und Kontingentklasse sind Pflicht.")
		return
	}
	rule, err := s.access.Create(c.Request.Context(),
		access.Rule{Token: req.Token, Role: req.Role, Tier: req.Tier, Comment: strings.TrimSpace(req.Comment)}, caller(c).Email)
	if s.ruleError(c, err) {
		return
	}
	c.JSON(http.StatusCreated, rule)
}

// updateRule godoc
//
//	@ID			updateAccessRule
//	@Summary	Replace an access rule
//	@Tags		access
//	@Accept		json
//	@Produce	json
//	@Security	BearerAuth
//	@Param		id		path		int			true	"Rule id"
//	@Param		rule	body		RuleRequest	true	"Rule"
//	@Success	200		{object}	access.Rule
//	@Failure	400		{object}	ErrorResponse
//	@Failure	404		{object}	ErrorResponse
//	@Router		/v1/access-rules/{id} [put]
func (s *Server) updateRule(c *gin.Context) {
	id, ok := ruleID(c)
	if !ok {
		return
	}
	var req RuleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abort(c, http.StatusBadRequest, "invalid", "Token, Rolle und Kontingentklasse sind Pflicht.")
		return
	}
	rule, err := s.access.Update(c.Request.Context(),
		access.Rule{ID: id, Token: req.Token, Role: req.Role, Tier: req.Tier, Comment: strings.TrimSpace(req.Comment)}, caller(c).Email)
	if s.ruleError(c, err) {
		return
	}
	c.JSON(http.StatusOK, rule)
}

// deleteRule godoc
//
//	@ID			deleteAccessRule
//	@Summary	Delete an access rule
//	@Tags		access
//	@Security	BearerAuth
//	@Param		id	path	int	true	"Rule id"
//	@Success	204
//	@Failure	404	{object}	ErrorResponse
//	@Router		/v1/access-rules/{id} [delete]
func (s *Server) deleteRule(c *gin.Context) {
	id, ok := ruleID(c)
	if !ok {
		return
	}
	if s.ruleError(c, s.access.Delete(c.Request.Context(), id)) {
		return
	}
	c.Status(http.StatusNoContent)
}

// listTiers godoc
//
//	@ID			listTiers
//	@Summary	Quota tiers a rule may use
//	@Tags		access
//	@Produce	json
//	@Security	BearerAuth
//	@Success	200	{array}	string
//	@Router		/v1/tiers [get]
func (s *Server) listTiers(c *gin.Context) {
	tiers := s.access.Tiers()
	sort.Strings(tiers)
	c.JSON(http.StatusOK, tiers)
}

// searchGroups godoc
//
//	@ID			searchGroups
//	@Summary		Search role-provider groups
//	@Description	For the access-rule editor: find group tokens by id or name.
//	@Tags			access
//	@Produce		json
//	@Security		BearerAuth
//	@Param			q	query	string	true	"Search text (min. 2 characters)"
//	@Success		200	{array}		roleprovider.Group
//	@Failure		502	{object}	ErrorResponse
//	@Router			/v1/groups [get]
func (s *Server) searchGroups(c *gin.Context) {
	q := strings.TrimSpace(c.Query("q"))
	if len([]rune(q)) < 2 {
		c.JSON(http.StatusOK, []roleprovider.Group{})
		return
	}
	groups, err := s.roles.SearchGroups(c.Request.Context(), q, 25)
	if err != nil {
		s.log.Warnw("group search failed", "error", err)
		abort(c, http.StatusBadGateway, "role_provider", "Gruppensuche gerade nicht verfügbar.")
		return
	}
	c.JSON(http.StatusOK, groups)
}

func ruleID(c *gin.Context) (uint, bool) {
	n, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || n == 0 {
		abort(c, http.StatusBadRequest, "invalid", "Ungültige Regel-Id.")
		return 0, false
	}
	return uint(n), true
}

// ruleError maps service errors to answers; true when it answered.
func (s *Server) ruleError(c *gin.Context, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, access.ErrNotFound):
		abort(c, http.StatusNotFound, "not_found", "Regel nicht gefunden.")
	case errors.Is(err, access.ErrDuplicate):
		abort(c, http.StatusConflict, "duplicate", "Für dieses Token gibt es schon eine Regel.")
	case errors.Is(err, access.ErrBootstrapRule):
		abort(c, http.StatusConflict, "bootstrap", "Admins aus der Konfiguration lassen sich hier nicht ändern.")
	case errors.Is(err, access.ErrInvalidToken), errors.Is(err, access.ErrUnknownTier):
		abort(c, http.StatusBadRequest, "invalid", err.Error())
	case strings.Contains(err.Error(), "unknown role"):
		abort(c, http.StatusBadRequest, "invalid", err.Error())
	default:
		s.internalError(c, "access rule", err)
	}
	return true
}

func (s *Server) internalError(c *gin.Context, what string, err error) {
	s.log.Errorw(what+" failed", "error", err)
	abort(c, http.StatusInternalServerError, "internal", "Interner Fehler.")
}
