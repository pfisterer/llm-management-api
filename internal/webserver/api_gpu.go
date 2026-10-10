package webserver

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/llm-management-api/internal/access"
	"github.com/pfisterer/llm-management-api/internal/gpu"
)

// requireGPU aborts unless the caller has a GPU tier (from any matching access rule).
func requireGPU(c *gin.Context) {
	if caller(c).Decision.GPUTier == "" {
		abort(c, http.StatusForbidden, "forbidden", "Kein Zugang zu GPU-Umgebungen.")
		return
	}
	c.Next()
}

// EnvironmentRequest adds an environment from a public Git repository.
type EnvironmentRequest struct {
	GitURL string `json:"git_url" binding:"required" example:"https://github.com/binder-examples/requirements"`
	// Branch, tag or commit; empty = default branch.
	Ref  string `json:"ref" example:"main"`
	Name string `json:"name" example:"Statistik-Kurs"`
}

// StartRequest starts a JupyterHub server for an environment.
type StartRequest struct {
	// With a GPU (waits in the queue if all GPUs are busy; counts against the GPU quota).
	GPU bool `json:"gpu" example:"false"`
}

// StartResponse is where the started server will be reachable.
type StartResponse struct {
	Server string `json:"server" example:"requirements-3"`
	URL    string `json:"url" example:"https://jupyter.gpu.services.dhbw.cloud/user/a@dhbw.de/requirements-3/"`
}

// BuildLogResponse is the tail of an environment's build log.
type BuildLogResponse struct {
	Log string `json:"log"`
}

func (s *Server) gpuError(c *gin.Context, what string, err error) {
	var he *gpu.HubError
	switch {
	case errors.Is(err, gpu.ErrNotFound):
		abort(c, http.StatusNotFound, "not_found", "Nicht gefunden.")
	case errors.Is(err, gpu.ErrForbidden):
		abort(c, http.StatusForbidden, "forbidden", "Diese Umgebung gehört jemand anderem.")
	case errors.Is(err, gpu.ErrInvalidRepo), errors.Is(err, gpu.ErrRefNotFound):
		abort(c, http.StatusBadRequest, "invalid", err.Error())
	case errors.Is(err, gpu.ErrNotReady):
		abort(c, http.StatusConflict, "not_ready", "Die Umgebung ist noch nicht gebaut.")
	case errors.As(err, &he) && he.Status < 500:
		// JupyterHub refused, e.g. GPU quota exhausted: its message is meant for people.
		abort(c, http.StatusConflict, "hub_refused", he.Message)
	default:
		s.log.Errorw(what+" failed", "error", err)
		abort(c, http.StatusBadGateway, "gpu_cluster", "GPU-Cluster gerade nicht erreichbar.")
	}
}

func (s *Server) environment(c *gin.Context) (gpu.Environment, bool) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		abort(c, http.StatusBadRequest, "invalid", "Ungültige Umgebungs-ID.")
		return gpu.Environment{}, false
	}
	cl := caller(c)
	e, err := s.gpu.Get(c.Request.Context(), uint(id), cl.Email, cl.Decision.Role.AtLeast(access.RoleAdmin))
	if err != nil {
		s.gpuError(c, "get environment", err)
		return gpu.Environment{}, false
	}
	return e, true
}

// listEnvironments godoc
//
//	@ID			listGPUEnvironments
//	@Summary	My environments
//	@Description	Environments built from Git repositories, newest first, with the current build status.
//	@Tags		gpu
//	@Produce	json
//	@Security	BearerAuth
//	@Success	200	{array}		gpu.Environment
//	@Failure	403	{object}	ErrorResponse
//	@Router		/v1/gpu/environments [get]
func (s *Server) listEnvironments(c *gin.Context) {
	envs, err := s.gpu.List(c.Request.Context(), caller(c).Email)
	if err != nil {
		s.gpuError(c, "list environments", err)
		return
	}
	if envs == nil {
		envs = []gpu.Environment{}
	}
	c.JSON(http.StatusOK, envs)
}

// createEnvironment godoc
//
//	@ID			createGPUEnvironment
//	@Summary	Add an environment from a Git repository
//	@Description	Resolves the ref to a commit. If Harbor already has the image for that commit, the environment is ready at once; otherwise a build starts (status building). Public repositories on the allowed hosts only.
//	@Tags		gpu
//	@Accept		json
//	@Produce	json
//	@Security	BearerAuth
//	@Param		environment	body		EnvironmentRequest	true	"Repository"
//	@Success	201			{object}	gpu.Environment
//	@Failure	400			{object}	ErrorResponse
//	@Failure	502			{object}	ErrorResponse
//	@Router		/v1/gpu/environments [post]
func (s *Server) createEnvironment(c *gin.Context) {
	var req EnvironmentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abort(c, http.StatusBadRequest, "invalid", "Die Git-URL ist Pflicht.")
		return
	}
	e, err := s.gpu.Create(c.Request.Context(), caller(c).Email, req.GitURL, req.Ref, req.Name)
	if err != nil {
		s.gpuError(c, "create environment", err)
		return
	}
	c.JSON(http.StatusCreated, e)
}

// getEnvironment godoc
//
//	@ID			getGPUEnvironment
//	@Summary	One environment with its build status
//	@Tags		gpu
//	@Produce	json
//	@Security	BearerAuth
//	@Param		id	path		int	true	"Environment id"
//	@Success	200	{object}	gpu.Environment
//	@Failure	404	{object}	ErrorResponse
//	@Router		/v1/gpu/environments/{id} [get]
func (s *Server) getEnvironment(c *gin.Context) {
	if e, ok := s.environment(c); ok {
		c.JSON(http.StatusOK, e)
	}
}

// getEnvironmentLog godoc
//
//	@ID			getGPUEnvironmentLog
//	@Summary	Tail of the build log
//	@Description	Available while the build job exists (one day after it finished).
//	@Tags		gpu
//	@Produce	json
//	@Security	BearerAuth
//	@Param		id	path		int	true	"Environment id"
//	@Success	200	{object}	BuildLogResponse
//	@Failure	404	{object}	ErrorResponse
//	@Router		/v1/gpu/environments/{id}/log [get]
func (s *Server) getEnvironmentLog(c *gin.Context) {
	e, ok := s.environment(c)
	if !ok {
		return
	}
	log, err := s.gpu.Log(c.Request.Context(), e)
	if err != nil {
		s.gpuError(c, "build log", err)
		return
	}
	c.JSON(http.StatusOK, BuildLogResponse{Log: log})
}

// rebuildEnvironment godoc
//
//	@ID			rebuildGPUEnvironment
//	@Summary	Build the environment again
//	@Tags		gpu
//	@Produce	json
//	@Security	BearerAuth
//	@Param		id	path		int	true	"Environment id"
//	@Success	200	{object}	gpu.Environment
//	@Failure	404	{object}	ErrorResponse
//	@Router		/v1/gpu/environments/{id}/rebuild [post]
func (s *Server) rebuildEnvironment(c *gin.Context) {
	e, ok := s.environment(c)
	if !ok {
		return
	}
	e, err := s.gpu.Rebuild(c.Request.Context(), e)
	if err != nil {
		s.gpuError(c, "rebuild environment", err)
		return
	}
	c.JSON(http.StatusOK, e)
}

// deleteEnvironment godoc
//
//	@ID			deleteGPUEnvironment
//	@Summary	Forget an environment
//	@Description	The image stays in Harbor until its retention rules remove it.
//	@Tags		gpu
//	@Security	BearerAuth
//	@Param		id	path	int	true	"Environment id"
//	@Success	204
//	@Failure	404	{object}	ErrorResponse
//	@Router		/v1/gpu/environments/{id} [delete]
func (s *Server) deleteEnvironment(c *gin.Context) {
	e, ok := s.environment(c)
	if !ok {
		return
	}
	if err := s.gpu.Delete(c.Request.Context(), e); err != nil {
		s.gpuError(c, "delete environment", err)
		return
	}
	c.Status(http.StatusNoContent)
}

// startEnvironment godoc
//
//	@ID			startGPUEnvironment
//	@Summary	Start a JupyterHub server for the environment
//	@Description	A named server per environment; the repository is checked out into ~/projekte/<repo>. With gpu=true it waits for a free GPU and counts against the GPU quota; JupyterHub's refusal comes back as 409 with its message.
//	@Tags		gpu
//	@Accept		json
//	@Produce	json
//	@Security	BearerAuth
//	@Param		id		path		int				true	"Environment id"
//	@Param		start	body		StartRequest	false	"Options"
//	@Success	202		{object}	StartResponse
//	@Failure	409		{object}	ErrorResponse
//	@Router		/v1/gpu/environments/{id}/start [post]
func (s *Server) startEnvironment(c *gin.Context) {
	e, ok := s.environment(c)
	if !ok {
		return
	}
	var req StartRequest
	_ = c.ShouldBindJSON(&req)
	u, err := s.gpu.Start(c.Request.Context(), e, caller(c).Email, req.GPU)
	if err != nil {
		s.gpuError(c, "start environment", err)
		return
	}
	c.JSON(http.StatusAccepted, StartResponse{Server: gpu.ServerName(e), URL: u})
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
//	@Description	Named servers are removed after stopping; the home stays.
//	@Tags		gpu
//	@Security	BearerAuth
//	@Param		name	path	string	true	"Server name"
//	@Success	204
//	@Failure	404	{object}	ErrorResponse
//	@Router		/v1/gpu/servers/{name} [delete]
func (s *Server) stopServer(c *gin.Context) {
	name := strings.TrimSpace(c.Param("name"))
	if err := s.gpu.StopServer(c.Request.Context(), caller(c).Email, name); err != nil {
		s.gpuError(c, "stop server", err)
		return
	}
	c.Status(http.StatusNoContent)
}
