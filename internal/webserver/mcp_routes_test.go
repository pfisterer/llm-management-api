package webserver

import (
	"strings"
	"testing"
	"time"

	"github.com/pfisterer/cloud-self-service-golib/mcpserve/mcptest"
	"github.com/pfisterer/llm-management-api/internal/access"
	"github.com/pfisterer/llm-management-api/internal/cluster/gpu"
	"github.com/pfisterer/llm-management-api/internal/cluster/usage"
	"github.com/pfisterer/llm-management-api/internal/llm/fleet"
	"github.com/pfisterer/llm-management-api/internal/llm/litellm"
	"go.uber.org/zap"
)

// Every route of the API is either covered by an MCP tool or left out with a reason an agent's user could read. A new route matches neither and fails the test, which forces the decision; a stale entry (a route that is gone, a tool that is gone) fails it too.
var mcpRouteCoverage = map[string]mcptest.Route{
	"GET /health":       {Omitted: "no API: the former health check, kept for compatibility"},
	"GET /healthz":      {Omitted: "no API: the liveness probe"},
	"GET /readyz":       {Omitted: "no API: the readiness probe"},
	"GET /config.json":  {Omitted: "no API: the UI's version footer"},
	"GET /swagger.json": {Omitted: "no API: the API description itself"},

	"GET /v1/me": {Tools: []string{"get_me"}},

	"GET /v1/tokens":        {Omitted: "API tokens are the credentials a model runs with; it never lists, issues or revokes them, the API Tokens page does"},
	"POST /v1/tokens":       {Omitted: "issuing a credential hands its secret to the model's context; the person issues tokens on the API Tokens page"},
	"DELETE /v1/tokens/:id": {Omitted: "revoking a credential is the person's act on the API Tokens page, where last use is shown"},

	"GET /v1/usage":                {Tools: []string{"get_my_llm_usage"}},
	"GET /v1/keys":                 {Tools: []string{"get_my_llm_usage"}},
	"POST /v1/keys":                {Omitted: "creates an LLM key and answers with its secret in clear text, which would land in the model's context and transcript; the person makes keys on the LLM page"},
	"PUT /v1/keys/default-project": {Omitted: "decides which project's tokens every new key and the chat spend from; the person's choice on the LLM page"},
	"DELETE /v1/keys/:id":          {Omitted: "revoking a key breaks the tools that use it; the person deletes keys on the LLM page"},
	"GET /v1/models/aliases":       {Tools: []string{"list_llm_models"}},

	"GET /v1/fleet":                                          {Omitted: "administration of the Mac fleet (role fleet-admin), not a user's business"},
	"GET /v1/fleet/inventory.csv":                            {Omitted: "administration of the Mac fleet (role fleet-admin)"},
	"GET /v1/fleet/inference":                                {Omitted: "administration of the Mac fleet (role fleet-admin)"},
	"GET /v1/fleet/profile":                                  {Omitted: "administration of the Mac fleet (role fleet-admin)"},
	"GET /v1/fleet/package":                                  {Omitted: "administration of the Mac fleet (role fleet-admin)"},
	"DELETE /v1/fleet/package":                               {Omitted: "administration of the Mac fleet (admin)"},
	"GET /v1/fleet/readme":                                   {Omitted: "administration of the Mac fleet (role fleet-admin)"},
	"PUT /v1/fleet/model":                                    {Omitted: "administration of the Mac fleet (role fleet-admin): changes the model every machine serves"},
	"POST /v1/fleet/:serial/block":                           {Omitted: "administration of the Mac fleet (role fleet-admin)"},
	"POST /v1/fleet/:serial/unblock":                         {Omitted: "administration of the Mac fleet (role fleet-admin)"},
	"DELETE /v1/fleet/:serial":                               {Omitted: "administration of the Mac fleet (role fleet-admin): forgets a machine"},
	"GET /v1/tree/projects":                                  {Omitted: "machine API of the budget tree with its own token, not a person's"},
	"PUT /v1/tree/projects/:node":                            {Omitted: "machine API of the budget tree with its own token, not a person's"},
	"DELETE /v1/tree/projects/:node":                         {Omitted: "machine API of the budget tree with its own token, not a person's"},
	"GET /v1/tree/catalog":                                   {Omitted: "machine API of the budget tree with its own token, not a person's"},
	"GET /v1/workspaces/usage/me":                            {Tools: []string{"get_my_workspace_usage"}},
	"GET /v1/workspaces/usage":                               {Tools: []string{"get_project_usage"}},
	"GET /v1/workspaces/catalog":                             {Omitted: "the profiles come with get_start_options; the bases and the defaults of storage and release times are for the project settings dialog"},
	"GET /v1/workspaces/projects":                            {Tools: []string{"list_my_projects"}},
	"PUT /v1/workspaces/projects/:slug":                      {Omitted: "project settings (members, environment, release times, LLM project) are a manager's act in the UI, which shows the whole project and the consequences"},
	"POST /v1/workspaces/projects/:slug/image/rebuild":       {Omitted: "building the project's environment is a manager's step in the UI, with the build's log"},
	"POST /v1/workspaces/projects/:slug/image/update":        {Omitted: "taking a new version of the project's image is a manager's step in the UI"},
	"GET /v1/workspaces/projects/:slug/credentials":          {Omitted: "a project's access to private repositories and registries: credentials, managed in the UI"},
	"PUT /v1/workspaces/projects/:slug/credentials":          {Omitted: "takes a token for a private repository or registry, a secret that must not pass through a model"},
	"DELETE /v1/workspaces/projects/:slug/credentials/:host": {Omitted: "credentials are managed in the UI"},
	"GET /v1/workspaces/projects/:slug/image/log":            {Omitted: "the build log of a project's environment, for managers in the UI next to the build"},
	"DELETE /v1/workspaces/projects/:slug":                   {Omitted: "admin: deletes a project with its workspace storage, which cannot be undone"},
	"GET /v1/workspaces/storage":                             {Omitted: "admin: how full the workspace storage is"},
	"GET /v1/workspaces/gpu-hours":                           {Omitted: "the hourly capacity report for admins and managers (how full the GPUs were); get_project_usage gives managers their project's use"},

	"GET /v1/gpu/servers":                 {Tools: []string{"list_my_servers"}},
	"DELETE /v1/gpu/servers/:name":        {Tools: []string{"stop_server"}},
	"GET /v1/gpu/servers/:name/progress":  {Tools: []string{"get_server_progress"}},
	"GET /v1/gpu/start-options":           {Tools: []string{"get_start_options"}},
	"POST /v1/gpu/servers":                {Tools: []string{"start_server"}},
	"POST /v1/gpu/jobs":                   {Tools: []string{"submit_job"}},
	"PUT /v1/gpu/files":                   {Omitted: "uploads a file's bytes (up to 5 GB) into the home; an agent puts files there from a workspace or with the CLI dhbw-cloud"},
	"GET /v1/gpu/jobs":                    {Tools: []string{"list_my_jobs"}},
	"GET /v1/gpu/jobs/:id":                {Tools: []string{"get_job"}},
	"GET /v1/gpu/jobs/:id/log":            {Tools: []string{"get_job_log"}},
	"GET /v1/gpu/jobs/:id/files":          {Tools: []string{"list_job_files"}},
	"GET /v1/gpu/jobs/:id/files/download": {Omitted: "streams a file's bytes (up to 5 GB), which do not belong in a model's context; the CLI dhbw-cloud downloads"},
	"DELETE /v1/gpu/jobs/:id":             {Tools: []string{"cancel_job"}},

	"GET /v1/gpu/fleet":                {Omitted: "GPU administration (admin): pools, tiers and node assignments"},
	"PUT /v1/gpu/fleet/nodes/:name":    {Omitted: "GPU administration (admin)"},
	"PUT /v1/gpu/fleet/pools/:id":      {Omitted: "GPU administration (admin)"},
	"DELETE /v1/gpu/fleet/pools/:id":   {Omitted: "GPU administration (admin)"},
	"GET /v1/inference/models":         {Omitted: "GPU administration (admin): the platform's own model catalogue"},
	"PUT /v1/inference/models/:key":    {Omitted: "GPU administration (admin)"},
	"DELETE /v1/inference/models/:key": {Omitted: "GPU administration (admin)"},
	"PUT /v1/inference/tiers/:tier":    {Omitted: "GPU administration (admin)"},
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
	return mcptest.ToolNames(t, mcptest.InMemory(t, s.newMCPServer(cl)))
}

func TestMCPCoversEveryRoute(t *testing.T) {
	s := fullServer(t)
	tools := mcpToolNames(t, s, mcpCaller{Caller: Caller{Identity: Identity{Email: "admin@dhbw.de"}, Person: access.Person{Role: access.RoleAdmin}}})
	if len(tools) < 10 {
		t.Fatalf("an admin with a writing credential should see every tool, got %v", tools)
	}

	var routes []string
	for _, r := range s.Router().Routes() {
		if r.Path != "/mcp" {
			routes = append(routes, r.Method+" "+r.Path)
		}
	}
	mcptest.CheckRoutes(t, routes, tools, mcpRouteCoverage)
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
