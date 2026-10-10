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
	APIURL       string      `json:"api_url,omitempty" example:"https://api.llm.services.dhbw.cloud/v1"`
	// Only for admins: autologin link to the LiteLLM admin UI.
	AdminUIURL string `json:"admin_ui_url,omitempty" example:"https://admin.llm.services.dhbw.cloud/autologin"`
	// GPU part: tier and JupyterHub, only with GPU access (and the GPU part enabled).
	GPUTier    string `json:"gpu_tier,omitempty" example:"student-gpu"`
	JupyterURL string `json:"jupyter_url,omitempty" example:"https://jupyter.gpu.services.dhbw.cloud"`
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
		resp.APIURL = s.apiURL
		if cl.Decision.Role.AtLeast(access.RoleAdmin) {
			resp.AdminUIURL = s.adminUI
		}
		if s.gpu != nil && cl.Decision.GPUTier != "" {
			resp.GPUTier = cl.Decision.GPUTier
			resp.JupyterURL = s.gpu.JupyterURL()
		}
	}
	c.JSON(http.StatusOK, resp)
}

// RuleRequest creates or replaces an access rule.
type RuleRequest struct {
	Token string      `json:"token" binding:"required" example:"group:wwi23seb"`
	Role  access.Role `json:"role" binding:"required" example:"user" enums:"user,fleet-admin,admin"`
	Tier  string      `json:"tier" binding:"required" example:"student"`
	// GPU tier (GPU notebooks and environments); empty = no GPU access.
	GPUTier string `json:"gpu_tier,omitempty" example:"student-gpu"`
	Comment string `json:"comment" example:"Kurs WWI23SEB"`
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
		access.Rule{Token: req.Token, Role: req.Role, Tier: req.Tier, GPUTier: req.GPUTier, Comment: strings.TrimSpace(req.Comment)}, caller(c).Email)
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
		access.Rule{ID: id, Token: req.Token, Role: req.Role, Tier: req.Tier, GPUTier: req.GPUTier, Comment: strings.TrimSpace(req.Comment)}, caller(c).Email)
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

// listGPUTiers godoc
//
//	@ID			listGPUTiers
//	@Summary	GPU tiers a rule may use
//	@Description	Empty when the GPU part is not enabled.
//	@Tags		access
//	@Produce	json
//	@Security	BearerAuth
//	@Success	200	{array}	string
//	@Router		/v1/gpu-tiers [get]
func (s *Server) listGPUTiers(c *gin.Context) {
	tiers := s.access.GPUTiers()
	sort.Strings(tiers)
	c.JSON(http.StatusOK, tiers)
}

// PrincipalSearchResponse is everything a rule's token may name: groups with
// their labels, and bare email addresses for individual people. Same shape as
// openstack-management-api's, so the UI uses one autocomplete for both.
type PrincipalSearchResponse struct {
	Groups []roleprovider.Group `json:"groups"`
	Users  []string             `json:"users"`
}

// searchPrincipals godoc
//
//	@ID			searchPrincipals
//	@Summary		Search groups and users
//	@Description	For the access-rule editor. Groups match on ID, display name or description; users on their EMAIL ADDRESS ONLY and only once q is non-empty — the directory is not browsable by person or name.
//	@Tags			access
//	@Produce		json
//	@Security		BearerAuth
//	@Param			q		query		string	false	"Search text"
//	@Param			limit	query		int		false	"Maximum entries per kind (default 10, at most 50)"
//	@Success		200		{object}	PrincipalSearchResponse
//	@Failure		502		{object}	ErrorResponse
//	@Router			/v1/principals/search [get]
func (s *Server) searchPrincipals(c *gin.Context) {
	q := strings.TrimSpace(c.Query("q"))
	limit, err := strconv.Atoi(c.DefaultQuery("limit", "10"))
	if err != nil || limit < 1 {
		limit = 10
	}
	limit = min(limit, 50)
	ctx := c.Request.Context()

	resp := PrincipalSearchResponse{Groups: []roleprovider.Group{}, Users: []string{}}
	groups, gErr := s.roles.SearchGroups(ctx, q, limit)
	if gErr != nil {
		s.log.Warnw("group search failed", "error", gErr)
	} else if groups != nil {
		resp.Groups = groups
	}
	var uErr error
	if q != "" {
		var users []string
		if users, uErr = s.roles.SearchUsers(ctx, q, limit); uErr != nil {
			s.log.Warnw("user search failed", "error", uErr)
		} else if users != nil {
			resp.Users = users
		}
	}
	// Only when nothing could be asked: an empty answer would read as "no
	// match" while the directory is in fact down.
	if gErr != nil && (q == "" || uErr != nil) {
		abort(c, http.StatusBadGateway, "role_provider", "Verzeichnis gerade nicht erreichbar.")
		return
	}
	c.JSON(http.StatusOK, resp)
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
	case errors.Is(err, access.ErrInvalidToken), errors.Is(err, access.ErrUnknownTier), errors.Is(err, access.ErrUnknownGPUTier):
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
