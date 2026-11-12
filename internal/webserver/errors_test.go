package webserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/cloud-self-service-golib/msg"
	"github.com/pfisterer/llm-management-api/internal/cluster/gpu"
	"github.com/pfisterer/llm-management-api/internal/cluster/workspace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// TestClassify holds every area's answer to every kind of error, as the five REST mappers answered before classify (internalError, upstreamError, workspaceError, gpuError, jobError): REST and MCP answer from this table, so a change here changes both.
func TestClassify(t *testing.T) {
	boom := errors.New("boom")
	inv := fmt.Errorf("wrapped: %w", &workspace.Invalid{Reason: msg.T("Ungültig.", "Invalid.")})
	refusal := &workspace.Refusal{Reason: msg.T("Abgelehnt.", "Refused.")}
	jobRefused := &gpu.JobRefused{Reason: msg.T("Job abgelehnt.", "Job refused.")}
	hub409 := &gpu.HubError{Status: 403, Message: "GPU-Kontingent erschöpft."}
	hub500 := &gpu.HubError{Status: 500, Message: "hub down"}
	badRepo := fmt.Errorf("%w: nur https", gpu.ErrInvalidRepo)
	badRef := fmt.Errorf("%w: main2", gpu.ErrRefNotFound)
	keyGone := &keyFailure{http.StatusServiceUnavailable, "upstream", msg.T("Kein Key.", "No key."), errors.New("Kein Key.")}
	keyUnknown := &keyFailure{http.StatusConflict, "person_unknown", msg.T("Unbekannt.", "Unknown."), nil}
	given := &apiError{Status: http.StatusForbidden, Code: "forbidden", Msg: msg.T("Nein.", "No.")}

	type want struct {
		status    int
		code, msg string
		logged    bool
	}
	internal := want{500, "internal", "Interner Fehler.", true}
	gateway := want{502, "litellm", "Das Gateway ist gerade nicht erreichbar.", true}
	cluster := want{502, "gpu_cluster", "GPU-Cluster gerade nicht erreichbar.", true}
	invalid := want{400, "invalid", "Ungültig.", false}
	refused := want{409, "refused", "Abgelehnt.", false}
	keyGoneW := want{503, "upstream", "Kein Key.", true}
	keyUnknownW := want{409, "person_unknown", "Unbekannt.", false}
	givenW := want{403, "forbidden", "Nein.", false}

	cases := []struct {
		area string
		err  error
		want want
	}{
		{"internal", boom, internal},
		{"internal", inv, internal},
		{"internal", workspace.ErrNotFound, internal},
		{"internal", given, givenW},

		{"gateway", boom, gateway},
		{"gateway", refusal, gateway},
		{"gateway", keyGone, keyGoneW},
		{"gateway", keyUnknown, keyUnknownW},

		{"project", boom, internal},
		{"project", inv, invalid},
		{"project", refusal, refused},
		{"project", fmt.Errorf("x: %w", workspace.ErrNotFound), want{404, "not_found", "Projekt nicht gefunden.", false}},
		{"project", gpu.ErrNotFound, internal},
		{"project", jobRefused, internal},
		{"project", hub409, internal},
		{"project", given, givenW},

		{"gpu", boom, cluster},
		{"gpu", gpu.ErrNotFound, want{404, "not_found", "Nicht gefunden.", false}},
		{"gpu", workspace.ErrNotFound, cluster},
		{"gpu", badRepo, want{400, "invalid", badRepo.Error(), false}},
		{"gpu", badRef, want{400, "invalid", badRef.Error(), false}},
		{"gpu", hub409, want{409, "hub_refused", "GPU-Kontingent erschöpft.", false}},
		{"gpu", hub500, cluster},
		{"gpu", inv, cluster},
		{"gpu", refusal, cluster},
		{"gpu", jobRefused, cluster},

		{"job", boom, cluster},
		{"job", gpu.ErrNotFound, want{404, "not_found", "Job nicht gefunden.", false}},
		{"job", workspace.ErrNotFound, cluster},
		{"job", inv, invalid},
		{"job", refusal, refused},
		{"job", jobRefused, want{400, "invalid", "Job abgelehnt.", false}},
		{"job", badRepo, cluster},
		{"job", hub409, cluster},
		{"job", keyGone, keyGoneW},
		{"job", fmt.Errorf("submit: %w", keyUnknown), keyUnknownW},
	}
	areas := map[string]errArea{"internal": internalErrors, "gateway": gatewayErrors, "project": projectErrors, "gpu": gpuErrors, "job": jobErrors}
	gin.SetMode(gin.TestMode)
	for _, tc := range cases {
		t.Run(tc.area+"/"+tc.err.Error(), func(t *testing.T) {
			core, logs := observer.New(zapcore.ErrorLevel)
			s := &Server{log: zap.New(core).Sugar()}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
			if !s.fail(c, areas[tc.area], "op", tc.err) {
				t.Fatal("fail returned false for an error")
			}
			var body ErrorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if w.Code != tc.want.status || body.Code != tc.want.code || body.Error != tc.want.msg {
				t.Errorf("REST = %d %q %q, want %d %q %q", w.Code, body.Code, body.Error, tc.want.status, tc.want.code, tc.want.msg)
			}
			if got := s.mcpFail(areas[tc.area], "op", tc.err).Error(); got != tc.want.msg {
				t.Errorf("MCP = %q, want the REST text %q", got, tc.want.msg)
			}
			// Logged once by fail and once by mcpFail when it is a 5xx with a cause, never otherwise.
			if n := logs.Len(); (n == 2) != tc.want.logged || (n != 0 && n != 2) {
				t.Errorf("logged %d times, want logged=%v", n, tc.want.logged)
			}
		})
	}
}

// TestFailNil: no error, no answer.
func TestFailNil(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	s := &Server{log: zap.NewNop().Sugar()}
	if s.fail(c, jobErrors, "op", nil) || c.IsAborted() || w.Body.Len() != 0 {
		t.Fatal("fail answered a nil error")
	}
}
