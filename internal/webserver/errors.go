package webserver

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/llm-management-api/internal/cluster/gpu"
	"github.com/pfisterer/llm-management-api/internal/cluster/workspace"
)

// apiError is an error as the API answers it: the status, the stable code and the text for people, for REST (ErrorResponse) and MCP (the tool's error text) alike. cause is what classify logged of a 5xx; it never reaches the caller.
type apiError struct {
	Status    int
	Code, Msg string
	cause     error
}

func (e *apiError) Error() string { return e.Msg }

// errArea is how one area of the API answers the errors of its services: which kinds of error it tells apart and what it answers with otherwise. The areas differ on purpose (a missing job is "Job nicht gefunden.", a JupyterHub that is not reachable 502, a broken database 500), so every REST handler and MCP tool names the area of the service it called, and both get the same answer.
type errArea struct {
	// logPrefix goes before what in the log of a 5xx.
	logPrefix string
	// notFound is the sentinel answered 404 with notFoundMsg; nil: none.
	notFound    error
	notFoundMsg string
	// refusals: workspace.Invalid is 400 invalid, workspace.Refusal 409 refused (a refusal by the project's rules, decided 10.11.2026).
	refusals bool
	// jobRefused: gpu.JobRefused is 400 invalid.
	jobRefused bool
	// hub: gpu.ErrInvalidRepo and gpu.ErrRefNotFound are 400 invalid with their text, a gpu.HubError below 500 is 409 hub_refused with JupyterHub's message (e.g. GPU quota exhausted: it is meant for people).
	hub bool
	// fallback is the answer to anything else; its cause is logged.
	fallback apiError
}

var (
	// internalErrors: the service's own database and stores.
	internalErrors = errArea{fallback: apiError{Status: http.StatusInternalServerError, Code: "internal", Msg: "Interner Fehler."}}
	// gatewayErrors: LiteLLM, the LLM gateway.
	gatewayErrors = errArea{fallback: apiError{Status: http.StatusBadGateway, Code: "litellm", Msg: "Das Gateway ist gerade nicht erreichbar."}}
	// projectErrors: the workspace projects.
	projectErrors = errArea{notFound: workspace.ErrNotFound, notFoundMsg: "Projekt nicht gefunden.", refusals: true, fallback: internalErrors.fallback}
	// gpuErrors: the servers in JupyterHub and the GPU cluster.
	gpuErrors = errArea{notFound: gpu.ErrNotFound, notFoundMsg: "Nicht gefunden.", hub: true, fallback: apiError{Status: http.StatusBadGateway, Code: "gpu_cluster", Msg: "GPU-Cluster gerade nicht erreichbar."}}
	// jobErrors: the batch jobs.
	jobErrors = errArea{logPrefix: "batch job ", notFound: gpu.ErrNotFound, notFoundMsg: "Job nicht gefunden.", refusals: true, jobRefused: true, fallback: gpuErrors.fallback}
)

// classify turns a service's error into the API's answer, by the rules of the area, and is the one place that logs the cause of a 5xx (what names the operation, in English). An apiError (e.g. of manager or workspaceAccess) stays as it is; a keyFailure keeps its own status, code and text in every area.
func (s *Server) classify(area errArea, what string, err error) *apiError {
	var ae *apiError
	var kf *keyFailure
	var inv *workspace.Invalid
	var refusal *workspace.Refusal
	var refused *gpu.JobRefused
	var he *gpu.HubError
	var out apiError
	switch {
	case errors.As(err, &ae):
		return ae
	case errors.As(err, &kf):
		out = apiError{Status: kf.status, Code: kf.code, Msg: kf.msg, cause: kf.err}
	case area.refusals && errors.As(err, &inv):
		out = apiError{Status: http.StatusBadRequest, Code: "invalid", Msg: inv.Reason}
	case area.refusals && errors.As(err, &refusal):
		out = apiError{Status: http.StatusConflict, Code: "refused", Msg: refusal.Reason}
	case area.jobRefused && errors.As(err, &refused):
		out = apiError{Status: http.StatusBadRequest, Code: "invalid", Msg: refused.Reason}
	case area.notFound != nil && errors.Is(err, area.notFound):
		out = apiError{Status: http.StatusNotFound, Code: "not_found", Msg: area.notFoundMsg}
	case area.hub && (errors.Is(err, gpu.ErrInvalidRepo) || errors.Is(err, gpu.ErrRefNotFound)):
		out = apiError{Status: http.StatusBadRequest, Code: "invalid", Msg: err.Error()}
	case area.hub && errors.As(err, &he) && he.Status < 500:
		out = apiError{Status: http.StatusConflict, Code: "hub_refused", Msg: he.Message}
	default:
		out = area.fallback
		out.cause = err
	}
	if out.Status >= 500 && out.cause != nil {
		s.log.Errorw(area.logPrefix+what+" failed", "error", out.cause)
	}
	return &out
}

// fail answers err for REST by the rules of the area and aborts; false and nothing written when err is nil. Use: if s.fail(c, jobErrors, "list", err) { return }.
func (s *Server) fail(c *gin.Context, area errArea, what string, err error) bool {
	if err == nil {
		return false
	}
	e := s.classify(area, what, err)
	abort(c, e.Status, e.Code, e.Msg)
	return true
}

// mcpFail is what an MCP tool reports of err: the text REST answers with, by the same rules (fail).
func (s *Server) mcpFail(area errArea, what string, err error) error {
	return errors.New(s.classify(area, what, err).Msg)
}
