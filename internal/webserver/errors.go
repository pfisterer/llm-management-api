package webserver

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/cloud-self-service-golib/apierr"
	"github.com/pfisterer/llm-management-api/internal/cluster/gpu"
	"github.com/pfisterer/llm-management-api/internal/cluster/workspace"
)

// apiError is an error as the API answers it: the status, the stable code and the text for people, for REST (ErrorResponse) and MCP (the tool's error text) alike. Its cause is what classify logged of a 5xx; it never reaches the caller. The mechanism is golib's apierr; the tables below are this service's.
type apiError = apierr.Error

// errArea is how one area of the API answers the errors of its services: which kinds of error it tells apart (the rules, in order) and what it answers with otherwise (the fallback). The areas differ on purpose (a missing job is "Job nicht gefunden.", a JupyterHub that is not reachable 502, a broken database 500), so every REST handler and MCP tool names the area of the service it called, and both get the same answer.
type errArea struct {
	// logPrefix goes before what in the log of a 5xx.
	logPrefix string
	apierr.Classifier
}

// area is an errArea with the rules every area has (a keyFailure keeps its own status, code and text) in front of rules, and fallback for anything else.
func area(logPrefix string, fallback apiError, rules ...apierr.Rule) errArea {
	return errArea{logPrefix: logPrefix, Classifier: apierr.Classifier{
		Rules:    append([]apierr.Rule{keyFailureRule}, rules...),
		Fallback: func(error) *apiError { e := fallback; return &e },
	}}
}

// keyFailureRule: a keyFailure keeps its own status, code and text in every area.
var keyFailureRule = apierr.As(func(kf *keyFailure) *apiError {
	return apiError{Status: kf.status, Code: kf.code, Msg: kf.msg}.WithCause(kf.err)
})

var (
	// refusalRules: workspace.Invalid is 400 invalid, workspace.Refusal 409 refused (a refusal by the project's rules, decided 10.11.2026).
	refusalRules = []apierr.Rule{
		apierr.As(func(inv *workspace.Invalid) *apiError { return apierr.New(http.StatusBadRequest, "invalid", inv.Reason) }),
		apierr.As(func(r *workspace.Refusal) *apiError { return apierr.New(http.StatusConflict, "refused", r.Reason) }),
	}
	// jobRefusedRule: gpu.JobRefused is 400 invalid.
	jobRefusedRule = apierr.As(func(r *gpu.JobRefused) *apiError { return apierr.New(http.StatusBadRequest, "invalid", r.Reason) })
	// hubRules: gpu.ErrInvalidRepo and gpu.ErrRefNotFound are 400 invalid with their text, a gpu.HubError below 500 is 409 hub_refused with JupyterHub's message (e.g. GPU quota exhausted: it is meant for people).
	hubRules = []apierr.Rule{
		apierr.IsWithText(gpu.ErrInvalidRepo, http.StatusBadRequest, "invalid"),
		apierr.IsWithText(gpu.ErrRefNotFound, http.StatusBadRequest, "invalid"),
		apierr.As(func(he *gpu.HubError) *apiError {
			if he.Status >= 500 {
				return nil
			}
			return apierr.New(http.StatusConflict, "hub_refused", he.Message)
		}),
	}

	internalFallback = apiError{Status: http.StatusInternalServerError, Code: "internal", Msg: "Interner Fehler."}
	clusterFallback  = apiError{Status: http.StatusBadGateway, Code: "gpu_cluster", Msg: "GPU-Cluster gerade nicht erreichbar."}
)

var (
	// internalErrors: the service's own database and stores.
	internalErrors = area("", internalFallback)
	// gatewayErrors: LiteLLM, the LLM gateway.
	gatewayErrors = area("", apiError{Status: http.StatusBadGateway, Code: "litellm", Msg: "Das Gateway ist gerade nicht erreichbar."})
	// projectErrors: the workspace projects.
	projectErrors = area("", internalFallback, append(refusalRules, apierr.Is(workspace.ErrNotFound, http.StatusNotFound, "not_found", "Projekt nicht gefunden."))...)
	// gpuErrors: the servers in JupyterHub and the GPU cluster.
	gpuErrors = area("", clusterFallback, append([]apierr.Rule{apierr.Is(gpu.ErrNotFound, http.StatusNotFound, "not_found", "Nicht gefunden.")}, hubRules...)...)
	// jobErrors: the batch jobs.
	jobErrors = area("batch job ", clusterFallback, append(refusalRules, jobRefusedRule, apierr.Is(gpu.ErrNotFound, http.StatusNotFound, "not_found", "Job nicht gefunden."))...)
)

// classify turns a service's error into the API's answer, by the rules of the area, and is the one place that logs the cause of a 5xx (what names the operation, in English). An apiError (e.g. of manager or workspaceAccess) stays as it is; a keyFailure keeps its own status, code and text in every area.
func (s *Server) classify(area errArea, what string, err error) *apiError {
	e := area.Classify(err)
	if e.Status >= 500 && e.Cause() != nil {
		s.log.Errorw(area.logPrefix+what+" failed", "error", e.Cause())
	}
	return e
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
	return s.classify(area, what, err).MCPError()
}
