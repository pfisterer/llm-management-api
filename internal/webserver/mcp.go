package webserver

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pfisterer/cloud-self-service-golib/ginweb"
	"github.com/pfisterer/cloud-self-service-golib/mcpserve"
	"github.com/pfisterer/cloud-self-service-golib/msg"
	"github.com/pfisterer/cloud-self-service-golib/page"
	"github.com/pfisterer/llm-management-api/internal/access"
	"github.com/pfisterer/llm-management-api/internal/cluster/gpu"
	"github.com/pfisterer/llm-management-api/internal/cluster/usage"
	"github.com/pfisterer/llm-management-api/internal/cluster/workspace"
	"github.com/pfisterer/llm-management-api/internal/llm/keys"
)

// The MCP endpoint (/mcp), so an agent can work with this API on a person's behalf: their workspace projects with budgets and usage, their LLM pools and keys, their servers and batch jobs; starting and stopping a server, submitting and cancelling a job.
//
// It lives in this service, as in openstack-management-api and dynamic-zones-api: /mcp is mounted with the same withCaller as /v1, so a request carries the same identity (sign-in or API token aiaas_…, X-Act-As for admins) and the same role and project rights, resolved per request. Every tool calls the functions the REST handlers call (projectsOf, startOptionsFor, submitJob, visibleJob, …), never the REST API over HTTP, so a rule is right or wrong in one place; mcp.go performs no authorization of its own beyond what the matching handler does (requireWorkspace, the manager rule).
//
// What it does not inherit is RejectWritesForReadOnlyTokens: every MCP call is a POST, so the tool says whether it writes (the mutates argument of mcpserve.AddTool) and a read-only token is not shown the writing tools. Nothing here hands out a secret: the LLM keys are listed without their key, API tokens are not offered at all. mcp_input_drift_test.go holds every tool input against the request type it feeds; mcp_routes_test.go holds every route of the API against the tool that covers it or the reason none does.

// mcpCaller is the caller as the tools act for them: the REST caller (identity, role, tokens, the person an admin acts for) and whether the credential may write.
type mcpCaller struct {
	Caller
	readOnly bool
}

// ReadOnly satisfies mcpserve.Caller: whether this request's credential may change anything.
func (c mcpCaller) ReadOnly() bool { return c.readOnly }

func (c mcpCaller) admin() bool { return c.Role.AtLeast(access.RoleAdmin) }

// registerMCP mounts the MCP endpoint on the group, which carries withCaller and not RejectWritesForReadOnlyTokens; a server per request, built around the caller, so one request's tools cannot run with another's rights.
func (s *Server) registerMCP(group *gin.RouterGroup) {
	handler := mcpserve.Handler(func(cl mcpCaller) *mcp.Server { return s.newMCPServer(cl) })
	// mcpserve.Handler answers an OPTIONS request itself (405, Allow: POST): it is no MCP call, and it does not reach the SDK.
	group.Any("", func(c *gin.Context) {
		ctx := mcpserve.WithCaller(c.Request.Context(), mcpCaller{Caller: caller(c), readOnly: ginweb.IsReadOnly(c)})
		handler.ServeHTTP(c.Writer, c.Request.WithContext(ctx))
	})
}

// newMCPServer registers the tools the parts of the service allow, under the same conditions as Router registers their routes.
func (s *Server) newMCPServer(cl mcpCaller) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "dhbw-cloud-ai", Title: "DHBW Cloud – KI-Dienst: LLM-Keys, Workspaces und Batch-Jobs", Version: s.version}, nil)
	s.registerMeTools(srv, cl)
	if s.keys != nil {
		s.registerLLMTools(srv, cl)
	}
	if s.gpu != nil && s.usage != nil && s.ws != nil {
		s.registerWorkspaceTools(srv, cl)
	}
	if s.gpu != nil && s.ws != nil {
		s.registerServerTools(srv, cl)
		if s.gpu.JobsEnabled() {
			s.registerJobTools(srv, cl)
		}
	}
	return srv
}

// ── Tool payloads ───────────────────────────────────────────────────────────
// Where the REST types are compact and carry no secret (MeResponse, gpu.Server, gpu.Job, gpu.JobFiles, usage.Report, keys.Usage, ModelAliasesResponse) the tools answer with them; a project is given as the fields a person asks about, not as workspace.Mine with its image internals.

type mcpProject struct {
	Slug        string           `json:"slug" jsonschema:"the project's short name; what the other tools take as project"`
	Name        string           `json:"name"`
	Role        string           `json:"role" jsonschema:"the caller's role in it: reader, member, manager or admin"`
	Description string           `json:"description,omitempty"`
	Profiles    []string         `json:"profiles" jsonschema:"the workspace profiles (server types) the project allows, e.g. cpu, gpu, gpu-kernel"`
	GPUClasses  []string         `json:"gpu_classes" jsonschema:"the GPU classes the project may use"`
	Limits      workspace.Limits `json:"limits" jsonschema:"per person at the same time: servers, cores, RAM, GPUs; batch jobs and their longest run time in hours (0 jobs = no batch jobs)"`
	Budgets     []mcpBudget      `json:"budgets" jsonschema:"the project's budgets with what is used and reserved in their window; an exhausted one stops starts and jobs"`
	LLMMTokens  int              `json:"llm_mtokens" jsonschema:"LLM tokens per 30 days in millions that the project's members share; 0 = none"`
	// NoWorkspaces: the tree gives the project LLM tokens only; it is not offered for servers or jobs.
	NoWorkspaces  bool       `json:"no_workspaces,omitempty" jsonschema:"true: LLM tokens only, no workspaces or batch jobs"`
	StorageGB     int        `json:"storage_gb" jsonschema:"the project's workspace storage in GB (the members' homes and the shared folder); 0 = none"`
	ImagePending  bool       `json:"image_pending,omitempty" jsonschema:"true: the project's environment is still being built from Git; starts and jobs wait for it"`
	BatchPriority string     `json:"batch_priority,omitempty" jsonschema:"guaranteed: batch jobs run like workspaces and are never evicted; empty: a workspace may evict a job, which then starts again"`
	EndsAt        *time.Time `json:"ends_at,omitempty" jsonschema:"the project's last day; nothing starts after it"`
}

// mcpBudget is workspace.BudgetStatus flat (the SDK's schema does not follow embedded structs the way encoding/json does).
type mcpBudget struct {
	Unit      string  `json:"unit" jsonschema:"server_hours, vcpu_hours, ram_gb_hours or gpu_hours"`
	GPUClass  string  `json:"gpu_class,omitempty" jsonschema:"only for gpu_hours; empty = all classes"`
	Window    string  `json:"window" jsonschema:"24h, 7d, 30d or total"`
	Amount    float64 `json:"amount"`
	PerPerson bool    `json:"per_person" jsonschema:"true: the amount is per person, else for the whole project"`
	Used      float64 `json:"used" jsonschema:"used in the window; for a per-person budget by the caller"`
	Reserved  float64 `json:"reserved,omitempty" jsonschema:"what running and waiting batch jobs still hold of a gpu_hours budget; counts as used"`
	Exhausted bool    `json:"exhausted"`
}

func toMCPProject(m workspace.Mine) mcpProject {
	p := mcpProject{Slug: m.Slug, Name: m.Name, Role: m.Role, Description: m.Description, Profiles: m.Profiles, GPUClasses: m.GPUClasses, Limits: m.Limits, Budgets: []mcpBudget{},
		LLMMTokens: m.LLMMTokens, NoWorkspaces: m.NoWorkspaces, StorageGB: m.StorageGB, ImagePending: m.ImagePending(), BatchPriority: m.BatchPriority, EndsAt: m.EndsAt}
	for _, b := range m.BudgetStatus {
		p.Budgets = append(p.Budgets, mcpBudget{Unit: b.Unit, GPUClass: b.GPUClass, Window: b.Window, Amount: b.Amount, PerPerson: b.PerPerson, Used: b.Used, Reserved: b.Reserved, Exhausted: b.Exhausted})
	}
	return p
}

type mcpProjectList struct {
	Projects []mcpProject `json:"projects"`
	// Total travels along so a model can tell "that is all of them" from "there is more behind this page" (golib page.Input).
	Total int `json:"total"`
}

// mcpStartOptions is StartOptionsResponse as a start chooses from it, without the hub's internals (secret names, material, release times).
type mcpStartOptions struct {
	Projects    []mcpStartProject   `json:"projects" jsonschema:"the caller's projects a server or job may run in"`
	Profiles    []workspace.Profile `json:"profiles" jsonschema:"every profile (server type) with whether it uses a GPU and its idle and maximum times; a project offers a subset"`
	LLMProjects []HubLLMProject     `json:"llm_projects" jsonschema:"the caller's projects with LLM tokens, for llm_project of start_server and submit_job; besides them \"none\" is always possible"`
}

type mcpStartProject struct {
	Slug         string        `json:"slug"`
	Name         string        `json:"name"`
	Profiles     []string      `json:"profiles" jsonschema:"the profiles a start may choose here"`
	GPUClasses   []HubGPUClass `json:"gpu_classes" jsonschema:"the GPU classes a start may choose here and whether GPUs of them are free right now; gpu_class may be left empty when there is exactly one"`
	LLMDefault   string        `json:"llm_default" jsonschema:"whose LLM tokens a start without llm_project gets: a project slug, or none"`
	ImagePending bool          `json:"image_pending,omitempty" jsonschema:"true: the environment is still being built; starts are refused until it is ready"`
	StorageGB    int           `json:"storage_gb" jsonschema:"the project's storage in GB; 0 = servers start with an empty home that is lost when they end"`
	ScratchGB    int           `json:"scratch_gb" jsonschema:"local storage in GB a server may write outside the home"`
	EndsAt       *time.Time    `json:"ends_at,omitempty"`
}

func toMCPStartOptions(o StartOptionsResponse) mcpStartOptions {
	out := mcpStartOptions{Projects: []mcpStartProject{}, Profiles: o.Profiles, LLMProjects: o.LLMProjects}
	for _, p := range o.Projects {
		out.Projects = append(out.Projects, mcpStartProject{Slug: p.Slug, Name: p.Name, Profiles: p.Profiles, GPUClasses: p.GPUClasses, LLMDefault: p.LLMDefault, ImagePending: p.ImagePending, StorageGB: p.StorageGB, ScratchGB: p.ScratchGB, EndsAt: p.EndsAt})
	}
	return out
}

type mcpServerList struct {
	Servers []gpu.Server `json:"servers"`
	Total   int          `json:"total"`
}

type mcpJobList struct {
	Jobs  []gpu.Job `json:"jobs"`
	Total int       `json:"total"`
}

// mcpDone is the answer of a tool that has nothing to return but that it did it.
type mcpDone struct {
	Done    bool   `json:"done"`
	Message string `json:"message"`
}

// ── Tool inputs ─────────────────────────────────────────────────────────────
// Package-level so mcp_input_drift_test.go can reflect over them.

type mcpPeriodInput struct {
	From string `json:"from,omitempty" jsonschema:"first day YYYY-MM-DD; default: 30 days before to, so the period is 30 days long"`
	To   string `json:"to,omitempty" jsonschema:"last day YYYY-MM-DD, included; default: yesterday (the last finished day); days are calendar days in Europe/Berlin"`
}

type mcpProjectPeriodInput struct {
	Project string `json:"project,omitempty" jsonschema:"the project's slug; required for a project's managers, an admin may leave it empty for everything"`
	mcpPeriodInput
}

type mcpServerNameInput struct {
	Name string `json:"name,omitempty" jsonschema:"the server's name as list_my_servers shows it; empty for the default server"`
}

type mcpStartServerInput struct {
	Project    string `json:"project" jsonschema:"slug of one of the caller's projects (get_start_options)"`
	Profile    string `json:"profile" jsonschema:"one of the project's profiles (get_start_options)"`
	GPUClass   string `json:"gpu_class,omitempty" jsonschema:"for a profile with a GPU: one of the project's GPU classes; may be left empty when the project has exactly one"`
	LLMProject string `json:"llm_project,omitempty" jsonschema:"whose LLM tokens the server's key draws from: a slug from llm_projects of get_start_options, none for a server without LLM key, or empty for the project's default (llm_default)"`
}

type mcpJobListInput struct {
	Project string `json:"project,omitempty" jsonschema:"only this project: the caller's jobs there, or all of the project for its managers and admins; empty = the caller's own jobs everywhere"`
	page.Input
}

type mcpJobInput struct {
	ID string `json:"id" jsonschema:"the job's id, as list_my_jobs shows it"`
}

type mcpJobLogInput struct {
	ID    string `json:"id" jsonschema:"the job's id"`
	Lines int    `json:"lines,omitempty" jsonschema:"how many lines from the end while the job runs; default 200, at most 5000"`
}

type mcpJobFilesInput struct {
	ID   string `json:"id" jsonschema:"the job's id"`
	Path string `json:"path,omitempty" jsonschema:"folder relative to the home the job ran in; default: the job's folder jobs/<name> with its output and checkpoints"`
}

type mcpSubmitJobInput struct {
	Project     string  `json:"project" jsonschema:"slug of one of the caller's projects with batch jobs (list_my_projects: limits.jobs_per_person > 0)"`
	Name        string  `json:"name,omitempty" jsonschema:"names the job and its output folder ~/jobs/<name>; empty = its id"`
	GPUClass    string  `json:"gpu_class,omitempty" jsonschema:"one of the project's GPU classes; may be left empty when the project has exactly one"`
	Command     string  `json:"command,omitempty" jsonschema:"a shell command run in working_dir, e.g. python train.py --epochs 10; exactly one of command and notebook"`
	Notebook    string  `json:"notebook,omitempty" jsonschema:"a notebook in the home run with papermill, e.g. projects/kurs/training.ipynb; exactly one of command and notebook"`
	WorkingDir  string  `json:"working_dir,omitempty" jsonschema:"folder relative to the home the command runs in; default: the home"`
	MaxHours    float64 `json:"max_hours,omitempty" jsonschema:"how long the job may run at the latest, which it reserves of the project's GPU hours; 0 = the project's maximum (limits.job_max_hours)"`
	LLMProject  string  `json:"llm_project,omitempty" jsonschema:"whose LLM tokens the job's key (OPENAI_API_KEY) draws from: a slug from llm_projects of get_start_options, none for no key, or empty for the project's default as for its workspaces"`
	Preemptible bool    `json:"preemptible,omitempty" jsonschema:"true: the job yields its GPU to workspaces although the project's budget would run it guaranteed"`
}

// ── Tools ───────────────────────────────────────────────────────────────────

func (s *Server) registerMeTools(srv *mcp.Server, cl mcpCaller) {
	mcpserve.AddTool(srv, cl, false, &mcp.Tool{
		Name:        "get_me",
		Description: "Who the caller is: e-mail address, role (user, fleet-admin, admin), the LLM tokens per 30 days of their projects added up in millions (0 = no LLM access), how many workspace projects they belong to, and the addresses of the chat, the LLM API and JupyterHub.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, MeResponse, error) {
		projects := 0
		if s.gpu != nil {
			projects = len(s.projectsOf(ctx, cl.Caller))
		}
		return nil, s.me(ctx, cl.Caller, projects), nil
	})
}

func (s *Server) registerLLMTools(srv *mcp.Server, cl mcpCaller) {
	mcpserve.AddTool(srv, cl, false, &mcp.Tool{
		Name:        "get_my_llm_usage",
		Description: "The caller's LLM projects (pools: tokens per 30 days, spend and budget, requests and tokens per minute, the models, which one new keys spend from by default) and their API keys for the OpenAI-compatible API: name, project, spend and budget, never the key itself. A new key is made on the LLM page of the self-service, not here.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, keys.Usage, error) {
		g, status, why := s.quota.GrantFor(ctx, cl.Tokens)
		if status != 0 {
			return nil, keys.Usage{}, msg.Err(why)
		}
		p, fail := s.keyPersonFor(ctx, cl.Caller)
		if fail != nil {
			return nil, keys.Usage{}, s.mcpFail(gatewayErrors, "LLM usage", fail)
		}
		u, err := s.keys.Usage(ctx, p, g)
		if err != nil {
			return nil, keys.Usage{}, s.mcpFail(gatewayErrors, "LLM usage", err)
		}
		s.nameWorkspaceKeys(ctx, u.Keys)
		return nil, u, nil
	})

	if s.aliases == nil {
		return
	}
	mcpserve.AddTool(srv, cl, false, &mcp.Tool{
		Name:        "list_llm_models",
		Description: "The model names (aliases such as chat-default, chat-fast, code, embedding; a group the caller's projects were granted on top only for them) the LLM API answers under, with the models currently behind each, how many deployments serve it and passed the last health check, and the longest prompt and answer every deployment takes (0 = unknown).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, ModelAliasesResponse, error) {
		grant, status, why := s.quota.GrantFor(ctx, cl.Tokens)
		if status != 0 && !cl.Role.AtLeast(access.RoleFleetAdmin) {
			return nil, ModelAliasesResponse{}, msg.Err(why)
		}
		var granted []string
		for _, p := range grant.Pools {
			granted = append(granted, p.Models...)
		}
		v, err := s.aliases.Get(ctx, granted)
		if err != nil {
			return nil, ModelAliasesResponse{}, s.mcpFail(gatewayErrors, "LLM models", err)
		}
		return nil, v, nil
	})
}

func (s *Server) registerWorkspaceTools(srv *mcp.Server, cl mcpCaller) {
	mcpserve.AddTool(srv, cl, false, &mcp.Tool{
		Name:        "list_my_projects",
		Description: "The caller's active workspace projects with their role in each, the allowed profiles and GPU classes, the limits per person, the budgets with what is used and reserved (an exhausted budget stops starts and jobs), the LLM tokens, storage and end. Projects come from the budget tree of the DHBW Cloud; nobody gets one here. Paged: limit and offset, total says how many there are.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in page.Input) (*mcp.CallToolResult, mcpProjectList, error) {
		mine, err := s.ws.ForPerson(ctx, cl.Tokens, cl.Email, false)
		if err != nil {
			return nil, mcpProjectList{}, s.mcpFail(internalErrors, "projects", err)
		}
		p := page.Of(mine, in.Request())
		out := mcpProjectList{Projects: []mcpProject{}, Total: p.Total}
		for _, m := range p.Items {
			out.Projects = append(out.Projects, toMCPProject(m))
		}
		return nil, out, nil
	})

	mcpserve.AddTool(srv, cl, false, &mcp.Tool{
		Name:        "get_my_workspace_usage",
		Description: "What the caller's own workspaces (servers, GPU kernels, batch jobs) reserved in every project, per day and per project, profile and GPU class: server hours, vCPU hours, RAM-GB hours, GPU hours, home GB-days, and the measured CPU and RAM use. Default period: the last 30 finished days. The budgets of list_my_projects count differently (their window includes today).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mcpPeriodInput) (*mcp.CallToolResult, usage.Report, error) {
		me := strings.ToLower(cl.Email)
		return s.mcpUsageReport(ctx, in, func(r usage.Row) bool { return strings.EqualFold(r.Person, me) })
	})

	mcpserve.AddTool(srv, cl, false, &mcp.Tool{
		Name:        "get_project_usage",
		Description: "What a whole project's workspaces reserved, per person, profile and GPU class: for the project's managers their project, for admins any project or, without one, everything. Default period: the last 30 finished days.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mcpProjectPeriodInput) (*mcp.CallToolResult, usage.Report, error) {
		if cl.admin() && in.Project == "" {
			return s.mcpUsageReport(ctx, in.mcpPeriodInput, nil)
		}
		if err := s.manager(ctx, cl.Caller, in.Project); err != nil {
			return nil, usage.Report{}, s.mcpFail(projectErrors, "workspace project", err)
		}
		return s.mcpUsageReport(ctx, in.mcpPeriodInput, func(r usage.Row) bool { return r.Project == in.Project })
	})
}

// mcpUsageReport is workspaceUsage without gin: the report over the period, the caller's defaults as the REST route's.
func (s *Server) mcpUsageReport(ctx context.Context, in mcpPeriodInput, keep usage.Filter) (*mcp.CallToolResult, usage.Report, error) {
	p, err := usage.Reports.Parse(in.From, in.To, time.Now())
	if err != nil {
		return nil, usage.Report{}, err
	}
	rep, err := usage.BuildReport(ctx, s.usage, p.From.AsUTC(), p.To.AsUTC(), keep)
	if err != nil {
		return nil, usage.Report{}, s.mcpFail(internalErrors, "workspace usage", err)
	}
	return nil, rep, nil
}

// mcpWorkspaceAccess is requireWorkspace for the tools: the caller's projects, or the refusal of workspaceAccess for somebody without one.
func (s *Server) mcpWorkspaceAccess(ctx context.Context, cl mcpCaller) ([]workspace.Mine, error) {
	mine := s.projectsOf(ctx, cl.Caller)
	if err := workspaceAccess(cl.Caller, func() []workspace.Mine { return mine }); err != nil {
		return nil, s.mcpFail(projectErrors, "workspace access", err)
	}
	return mine, nil
}

func (s *Server) registerServerTools(srv *mcp.Server, cl mcpCaller) {
	mcpserve.AddTool(srv, cl, false, &mcp.Tool{
		Name:        "list_my_servers",
		Description: "The caller's JupyterHub servers (workspaces): name, whether ready or still starting (pending), URL, profile, project, image, when started and last active. The default server has an empty name. Paged: limit and offset, total says how many there are.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in page.Input) (*mcp.CallToolResult, mcpServerList, error) {
		if _, err := s.mcpWorkspaceAccess(ctx, cl); err != nil {
			return nil, mcpServerList{}, err
		}
		servers, err := s.gpu.Servers(ctx, cl.Email)
		if err != nil {
			return nil, mcpServerList{}, s.mcpFail(gpuErrors, "servers", err)
		}
		p := page.Of(servers, in.Request())
		return nil, mcpServerList{Servers: p.Items, Total: p.Total}, nil
	})

	mcpserve.AddTool(srv, cl, false, &mcp.Tool{
		Name:        "get_server_progress",
		Description: "How far the start of one of the caller's servers got: JupyterHub's events (waiting for a GPU, pulling the image, …) and whether it ended ready or failed with the hub's reason. Ask again every few seconds while it starts.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mcpServerNameInput) (*mcp.CallToolResult, gpu.Progress, error) {
		if _, err := s.mcpWorkspaceAccess(ctx, cl); err != nil {
			return nil, gpu.Progress{}, err
		}
		name, ok := mcpServerName(in.Name)
		if !ok {
			return nil, gpu.Progress{}, msg.Errorf("Ungültiger Workspace-Name.", "Invalid workspace name.")
		}
		p, err := s.gpu.ServerProgress(ctx, cl.Email, name)
		if err != nil {
			return nil, gpu.Progress{}, s.mcpFail(gpuErrors, "server progress", err)
		}
		return nil, p, nil
	})

	mcpserve.AddTool(srv, cl, false, &mcp.Tool{
		Name:        "get_start_options",
		Description: "What the caller may start: their projects with the profiles and GPU classes a start may choose there (and whether GPUs are free right now), the profiles with their GPU use and times, and the LLM projects a server's or job's key may draw from. The ids here are what start_server and submit_job take.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, mcpStartOptions, error) {
		mine, err := s.mcpWorkspaceAccess(ctx, cl)
		if err != nil {
			return nil, mcpStartOptions{}, err
		}
		return nil, toMCPStartOptions(s.startOptionsOf(ctx, mine, cl.Tokens)), nil
	})

	mcpserve.AddTool(srv, cl, true, &mcp.Tool{
		Name:        "start_server",
		Description: "Start a workspace server in a project with a profile (get_start_options). The same checks as a start in JupyterHub: membership, profile and GPU class allowed, the project's limits per person, the budgets, free GPUs; a refusal says why. The server starts in the background; get_server_progress and list_my_servers show when it is ready. A running server counts against the project's budgets, so the person should want it.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mcpStartServerInput) (*mcp.CallToolResult, gpu.Server, error) {
		mine, err := s.mcpWorkspaceAccess(ctx, cl)
		if err != nil {
			return nil, gpu.Server{}, err
		}
		if in.Project == "" || in.Profile == "" {
			return nil, gpu.Server{}, msg.Errorf("project und profile fehlen.", "project and profile are missing.")
		}
		// The input is the REST request field for field (mcp_input_drift_test.go holds them together), so it converts.
		o, why := s.startOptionsFor(ctx, mine, cl.Tokens, StartServerRequest(in))
		if !why.IsZero() {
			return nil, gpu.Server{}, msg.Err(why)
		}
		s.log.Infow("MCP start_server", "actor", cl.actorEmail(), "user", cl.Email, "project", in.Project, "profile", in.Profile)
		srv, err := s.gpu.StartServer(ctx, cl.Email, o)
		if err != nil {
			return nil, gpu.Server{}, s.mcpFail(gpuErrors, "start server", err)
		}
		return nil, srv, nil
	})

	mcpserve.AddTool(srv, cl, true, &mcp.Tool{
		Name:        "stop_server",
		Description: "Stop one of the caller's servers (list_my_servers). A named server is removed after stopping, the home stays; unsaved work in the server's memory is lost, so ask the person before stopping a server they may be using.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mcpServerNameInput) (*mcp.CallToolResult, mcpDone, error) {
		if _, err := s.mcpWorkspaceAccess(ctx, cl); err != nil {
			return nil, mcpDone{}, err
		}
		name, ok := mcpServerName(in.Name)
		if !ok {
			return nil, mcpDone{}, msg.Errorf("Ungültiger Workspace-Name.", "Invalid workspace name.")
		}
		s.log.Infow("MCP stop_server", "actor", cl.actorEmail(), "user", cl.Email, "server", name)
		if err := s.gpu.StopServer(ctx, cl.Email, name); err != nil {
			return nil, mcpDone{}, s.mcpFail(gpuErrors, "stop server", err)
		}
		return nil, mcpDone{Done: true, Message: msg.T("Der Server wird beendet.", "The server is being stopped.").In(msg.LangOf(ctx))}, nil
	})
}

// mcpServerName is the server's name for the GPU part: empty or "-" is the default server, as in the REST path; anything else must be a name a start generates.
func mcpServerName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" || name == "-" {
		return "", true
	}
	return name, serverNameRE.MatchString(name)
}

func (s *Server) registerJobTools(srv *mcp.Server, cl mcpCaller) {
	a := jobActorOf(cl.Caller)

	mcpserve.AddTool(srv, cl, false, &mcp.Tool{
		Name:        "list_my_jobs",
		Description: "The caller's batch jobs, newest first, a waiting one with its place in its queue; with project the caller's jobs there, or all of the project for its managers and admins. A job's state is queued, running, succeeded, failed or cancelled; result is what it reported in its last RESULT line. Paged: limit and offset, total says how many there are.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mcpJobListInput) (*mcp.CallToolResult, mcpJobList, error) {
		jobs, err := listJobsFor(ctx, s.ws, s.gpu, a, in.Project)
		if err != nil {
			return nil, mcpJobList{}, s.mcpFail(jobErrors, "list", err)
		}
		p := page.Of(jobs, in.Request())
		return nil, mcpJobList{Jobs: p.Items, Total: p.Total}, nil
	})

	mcpserve.AddTool(srv, cl, false, &mcp.Tool{
		Name:        "get_job",
		Description: "One batch job, read live while it waits or runs: state, evictions, GPU hours, exit code and end. The caller's own jobs, and those of projects they manage.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mcpJobInput) (*mcp.CallToolResult, gpu.Job, error) {
		j, err := visibleJob(ctx, s.ws, s.gpu, a, in.ID)
		if err != nil {
			return nil, gpu.Job{}, s.mcpFail(jobErrors, "get", err)
		}
		return nil, j, nil
	})

	mcpserve.AddTool(srv, cl, false, &mcp.Tool{
		Name:        "get_job_log",
		Description: "The end of a batch job's output: live while it runs, afterwards the last 64 KB kept for 30 days. While it waits, a line says what for. The whole output of every attempt is in ~/jobs/<name>/ (list_job_files).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mcpJobLogInput) (*mcp.CallToolResult, JobLogResponse, error) {
		j, err := visibleJob(ctx, s.ws, s.gpu, a, in.ID)
		if err != nil {
			return nil, JobLogResponse{}, s.mcpFail(jobErrors, "log", err)
		}
		lines := in.Lines
		if lines == 0 {
			lines = 200
		}
		log, err := s.gpu.JobLog(ctx, j.ID, min(max(lines, 1), 5000))
		if err != nil {
			return nil, JobLogResponse{}, s.mcpFail(jobErrors, "log", err)
		}
		return nil, JobLogResponse{Log: log}, nil
	})

	mcpserve.AddTool(srv, cl, false, &mcp.Tool{
		Name:        "list_job_files",
		Description: "The files of a folder in the home a batch job ran in, also after it ended: by default the job's folder jobs/<name> with its output and checkpoints. The job's person sees their whole home (the project's shared folder at project-shared) while a member of the project; managers and admins only the job's folder. The first call may take a few seconds. Downloads are made with the CLI dhbw-cloud, not here.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mcpJobFilesInput) (*mcp.CallToolResult, gpu.JobFiles, error) {
		j, err := visibleJob(ctx, s.ws, s.gpu, a, in.ID)
		if err != nil {
			return nil, gpu.JobFiles{}, s.mcpFail(jobErrors, "files", err)
		}
		root := jobFilesRoot(ctx, s.ws, a, j)
		p := in.Path
		if p == "" {
			p = "jobs/" + j.Name
		}
		rel, ok := gpu.JobFilePath(p, root)
		if !ok {
			return nil, gpu.JobFiles{}, msg.Err(onlyPathsIn(root))
		}
		list, err := s.gpu.ListJobFiles(ctx, j, rel)
		if errors.Is(err, gpu.ErrNotFound) {
			return nil, gpu.JobFiles{}, msg.Errorf("Ordner nicht gefunden.", "Folder not found.")
		}
		if err != nil {
			return nil, gpu.JobFiles{}, s.mcpFail(jobErrors, "files", err)
		}
		return nil, list, nil
	})

	mcpserve.AddTool(srv, cl, true, &mcp.Tool{
		Name:        "submit_job",
		Description: "Submit a batch job: a command or a notebook in a project's environment, with the person's home and the project's shared folder, on a GPU of the class. The same checks as over the API: membership, batch jobs and GPU class allowed, jobs per person, maximum run time, and the budget (used plus what running and waiting jobs reserve plus this job's GPUs × max_hours must fit); a refusal says why. The job reserves GPU hours of the project's budget, so the person should want it. Output goes to ~/jobs/<name>/; get_job and get_job_log follow it.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mcpSubmitJobInput) (*mcp.CallToolResult, gpu.Job, error) {
		if _, err := s.mcpWorkspaceAccess(ctx, cl); err != nil {
			return nil, gpu.Job{}, err
		}
		if in.Project == "" {
			return nil, gpu.Job{}, msg.Errorf("project fehlt.", "project is missing.")
		}
		s.log.Infow("MCP submit_job", "actor", cl.actorEmail(), "user", cl.Email, "project", in.Project)
		// The input is the REST request field for field (mcp_input_drift_test.go holds them together), so it converts.
		j, err := submitJob(ctx, s.ws, s.gpu, s.jobKeys(), a, SubmitJobRequest(in))
		if err != nil {
			return nil, gpu.Job{}, s.mcpFail(jobErrors, "submit", err)
		}
		return nil, j, nil
	})

	mcpserve.AddTool(srv, cl, true, &mcp.Tool{
		Name:        "cancel_job",
		Description: "Cancel a waiting or running batch job; what it used stays booked, its output so far stays in ~/jobs/<name>/. The caller's own jobs, and those of projects they manage. An ended job is returned as it is.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mcpJobInput) (*mcp.CallToolResult, gpu.Job, error) {
		j, err := visibleJob(ctx, s.ws, s.gpu, a, in.ID)
		if err != nil {
			return nil, gpu.Job{}, s.mcpFail(jobErrors, "cancel", err)
		}
		s.log.Infow("MCP cancel_job", "actor", cl.actorEmail(), "user", cl.Email, "job", j.ID)
		j, err = s.gpu.CancelJob(ctx, j.ID, a.person)
		if err != nil {
			return nil, gpu.Job{}, s.mcpFail(jobErrors, "cancel", err)
		}
		return nil, j, nil
	})
}

// actorEmail is who really calls, for the log: the admin acting on behalf, else the person.
func (c mcpCaller) actorEmail() string {
	if c.Actor != "" {
		return c.Actor
	}
	return c.Email
}
