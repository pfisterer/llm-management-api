package webserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/pfisterer/cloud-self-service-golib/mcpserve/mcptest"
	"github.com/pfisterer/llm-management-api/internal/cluster/gpu"
)

// mcpEndpoint serves the real router (with the real authentication) and returns its /mcp, for mcptest.Dial with an API token as an MCP client connects.
func mcpEndpoint(t *testing.T, h http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL + "/mcp"
}

// Without a credential /mcp is refused like /v1: no identity, no tools.
func TestMCPNeedsAuthentication(t *testing.T) {
	e := newJobEnv(t)
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no bearer: want 401, got %d %s", rec.Code, rec.Body.String())
	}
}

// A read-only token lists and reads but is shown no writing tool; get_me answers as /v1/me does for the token's owner.
func TestMCPReadOnlyToken(t *testing.T) {
	e := newJobEnv(t)
	endpoint := mcpEndpoint(t, e.h)
	session := mcptest.Dial(t, endpoint, e.token(t, jzMember, true))
	names := mcptest.ToolNames(t, session)
	for _, want := range []string{"get_me", "list_my_servers", "get_start_options", "list_my_jobs", "get_job"} {
		if !slices.Contains(names, want) {
			t.Errorf("read tool %q missing from %v", want, names)
		}
	}
	for _, write := range []string{"start_server", "stop_server", "submit_job", "cancel_job"} {
		if slices.Contains(names, write) {
			t.Errorf("writing tool %q offered to a read-only token", write)
		}
	}
	res := mcptest.Call(t, session, "get_me", nil)
	var me MeResponse
	if err := json.Unmarshal([]byte(mcptest.Text(res)), &me); err != nil || res.IsError || me.Email != jzMember || me.WorkspaceProjects != 1 {
		t.Fatalf("get_me: %v %s (%v)", res.IsError, mcptest.Text(res), err)
	}
	// Somebody without a project is refused the workspaces, with the REST route's words.
	nobody := mcptest.Dial(t, endpoint, e.token(t, jzNobody, true))
	if res := mcptest.Call(t, nobody, "list_my_servers", nil); !res.IsError || !strings.Contains(mcptest.Text(res), "Kein Zugang zu Workspaces") {
		t.Fatalf("nobody's servers: %v %s", res.IsError, mcptest.Text(res))
	}
}

// A job submitted over MCP is the same job REST shows, checked and refused by the same rules.
func TestMCPSubmitJobLikeREST(t *testing.T) {
	e := newJobEnv(t)
	endpoint := mcpEndpoint(t, e.h)
	session := mcptest.Dial(t, endpoint, e.token(t, jzMember, false))
	res := mcptest.Call(t, session, "submit_job", map[string]any{"project": "t-kurs", "command": "true", "name": "mcp-1"})
	if res.IsError {
		t.Fatalf("submit_job: %s", mcptest.Text(res))
	}
	var job gpu.Job
	if err := json.Unmarshal([]byte(mcptest.Text(res)), &job); err != nil || job.ID == "" || job.Project != "t-kurs" || job.Person != jzMember {
		t.Fatalf("submit_job answer: %s (%v)", mcptest.Text(res), err)
	}
	code, body := call(t, e.h, "GET", "/v1/gpu/jobs", e.token(t, jzMember, true), "", nil)
	if code != http.StatusOK || !strings.Contains(string(body), job.ID) {
		t.Fatalf("REST does not list the MCP job: %d %s", code, body)
	}
	if res := mcptest.Call(t, session, "get_job", map[string]any{"id": job.ID}); res.IsError {
		t.Fatalf("get_job: %s", mcptest.Text(res))
	}
	// Another project's job is invisible, with the same answer as an unknown one (REST: 404).
	outsider := mcptest.Dial(t, endpoint, e.token(t, jzOutsider, false))
	if res := mcptest.Call(t, outsider, "get_job", map[string]any{"id": job.ID}); !res.IsError || !strings.Contains(mcptest.Text(res), "nicht gefunden") {
		t.Fatalf("outsider sees the job: %v %s", res.IsError, mcptest.Text(res))
	}
	// A project the caller is no member of is refused as over REST (409 with this reason, the same for an unknown project).
	if res := mcptest.Call(t, session, "submit_job", map[string]any{"project": "t-fremd", "command": "true"}); !res.IsError || !strings.Contains(mcptest.Text(res), "kein Zugang") {
		t.Fatalf("foreign project: %v %s", res.IsError, mcptest.Text(res))
	}
	if res := mcptest.Call(t, session, "cancel_job", map[string]any{"id": job.ID}); res.IsError {
		t.Fatalf("cancel_job: %s", mcptest.Text(res))
	}
}
