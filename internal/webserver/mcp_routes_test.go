package webserver

import (
	"context"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pfisterer/llm-management-api/internal/access"
	"github.com/pfisterer/llm-management-api/internal/cluster/gpu"
	"github.com/pfisterer/llm-management-api/internal/cluster/usage"
	"github.com/pfisterer/llm-management-api/internal/llm/fleet"
	"github.com/pfisterer/llm-management-api/internal/llm/litellm"
	"go.uber.org/zap"
)

// Every route of the API is either covered by an MCP tool or left out with a reason an agent's user could read. A new route matches neither and fails the test, which forces the decision; a stale entry (a route that is gone, a tool that is gone) fails it too.
type routeDecision struct {
	// tool is the MCP tool that offers what the route offers; one tool may cover several routes.
	tool string
	// omitted says why an agent does not get the route.
	omitted string
}

var mcpRouteCoverage = map[string]routeDecision{
	"GET /health":       {omitted: "no API: the probes' health check"},
	"GET /config.json":  {omitted: "no API: the UI's version footer"},
	"GET /swagger.json": {omitted: "no API: the API description itself"},

	"GET /v1/me": {tool: "get_me"},

	"GET /v1/tokens":        {omitted: "API tokens are the credentials a model runs with; it never lists, issues or revokes them, the API Tokens page does"},
	"POST /v1/tokens":       {omitted: "issuing a credential hands its secret to the model's context; the person issues tokens on the API Tokens page"},
	"DELETE /v1/tokens/:id": {omitted: "revoking a credential is the person's act on the API Tokens page, where last use is shown"},

	"GET /v1/usage":                {tool: "get_my_llm_usage"},
	"GET /v1/keys":                 {tool: "get_my_llm_usage"},
	"POST /v1/keys":                {omitted: "creates an LLM key and answers with its secret in clear text, which would land in the model's context and transcript; the person makes keys on the LLM page"},
	"PUT /v1/keys/default-project": {omitted: "decides which project's tokens every new key and the chat spend from; the person's choice on the LLM page"},
	"DELETE /v1/keys/:id":          {omitted: "revoking a key breaks the tools that use it; the person deletes keys on the LLM page"},
	"GET /v1/models/aliases":       {tool: "list_llm_models"},

	"GET /v1/fleet":                                          {omitted: "administration of the Mac fleet (role fleet-admin), not a user's business"},
	"GET /v1/fleet/inventory.csv":                            {omitted: "administration of the Mac fleet (role fleet-admin)"},
	"GET /v1/fleet/inference":                                {omitted: "administration of the Mac fleet (role fleet-admin)"},
	"GET /v1/fleet/profile":                                  {omitted: "administration of the Mac fleet (role fleet-admin)"},
	"GET /v1/fleet/package":                                  {omitted: "administration of the Mac fleet (role fleet-admin)"},
	"DELETE /v1/fleet/package":                               {omitted: "administration of the Mac fleet (admin)"},
	"GET /v1/fleet/readme":                                   {omitted: "administration of the Mac fleet (role fleet-admin)"},
	"PUT /v1/fleet/model":                                    {omitted: "administration of the Mac fleet (role fleet-admin): changes the model every machine serves"},
	"POST /v1/fleet/:serial/block":                           {omitted: "administration of the Mac fleet (role fleet-admin)"},
	"POST /v1/fleet/:serial/unblock":                         {omitted: "administration of the Mac fleet (role fleet-admin)"},
	"DELETE /v1/fleet/:serial":                               {omitted: "administration of the Mac fleet (role fleet-admin): forgets a machine"},
	"GET /v1/tree/projects":                                  {omitted: "machine API of the budget tree with its own token, not a person's"},
	"PUT /v1/tree/projects/:node":                            {omitted: "machine API of the budget tree with its own token, not a person's"},
	"DELETE /v1/tree/projects/:node":                         {omitted: "machine API of the budget tree with its own token, not a person's"},
	"GET /v1/tree/catalog":                                   {omitted: "machine API of the budget tree with its own token, not a person's"},
	"GET /v1/workspaces/usage/me":                            {tool: "get_my_workspace_usage"},
	"GET /v1/workspaces/usage":                               {tool: "get_project_usage"},
	"GET /v1/workspaces/catalog":                             {omitted: "the profiles come with get_start_options; the bases and the defaults of storage and release times are for the project settings dialog"},
	"GET /v1/workspaces/projects":                            {tool: "list_my_projects"},
	"PUT /v1/workspaces/projects/:slug":                      {omitted: "project settings (members, environment, release times, LLM project) are a manager's act in the UI, which shows the whole project and the consequences"},
	"POST /v1/workspaces/projects/:slug/image/rebuild":       {omitted: "building the project's environment is a manager's step in the UI, with the build's log"},
	"POST /v1/workspaces/projects/:slug/image/update":        {omitted: "taking a new version of the project's image is a manager's step in the UI"},
	"GET /v1/workspaces/projects/:slug/credentials":          {omitted: "a project's access to private repositories and registries: credentials, managed in the UI"},
	"PUT /v1/workspaces/projects/:slug/credentials":          {omitted: "takes a token for a private repository or registry, a secret that must not pass through a model"},
	"DELETE /v1/workspaces/projects/:slug/credentials/:host": {omitted: "credentials are managed in the UI"},
	"GET /v1/workspaces/projects/:slug/image/log":            {omitted: "the build log of a project's environment, for managers in the UI next to the build"},
	"DELETE /v1/workspaces/projects/:slug":                   {omitted: "admin: deletes a project with its workspace storage, which cannot be undone"},
	"GET /v1/workspaces/storage":                             {omitted: "admin: how full the workspace storage is"},
	"GET /v1/workspaces/gpu-hours":                           {omitted: "the hourly capacity report for admins and managers (how full the GPUs were); get_project_usage gives managers their project's use"},

	"GET /v1/gpu/servers":                 {tool: "list_my_servers"},
	"DELETE /v1/gpu/servers/:name":        {tool: "stop_server"},
	"GET /v1/gpu/servers/:name/progress":  {tool: "get_server_progress"},
	"GET /v1/gpu/start-options":           {tool: "get_start_options"},
	"POST /v1/gpu/servers":                {tool: "start_server"},
	"POST /v1/gpu/jobs":                   {tool: "submit_job"},
	"PUT /v1/gpu/files":                   {omitted: "uploads a file's bytes (up to 5 GB) into the home; an agent puts files there from a workspace or with the CLI dhbw-cloud"},
	"GET /v1/gpu/jobs":                    {tool: "list_my_jobs"},
	"GET /v1/gpu/jobs/:id":                {tool: "get_job"},
	"GET /v1/gpu/jobs/:id/log":            {tool: "get_job_log"},
	"GET /v1/gpu/jobs/:id/files":          {tool: "list_job_files"},
	"GET /v1/gpu/jobs/:id/files/download": {omitted: "streams a file's bytes (up to 5 GB), which do not belong in a model's context; the CLI dhbw-cloud downloads"},
	"DELETE /v1/gpu/jobs/:id":             {tool: "cancel_job"},

	"GET /v1/gpu/fleet":                {omitted: "GPU administration (admin): pools, tiers and node assignments"},
	"PUT /v1/gpu/fleet/nodes/:name":    {omitted: "GPU administration (admin)"},
	"PUT /v1/gpu/fleet/pools/:id":      {omitted: "GPU administration (admin)"},
	"DELETE /v1/gpu/fleet/pools/:id":   {omitted: "GPU administration (admin)"},
	"GET /v1/inference/models":         {omitted: "GPU administration (admin): the platform's own model catalogue"},
	"PUT /v1/inference/models/:key":    {omitted: "GPU administration (admin)"},
	"DELETE /v1/inference/models/:key": {omitted: "GPU administration (admin)"},
	"PUT /v1/inference/tiers/:tier":    {omitted: "GPU administration (admin)"},
}

// fullServer has every part switched on, so Router registers every route there is: API tokens, aliases, the Mac fleet, the tree, workspaces with usage, servers, batch jobs, GPU administration and the model catalogue.
func fullServer(t *testing.T) *Server {
	t.Helper()
	env := newJobEnv(t, func(o *Options, _ *gpu.Config) {
		o.Usage = usage.NewMemoryStore()
		o.Keys = testKeys(fakeBackend(t).URL)
		o.TreeToken = "tree"
		o.Aliases = NewModelAliases(litellm.New("http://127.0.0.1:1", "m", time.Second), []string{"chat-default"}, zap.NewNop().Sugar())
		fc, wg, err := fleet.ParseConfig(`{"enabled":true,"enrollToken":"e","adminToken":"a"}`, `{"hubPublicKey":"HUB="}`)
		if err != nil {
			t.Fatal(err)
		}
		o.Fleet = fleet.NewService(fc, wg, fleet.NewMemoryStore(), fleet.NewScripts(t.TempDir()))
	})
	env.gpu.SetFleet(gpu.NewMemoryFleetStore(), gpu.FleetConfig{})
	env.gpu.SetModels(gpu.NewMemoryModelStore())
	return env.api
}

// mcpToolNames are the tools the server offers the caller, read over an in-memory MCP session.
func mcpToolNames(t *testing.T, s *Server, cl mcpCaller) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	clientT, serverT := mcp.NewInMemoryTransports()
	ss, err := s.newMCPServer(cl).Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	return names
}

func TestMCPCoversEveryRoute(t *testing.T) {
	s := fullServer(t)
	tools := mcpToolNames(t, s, mcpCaller{Caller: Caller{Identity: Identity{Email: "admin@dhbw.de"}, Person: access.Person{Role: access.RoleAdmin}}})
	if len(tools) < 10 {
		t.Fatalf("an admin with a writing credential should see every tool, got %v", tools)
	}

	routes := map[string]bool{}
	for _, r := range s.Router().Routes() {
		if r.Path == "/mcp" {
			continue
		}
		key := r.Method + " " + r.Path
		routes[key] = true
		d, ok := mcpRouteCoverage[key]
		switch {
		case !ok:
			t.Errorf("%s is covered by no tool and has no reason in mcpRouteCoverage: add the tool to mcp.go, or write down why an agent does not get it", key)
		case d.tool == "" && d.omitted == "":
			t.Errorf("%s: a decision needs a tool or a reason", key)
		case d.tool != "" && !slices.Contains(tools, d.tool):
			t.Errorf("%s names the tool %q, which the server does not offer", key, d.tool)
		}
	}
	// Stale entries: a route that is gone still has a decision, or a tool covers no route of the API.
	for key := range mcpRouteCoverage {
		if !routes[key] {
			t.Errorf("mcpRouteCoverage lists %s, which the router no longer has", key)
		}
	}
	for _, tool := range tools {
		covers := false
		for _, d := range mcpRouteCoverage {
			covers = covers || d.tool == tool
		}
		if !covers {
			t.Errorf("the tool %q covers no route of the API; every tool mirrors a REST route, so both stay in step", tool)
		}
	}
	// A read-only credential is shown no writing tool.
	ro := mcpToolNames(t, s, mcpCaller{Caller: Caller{Identity: Identity{Email: "admin@dhbw.de"}, Person: access.Person{Role: access.RoleAdmin}}, readOnly: true})
	for _, name := range ro {
		if strings.HasPrefix(name, "start_") || strings.HasPrefix(name, "stop_") || strings.HasPrefix(name, "submit_") || strings.HasPrefix(name, "cancel_") {
			t.Errorf("a read-only credential is offered the writing tool %q", name)
		}
	}
	if len(ro) == len(tools) {
		t.Errorf("a read-only credential sees as many tools as a writing one (%d); the mutating ones must be left out", len(ro))
	}
}
