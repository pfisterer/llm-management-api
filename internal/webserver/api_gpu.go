package webserver

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/llm-management-api/internal/access"
	"github.com/pfisterer/llm-management-api/internal/gpu"
)

// requireWorkspace aborts unless the caller may use workspaces: a member of an active project, or an admin (who manages the projects).
func (s *Server) requireWorkspace(c *gin.Context) {
	if !caller(c).Role.AtLeast(access.RoleAdmin) && len(s.myProjects(c)) == 0 {
		abort(c, http.StatusForbidden, "forbidden", "Kein Zugang zu Workspaces (kein Projekt).")
		return
	}
	c.Next()
}

// BuildLogResponse is the tail of a project image's build log.
type BuildLogResponse struct {
	Log string `json:"log"`
}

func (s *Server) gpuError(c *gin.Context, what string, err error) {
	var he *gpu.HubError
	switch {
	case errors.Is(err, gpu.ErrNotFound):
		abort(c, http.StatusNotFound, "not_found", "Nicht gefunden.")
	case errors.Is(err, gpu.ErrInvalidRepo), errors.Is(err, gpu.ErrRefNotFound):
		abort(c, http.StatusBadRequest, "invalid", err.Error())
	case errors.As(err, &he) && he.Status < 500:
		// JupyterHub refused, e.g. GPU quota exhausted: its message is meant for people.
		abort(c, http.StatusConflict, "hub_refused", he.Message)
	default:
		s.log.Errorw(what+" failed", "error", err)
		abort(c, http.StatusBadGateway, "gpu_cluster", "GPU-Cluster gerade nicht erreichbar.")
	}
}

// listServers godoc
//
//	@ID			listGPUServers
//	@Summary	My JupyterHub servers
//	@Tags		gpu
//	@Produce	json
//	@Security	BearerAuth
//	@Success	200	{array}		gpu.Server
//	@Failure	502	{object}	ErrorResponse
//	@Router		/v1/gpu/servers [get]
func (s *Server) listServers(c *gin.Context) {
	servers, err := s.gpu.Servers(c.Request.Context(), caller(c).Email)
	if err != nil {
		s.gpuError(c, "list servers", err)
		return
	}
	c.JSON(http.StatusOK, servers)
}

// stopServer godoc
//
//	@ID			stopGPUServer
//	@Summary	Stop one of my servers
//	@Description	Named servers are removed after stopping; the home stays. The default server has no name and is addressed as "-".
//	@Tags		gpu
//	@Security	BearerAuth
//	@Param		name	path	string	true	"Server name, \"-\" for the default server"
//	@Success	204
//	@Failure	404	{object}	ErrorResponse
//	@Router		/v1/gpu/servers/{name} [delete]
func (s *Server) stopServer(c *gin.Context) {
	name := strings.TrimSpace(c.Param("name"))
	// The default server's name is empty, which a path cannot carry: "-" stands for it.
	if name == "-" {
		name = ""
	}
	if err := s.gpu.StopServer(c.Request.Context(), caller(c).Email, name); err != nil {
		s.gpuError(c, "stop server", err)
		return
	}
	c.Status(http.StatusNoContent)
}
