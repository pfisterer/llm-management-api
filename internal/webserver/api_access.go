package webserver

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/llm-management-api/internal/access"
)

// MeResponse describes the caller. Role comes from the configuration (ADMIN_TOKENS, FLEET_ADMIN_TOKENS; everybody else is a user); the LLM section depends on llm_mtokens, the workspaces on workspace_projects.
type MeResponse struct {
	Email string      `json:"email" example:"a@dhbw.de"`
	Role  access.Role `json:"role" example:"user" enums:"user,fleet-admin,admin"`
	// LLM: tokens per 30 days in millions of the projects whose pools the caller may draw from, added up — each pool is shared with the project's other members, so this is no allowance of the caller's own; chat and API only when it is more than 0. The limits are per project (getUsage).
	LLMMTokens int    `json:"llm_mtokens" example:"100"`
	ChatURL    string `json:"chat_url,omitempty" example:"https://chat.llm.services.dhbw.cloud"`
	APIURL     string `json:"api_url,omitempty" example:"https://api.llm.services.dhbw.cloud/v1"`
	// Only for admins: autologin link to the LiteLLM admin UI.
	AdminUIURL string `json:"admin_ui_url,omitempty" example:"https://admin.llm.services.dhbw.cloud/autologin"`
	// JupyterHub, only with a project (and the GPU part enabled).
	JupyterURL string `json:"jupyter_url,omitempty" example:"https://jupyter.gpu.services.dhbw.cloud"`
	// Workspaces: number of active projects the caller belongs to.
	WorkspaceProjects int `json:"workspace_projects" example:"1"`
}

// getMe godoc
//
//	@ID			getMe
//	@Summary		Who am I, and what may I do?
//	@Description	Always 200 for an authenticated caller. Role is user unless the configuration makes the caller fleet-admin or admin; without LLM tokens from a project, llm_mtokens is 0 and the LLM links are missing.
//	@Tags			access
//	@Produce		json
//	@Security		BearerAuth
//	@Success		200	{object}	MeResponse
//	@Failure		401	{object}	ErrorResponse
//	@Router			/v1/me [get]
func (s *Server) getMe(c *gin.Context) {
	cl := caller(c)
	resp := MeResponse{Email: cl.Email, Role: cl.Role}
	if cl.Role.AtLeast(access.RoleAdmin) {
		resp.AdminUIURL = s.adminUI
	}
	if s.ws != nil {
		if pools, status, _ := s.quota.PoolsFor(c.Request.Context(), cl.Tokens); status == 0 {
			for _, p := range pools {
				resp.LLMMTokens += p.MTokens
			}
		}
	}
	if resp.LLMMTokens > 0 {
		resp.ChatURL = s.chatURL
		resp.APIURL = s.apiURL
	}
	if s.gpu != nil {
		resp.WorkspaceProjects = len(s.myProjects(c))
		if resp.WorkspaceProjects > 0 {
			resp.JupyterURL = s.gpu.JupyterURL()
		}
	}
	c.JSON(http.StatusOK, resp)
}

func (s *Server) internalError(c *gin.Context, what string, err error) {
	s.log.Errorw(what+" failed", "error", err)
	abort(c, http.StatusInternalServerError, "internal", "Interner Fehler.")
}
