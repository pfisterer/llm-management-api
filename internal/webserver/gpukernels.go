package webserver

import (
	"errors"
	"net"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/llm-management-api/internal/gpu"
)

// kernelRoutes: GPU kernel pods for the notebook servers of "CPU + GPU-Kernel" (docs/bauplan.md 6.2). The caller is the server pod at the request's source address with its own hub token (JUPYTERHUB_API_TOKEN, header "Authorization: token …"); it can start, poll and stop only its own kernels, and the pod is built from the server pod, not from the request.
func kernelRoutes(r *gin.Engine, o GPUHubOptions) {
	caller := func(c *gin.Context) *gpu.KernelOwner {
		ip, _, err := net.SplitHostPort(c.Request.RemoteAddr)
		token, ok := strings.CutPrefix(c.GetHeader("Authorization"), "token ")
		if err != nil || !ok {
			abort(c, http.StatusUnauthorized, "unauthorized", "Nur für Workspace-Server.")
			return nil
		}
		owner, err := o.Kernels.KernelCaller(c.Request.Context(), ip, strings.TrimSpace(token))
		switch {
		case errors.Is(err, gpu.ErrNoKernelOwner):
			abort(c, http.StatusForbidden, "forbidden", "GPU-Kernel gibt es nur in Servern der Art „CPU + GPU-Kernel“.")
			return nil
		case err != nil:
			o.Log.Errorw("gpu kernel caller", "ip", ip, "error", err)
			abort(c, http.StatusBadGateway, "unavailable", "Der Server konnte nicht geprüft werden.")
			return nil
		}
		return owner
	}
	failed := func(c *gin.Context, owner *gpu.KernelOwner, what string, err error) {
		var refused *gpu.KernelRefused
		if errors.As(err, &refused) {
			abort(c, http.StatusConflict, "refused", refused.Reason)
			return
		}
		o.Log.Errorw("gpu kernel "+what, "owner", owner.Pod, "error", err)
		abort(c, http.StatusInternalServerError, "internal", "GPU-Kernel: Fehler im KI-Dienst.")
	}
	r.POST("/gpu/kernels", func(c *gin.Context) {
		owner := caller(c)
		if owner == nil {
			return
		}
		var req gpu.KernelRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abort(c, http.StatusBadRequest, "invalid", "Ungültige Anfrage.")
			return
		}
		name, err := o.Kernels.StartKernel(c.Request.Context(), owner, req)
		if err != nil {
			failed(c, owner, "start", err)
			return
		}
		c.JSON(http.StatusCreated, gin.H{"name": name})
	})
	r.GET("/gpu/kernels", func(c *gin.Context) {
		owner := caller(c)
		if owner == nil {
			return
		}
		ov, err := o.Kernels.KernelOverview(c.Request.Context(), owner)
		if err != nil {
			failed(c, owner, "overview", err)
			return
		}
		c.JSON(http.StatusOK, ov)
	})
	r.GET("/gpu/kernels/:id", func(c *gin.Context) {
		owner := caller(c)
		if owner == nil {
			return
		}
		st, err := o.Kernels.KernelStatus(c.Request.Context(), owner, c.Param("id"))
		switch {
		case err != nil:
			failed(c, owner, "status", err)
		case st == nil:
			abort(c, http.StatusNotFound, "not_found", "Kein Kernel-Pod.")
		default:
			c.JSON(http.StatusOK, st)
		}
	})
	r.DELETE("/gpu/kernels/:id", func(c *gin.Context) {
		owner := caller(c)
		if owner == nil {
			return
		}
		if err := o.Kernels.StopKernel(c.Request.Context(), owner, c.Param("id")); err != nil {
			failed(c, owner, "stop", err)
			return
		}
		c.Status(http.StatusNoContent)
	})
}
