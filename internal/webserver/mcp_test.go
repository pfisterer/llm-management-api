package webserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pfisterer/llm-management-api/internal/cluster/gpu"
)

// bearerTransport puts the API token on every request, the way an MCP client configured with one does.
type bearerTransport struct {
	secret string
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", "Bearer "+b.secret)
	return http.DefaultTransport.RoundTrip(clone)
}

// mcpSession connects to the real router's /mcp (behind the real authentication) with an API token, as an MCP client does.
func mcpSession(t *testing.T, h http.Handler, secret string) *mcp.ClientSession {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	transport := &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", HTTPClient: &http.Client{Transport: bearerTransport{secret: secret}}}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect to /mcp: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func callTool(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	return res
}

func toolText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func sessionToolNames(t *testing.T, session *mcp.ClientSession) []string {
	t.Helper()
	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names
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
	session := mcpSession(t, e.h, e.token(t, jzMember, true))
	names := sessionToolNames(t, session)
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
	res := callTool(t, session, "get_me", nil)
	var me MeResponse
	if err := json.Unmarshal([]byte(toolText(res)), &me); err != nil || res.IsError || me.Email != jzMember || me.WorkspaceProjects != 1 {
		t.Fatalf("get_me: %v %s (%v)", res.IsError, toolText(res), err)
	}
	// Somebody without a project is refused the workspaces, with the REST route's words.
	nobody := mcpSession(t, e.h, e.token(t, jzNobody, true))
	if res := callTool(t, nobody, "list_my_servers", nil); !res.IsError || !strings.Contains(toolText(res), "Kein Zugang zu Workspaces") {
		t.Fatalf("nobody's servers: %v %s", res.IsError, toolText(res))
	}
}

// A job submitted over MCP is the same job REST shows, checked and refused by the same rules.
func TestMCPSubmitJobLikeREST(t *testing.T) {
	e := newJobEnv(t)
	session := mcpSession(t, e.h, e.token(t, jzMember, false))
	res := callTool(t, session, "submit_job", map[string]any{"project": "t-kurs", "command": "true", "name": "mcp-1"})
	if res.IsError {
		t.Fatalf("submit_job: %s", toolText(res))
	}
	var job gpu.Job
	if err := json.Unmarshal([]byte(toolText(res)), &job); err != nil || job.ID == "" || job.Project != "t-kurs" || job.Person != jzMember {
		t.Fatalf("submit_job answer: %s (%v)", toolText(res), err)
	}
	code, body := call(t, e.h, "GET", "/v1/gpu/jobs", e.token(t, jzMember, true), "", nil)
	if code != http.StatusOK || !strings.Contains(string(body), job.ID) {
		t.Fatalf("REST does not list the MCP job: %d %s", code, body)
	}
	if res := callTool(t, session, "get_job", map[string]any{"id": job.ID}); res.IsError {
		t.Fatalf("get_job: %s", toolText(res))
	}
	// Another project's job is invisible, with the same answer as an unknown one (REST: 404).
	outsider := mcpSession(t, e.h, e.token(t, jzOutsider, false))
	if res := callTool(t, outsider, "get_job", map[string]any{"id": job.ID}); !res.IsError || !strings.Contains(toolText(res), "nicht gefunden") {
		t.Fatalf("outsider sees the job: %v %s", res.IsError, toolText(res))
	}
	// A project the caller is no member of is refused as over REST (409 with this reason, the same for an unknown project).
	if res := callTool(t, session, "submit_job", map[string]any{"project": "t-fremd", "command": "true"}); !res.IsError || !strings.Contains(toolText(res), "kein Zugang") {
		t.Fatalf("foreign project: %v %s", res.IsError, toolText(res))
	}
	if res := callTool(t, session, "cancel_job", map[string]any{"id": job.ID}); res.IsError {
		t.Fatalf("cancel_job: %s", toolText(res))
	}
}
