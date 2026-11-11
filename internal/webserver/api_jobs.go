package webserver

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/cloud-self-service-golib/page"
	"github.com/pfisterer/llm-management-api/internal/access"
	"github.com/pfisterer/llm-management-api/internal/cluster/gpu"
	"github.com/pfisterer/llm-management-api/internal/cluster/workspace"
	"github.com/pfisterer/llm-management-api/internal/llm/keys"
)

// Batch jobs (docs/architektur.md "Batch-Jobs"): GPU jobs without a workspace, in the project's volume and environment. People see and cancel their own jobs, the project's managers and admins all of the project.

// SubmitJobRequest is a job to run: in a project (and GPU class, optional when it has one), a shell command or a notebook (papermill), relative to the home.
type SubmitJobRequest struct {
	Project string `json:"project" binding:"required" example:"t-7841d99e-68a2-47b7-88a5-1ab55180f4f5"`
	// Name names the job and its output folder ~/jobs/<name>; empty = its id.
	Name     string `json:"name" example:"training-1"`
	GPUClass string `json:"gpu_class" example:"general-large"`
	// Command runs in a shell (bash where the image has it) in WorkingDir; Notebook is a path in the home run with papermill, the executed copy lands next to it as <name>.<job name>.ipynb. Exactly one of them.
	Command    string `json:"command" example:"python train.py --epochs 10"`
	Notebook   string `json:"notebook" example:"projects/kurs/training.ipynb"`
	WorkingDir string `json:"working_dir" example:"projects/kurs"`
	// MaxHours is how long the job may run at the latest, which it reserves of the project's GPU hours; 0 = the project's maximum (job_max_hours).
	MaxHours float64 `json:"max_hours" example:"2"`
	// LLMProject is whose LLM tokens the job's key (OPENAI_API_KEY with OPENAI_BASE_URL) draws from, as at a workspace's start: one of the caller's projects with LLM tokens, "none" = without key, empty = as for the project's workspaces (its default_llm_project, else the project itself when it has LLM tokens, else the caller's first LLM project; none when that cannot be had).
	LLMProject string `json:"llm_project" example:"none"`
	// Preemptible lets a job yield its GPU to workspaces although the project's budget would run it guaranteed; it then counts like any preemptible job.
	Preemptible bool `json:"preemptible"`
}

// JobLogResponse is the end of a job's output: live from its pod, or what was kept when it ended.
type JobLogResponse struct {
	Log string `json:"log"`
}

// jobActor is who acts on jobs: the person (hub user name), their Keycloak sub when known (signed in as themself), their tokens for the project roles, and whether they are an admin of the service.
type jobActor struct {
	person string
	sub    string
	tokens []string
	admin  bool
}

// jobKeys makes a job's LLM key (the person's workspace key, keySource) and says where the LLM API is; nil = jobs without key.
type jobKeys struct {
	keySource
	baseURL string
	// gateway and limits: which models the key may use and their limits, for Claude Code's variables (agentEnv); empty gateway = none.
	gateway string
	limits  interface {
		Limits(ctx context.Context) (map[string]ModelLimit, error)
	}
}

// agentEnv are Claude Code's variables for a key, as the hub sets them in a workspace (dhbw_agent_env in the hub's values): the gateway's root as ANTHROPIC_BASE_URL, the agent model ("agent" where the key may use it, else "code", else the first) in every slot, chat-fast as the small one, and the model's limits; without the key itself (ANTHROPIC_AUTH_TOKEN, from the Secret). nil without models.
func agentEnv(baseURL string, models []string, limits map[string]ModelLimit) map[string]string {
	base := strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/v1")
	agent := ""
	switch {
	case slices.Contains(models, "agent"):
		agent = "agent"
	case slices.Contains(models, DefaultWorkspaceModel):
		agent = DefaultWorkspaceModel
	case len(models) > 0:
		agent = models[0]
	}
	if base == "" || agent == "" {
		return nil
	}
	small := agent
	if slices.Contains(models, "chat-fast") {
		small = "chat-fast"
	}
	maxIn, maxOut := 24576, 8192
	if l, ok := limits[agent]; ok {
		if l.MaxInputTokens > 0 {
			maxIn = l.MaxInputTokens
		}
		if l.MaxOutputTokens > 0 {
			maxOut = l.MaxOutputTokens
		}
	}
	return map[string]string{"ANTHROPIC_BASE_URL": base, "ANTHROPIC_MODEL": agent, "ANTHROPIC_DEFAULT_OPUS_MODEL": agent, "ANTHROPIC_DEFAULT_SONNET_MODEL": agent, "ANTHROPIC_DEFAULT_HAIKU_MODEL": small,
		"CLAUDE_CODE_SUBAGENT_MODEL": agent, "CLAUDE_CODE_MAX_CONTEXT_TOKENS": strconv.Itoa(maxIn + maxOut), "CLAUDE_CODE_MAX_OUTPUT_TOKENS": strconv.Itoa(maxOut), "CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS": "1"}
}

// keyReasons are the reasons of workspaceKey for no key, for people.
var keyReasons = map[string]string{
	"the chosen LLM project is none of the person's projects with LLM tokens": "Das gewählte LLM-Projekt ist keines der eigenen Projekte mit LLM-Tokens.",
	"person unknown to the LLM service":                                       "Für LLM-Schlüssel muss die Person den KI-Dienst einmal selbst genutzt haben (anmelden und die LLM-Seite öffnen).",
	"project has no LLM tokens":                                               "Das gewählte Projekt hat keine LLM-Tokens.",
}

// submitJob checks the job against the project (membership, batch jobs, class, limits, budget with reservations) and submits it with the project's environment as a start sees it. Check and submit hold the project's job lock: requests sent together cannot all pass the check before any of them is saved.
func submitJob(ctx context.Context, ws *workspace.Service, g *gpu.Service, k *jobKeys, a jobActor, req SubmitJobRequest) (gpu.Job, error) {
	var job gpu.Job
	err := g.LockProjectJobs(ctx, req.Project, func(ctx context.Context) error {
		var err error
		job, err = checkAndSubmitJob(ctx, ws, g, k, a, req)
		return err
	})
	return job, err
}

func checkAndSubmitJob(ctx context.Context, ws *workspace.Service, g *gpu.Service, k *jobKeys, a jobActor, req SubmitJobRequest) (gpu.Job, error) {
	live, err := g.Jobs(ctx, gpu.JobFilter{Project: req.Project, Person: a.person, Live: true})
	if err != nil {
		return gpu.Job{}, err
	}
	grant, err := ws.CheckJob(ctx, a.tokens, workspace.JobRequest{Person: a.person, Project: req.Project, GPUClass: req.GPUClass, GPUs: 1, MaxHours: req.MaxHours, Jobs: len(live)})
	if err != nil {
		return gpu.Job{}, err
	}
	mine, err := ws.ForPerson(ctx, a.tokens, a.person, false)
	if err != nil {
		return gpu.Job{}, err
	}
	var hp *HubProject
	for _, p := range hubProjects(ctx, ws, mine) {
		if p.Slug == req.Project {
			hp = &p
		}
	}
	if hp == nil {
		return gpu.Job{}, &workspace.Refusal{Reason: "Kein aktives Projekt " + req.Project + " mit Workspace-Zugang."}
	}
	spec := gpu.JobSpec{Name: req.Name, Person: a.person, Project: req.Project, Class: grant.GPUClass, GPUs: 1, MaxHours: grant.MaxHours, Guaranteed: grant.Guaranteed && !req.Preemptible,
		Image: hp.ProjectImage, Archs: hp.ImageArchs, PullSecret: hp.PullSecret, SharedReadOnly: hp.SharedReadOnly, ScratchGB: hp.ScratchGB, StorageGB: hp.StorageGB,
		Command: req.Command, Notebook: req.Notebook, WorkingDir: req.WorkingDir}
	if err := jobLLMKey(ctx, ws, k, a, req, &spec); err != nil {
		return gpu.Job{}, err
	}
	return g.SubmitJob(ctx, spec)
}

// jobLLMKey gives the job the person's LLM key by the rule of their workspaces in the project (keySource.workspaceKey, llmProjectFor). A chosen LLM project that yields no key refuses the job; left to the project, the job runs without key when there is none or it cannot be had right now (LiteLLM not reachable), as a workspace starts without.
func jobLLMKey(ctx context.Context, ws *workspace.Service, k *jobKeys, a jobActor, req SubmitJobRequest, spec *gpu.JobSpec) error {
	chosen := strings.TrimSpace(req.LLMProject)
	if chosen == gpu.NoLLM {
		return nil
	}
	if k == nil {
		if chosen != "" {
			return &workspace.Refusal{Reason: "LLM-Schlüssel für Jobs sind in diesem Dienst nicht eingerichtet."}
		}
		return nil
	}
	proj, err := ws.Get(ctx, req.Project)
	if err != nil {
		return err
	}
	got, fail := k.workspaceKey(ctx, a.person, a.sub, a.tokens, proj, chosen)
	switch {
	case fail != nil && chosen != "":
		// A key that could not be found out for a job whose LLM project was chosen: an error status with the key's text (classify).
		return fail
	case fail != nil:
		return nil
	case got.Key == "" && chosen != "":
		reason := keyReasons[got.Reason]
		if reason == "" {
			reason = "Kein LLM-Schlüssel aus " + chosen + "."
		}
		return &workspace.Refusal{Reason: reason}
	case got.Key == "":
		return nil
	}
	spec.LLMKey, spec.LLMProject, spec.LLMBaseURL = got.Key, got.LLMProject, k.baseURL
	// Claude Code's variables need the key's models; without them (gateway not reachable) the job keeps OPENAI_*.
	if k.gateway != "" {
		if models, err := keys.ModelsOf(ctx, k.gateway, got.Key); err == nil {
			var limits map[string]ModelLimit
			if k.limits != nil {
				limits, _ = k.limits.Limits(ctx)
			}
			spec.LLMAgentEnv = agentEnv(k.baseURL, models, limits)
		}
	}
	return nil
}

// mayManage says whether the actor may see and cancel all jobs of the project: an admin or one of its managers (workspace.Service.MayManage); an unknown project or one that is not theirs is no.
func mayManage(ctx context.Context, ws *workspace.Service, a jobActor, project string) bool {
	ok, _ := ws.MayManage(ctx, a.tokens, a.admin, project)
	return ok
}

// visibleJob is the job when the actor may see it: their own, or one of a project they manage.
func visibleJob(ctx context.Context, ws *workspace.Service, g *gpu.Service, a jobActor, id string) (gpu.Job, error) {
	j, err := g.GetJob(ctx, id)
	if err != nil {
		return gpu.Job{}, err
	}
	if !strings.EqualFold(j.Person, a.person) && !mayManage(ctx, ws, a, j.Project) {
		return gpu.Job{}, gpu.ErrNotFound
	}
	return j, nil
}

func (s *Server) jobActor(c *gin.Context) jobActor {
	return jobActorOf(caller(c))
}

// jobActorOf is the caller as the jobs see them, for the REST handlers and the MCP tools alike; the sub only when they act as themself.
func jobActorOf(cl Caller) jobActor {
	a := jobActor{person: cl.Email, tokens: cl.Tokens, admin: cl.Role.AtLeast(access.RoleAdmin)}
	if cl.Actor == "" {
		a.sub = cl.Subject
	}
	return a
}

// listJobsFor are the jobs the actor sees: without project their own, with project their own there, or all of the project when they manage it.
func listJobsFor(ctx context.Context, ws *workspace.Service, g *gpu.Service, a jobActor, project string) ([]gpu.Job, error) {
	f := gpu.JobFilter{Project: project, Person: a.person}
	if f.Project != "" && mayManage(ctx, ws, a, f.Project) {
		f.Person = ""
	}
	return g.Jobs(ctx, f)
}

// jobKeys is how the server makes jobs' LLM keys; nil without keys or master key.
func (s *Server) jobKeys() *jobKeys {
	if s.keys == nil || s.master == "" {
		return nil
	}
	k := &jobKeys{keySource: keySource{Keys: s.keys, Quota: s.quota, MasterKey: s.master}, baseURL: s.apiURL, gateway: s.gateway}
	if s.aliases != nil {
		k.limits = s.aliases
	}
	return k
}

// listJobs godoc
//
//	@ID				listGPUJobs
//	@Summary		My batch jobs, or a project's
//	@Description	Newest first, a waiting one with its place in its queue. Without project the caller's own jobs; with project the caller's jobs there, or all of the project for its managers and admins.
//	@Tags			gpu
//	@Produce		json
//	@Security		BearerAuth
//	@Param			project	query		string	false	"project slug"
//	@Param			limit	query		int		false	"page size; default 100, at most 500"
//	@Param			offset	query		int		false	"how many entries to skip"
//	@Success		200		{object}	Page{items=[]gpu.Job}
//	@Failure		400		{object}	ErrorResponse
//	@Failure		502		{object}	ErrorResponse
//	@Router			/v1/gpu/jobs [get]
func (s *Server) listJobs(c *gin.Context) {
	r, ok := pageRequest(c)
	if !ok {
		return
	}
	jobs, err := listJobsFor(c.Request.Context(), s.ws, s.gpu, s.jobActor(c), c.Query("project"))
	if err != nil {
		s.fail(c, jobErrors, "list", err)
		return
	}
	c.JSON(http.StatusOK, page.Of(jobs, r))
}

// submitJob godoc
//
//	@ID				submitGPUJob
//	@Summary		Submit a batch job
//	@Description	Checks the job against the project (membership, batch jobs and GPU class allowed, jobs per person, maximum run time, budget: used plus what running and waiting jobs reserve plus GPUs × max_hours of this one must fit), then queues it in the project's queue. A refusal is 409 with its reason; an unknown project and one the caller is no member of are refused alike. Callers without any project get 403. The job runs in the project's environment with the person's home and the shared folder; its output also goes to ~/jobs/<name>/output-<attempt>.log. A preemptible job (the project's batch_priority) is evicted for a workspace and starts again from the beginning; max_hours counts over all its attempts. Before an eviction, its deadline or a cancel the command gets TERM and DHBW_JOB_GRACE_SECONDS (default 120) to save a checkpoint into DHBW_JOB_DIR; DHBW_JOB_ATTEMPT counts its starts. The job gets the person's LLM key (OPENAI_API_KEY, OPENAI_BASE_URL, and Claude Code's ANTHROPIC_* as in a workspace) by the rule of their workspaces in the project, llm_project chooses another LLM project or none; a chosen one without key is refused with 409.
//	@Tags			gpu
//	@Accept			json
//	@Produce		json
//	@Security		BearerAuth
//	@Param			job	body		SubmitJobRequest	true	"what to run"
//	@Success		201	{object}	gpu.Job
//	@Failure		400	{object}	ErrorResponse
//	@Failure		409	{object}	ErrorResponse
//	@Router			/v1/gpu/jobs [post]
func (s *Server) submitJob(c *gin.Context) {
	var req SubmitJobRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abort(c, http.StatusBadRequest, "invalid", "project fehlt.")
		return
	}
	j, err := submitJob(c.Request.Context(), s.ws, s.gpu, s.jobKeys(), s.jobActor(c), req)
	if err != nil {
		s.fail(c, jobErrors, "submit", err)
		return
	}
	c.JSON(http.StatusCreated, j)
}

// getJob godoc
//
//	@ID				getGPUJob
//	@Summary		One batch job
//	@Description	A waiting or running job is read live from Kubernetes and Kueue (state, evictions, GPU hours, end), the list only once a minute.
//	@Tags			gpu
//	@Produce		json
//	@Security		BearerAuth
//	@Param			id	path		string	true	"job id"
//	@Success		200	{object}	gpu.Job
//	@Failure		404	{object}	ErrorResponse
//	@Router			/v1/gpu/jobs/{id} [get]
func (s *Server) getJob(c *gin.Context) {
	j, err := visibleJob(c.Request.Context(), s.ws, s.gpu, s.jobActor(c), c.Param("id"))
	if err != nil {
		s.fail(c, jobErrors, "get", err)
		return
	}
	c.JSON(http.StatusOK, j)
}

// getJobLog godoc
//
//	@ID				getGPUJobLog
//	@Summary		The end of a batch job's output
//	@Description	Live from its pod while it runs and until Kubernetes deletes the finished job (a day), then the last 64 KB kept for 30 days. While a job waits (for a GPU, after an eviction, or for its next pod to start) the end of its last attempt and a line "== <time> wartet …" or "== <time> Versuch <n> wird gestartet". The whole output of every attempt stays in ~/jobs/<name>/.
//	@Tags			gpu
//	@Produce		json
//	@Security		BearerAuth
//	@Param			id		path		string	true	"job id"
//	@Param			lines	query		int		false	"lines (live only), default 200, at most 5000"
//	@Success		200		{object}	JobLogResponse
//	@Failure		404		{object}	ErrorResponse
//	@Router			/v1/gpu/jobs/{id}/log [get]
func (s *Server) getJobLog(c *gin.Context) {
	ctx := c.Request.Context()
	j, err := visibleJob(ctx, s.ws, s.gpu, s.jobActor(c), c.Param("id"))
	if err != nil {
		s.fail(c, jobErrors, "log", err)
		return
	}
	lines, _ := strconv.Atoi(c.DefaultQuery("lines", "200"))
	log, err := s.gpu.JobLog(ctx, j.ID, min(max(lines, 1), 5000))
	if err != nil {
		s.fail(c, jobErrors, "log", err)
		return
	}
	c.JSON(http.StatusOK, JobLogResponse{Log: log})
}

// cancelJob godoc
//
//	@ID				cancelGPUJob
//	@Summary		Cancel a batch job
//	@Description	Ends a waiting or running job; what it used stays booked. The person's own jobs (also after they left the project), all of the project for its managers and admins. An ended job is returned as it is.
//	@Tags			gpu
//	@Produce		json
//	@Security		BearerAuth
//	@Param			id	path		string	true	"job id"
//	@Success		200	{object}	gpu.Job
//	@Failure		404	{object}	ErrorResponse
//	@Router			/v1/gpu/jobs/{id} [delete]
func (s *Server) cancelJob(c *gin.Context) {
	ctx := c.Request.Context()
	a := s.jobActor(c)
	j, err := visibleJob(ctx, s.ws, s.gpu, a, c.Param("id"))
	if err != nil {
		s.fail(c, jobErrors, "cancel", err)
		return
	}
	j, err = s.gpu.CancelJob(ctx, j.ID, a.person)
	if err != nil {
		s.fail(c, jobErrors, "cancel", err)
		return
	}
	c.JSON(http.StatusOK, j)
}

// jobFilesRoot is the part of the home of the job's person that the actor may read, relative to the home: the whole home for that person while a member of the job's project (their own files, as in a workspace), else only the job's folder jobs/<name> (its output and checkpoints) — for the project's managers and admins, and for the person after leaving the project.
func jobFilesRoot(ctx context.Context, ws *workspace.Service, a jobActor, j gpu.Job) string {
	if strings.EqualFold(j.Person, a.person) {
		if p, err := ws.Get(ctx, j.Project); err == nil && p.RoleOf(a.tokens) != "" {
			return ""
		}
	}
	return "jobs/" + j.Name
}

// jobFile is the visible job and the checked path of a file request: in the readable part of the home (jobFilesRoot), the job's folder when path is empty.
func (s *Server) jobFile(c *gin.Context) (gpu.Job, string, bool) {
	ctx := c.Request.Context()
	a := s.jobActor(c)
	j, err := visibleJob(ctx, s.ws, s.gpu, a, c.Param("id"))
	if err != nil {
		s.fail(c, jobErrors, "files", err)
		return gpu.Job{}, "", false
	}
	root := jobFilesRoot(ctx, s.ws, a, j)
	p := c.Query("path")
	if p == "" {
		p = "jobs/" + j.Name
	}
	rel, ok := gpu.JobFilePath(p, root)
	if !ok {
		where := "im Home"
		if root != "" {
			where = "im Ordner des Jobs (~/" + root + ")"
		}
		abort(c, http.StatusBadRequest, "invalid", "Nur Pfade "+where+".")
		return gpu.Job{}, "", false
	}
	return j, rel, true
}

// listJobFiles godoc
//
//	@ID				listGPUJobFiles
//	@Summary		Files of a batch job
//	@Description	A folder of the home the job ran in (the person's home in the project's volume), read by a short-lived pod, also when the job has ended and without a workspace. path is relative to the home; without it the job's folder ~/jobs/<name> (output, checkpoints). The job's person reads their whole home, with the project's shared folder at project-shared, while a member of the project; the project's managers and admins, and the person after leaving it, only the job's folder. Symbolic links are listed, not followed. The first request may take a few seconds.
//	@Tags			gpu
//	@Produce		json
//	@Security		BearerAuth
//	@Param			id		path		string	true	"job id"
//	@Param			path	query		string	false	"folder relative to the home, default jobs/<name>"
//	@Success		200		{object}	gpu.JobFiles
//	@Failure		400		{object}	ErrorResponse
//	@Failure		404		{object}	ErrorResponse
//	@Router			/v1/gpu/jobs/{id}/files [get]
func (s *Server) listJobFiles(c *gin.Context) {
	j, rel, ok := s.jobFile(c)
	if !ok {
		return
	}
	list, err := s.gpu.ListJobFiles(c.Request.Context(), j, rel)
	if errors.Is(err, gpu.ErrNotFound) {
		abort(c, http.StatusNotFound, "not_found", "Ordner nicht gefunden.")
		return
	}
	if err != nil {
		s.fail(c, jobErrors, "files", err)
		return
	}
	c.JSON(http.StatusOK, list)
}

// downloadJobFile godoc
//
//	@ID				downloadGPUJobFile
//	@Summary		Download a file of a batch job
//	@Description	A file of the home the job ran in, streamed; the same rights and paths as the listing (listGPUJobFiles). At most 5 GB and 30 minutes; a symbolic link must point into the readable part.
//	@Tags			gpu
//	@Produce		octet-stream
//	@Security		BearerAuth
//	@Param			id		path		string	true	"job id"
//	@Param			path	query		string	true	"file relative to the home"
//	@Success		200		{file}		file
//	@Failure		400		{object}	ErrorResponse
//	@Failure		404		{object}	ErrorResponse
//	@Router			/v1/gpu/jobs/{id}/files/download [get]
func (s *Server) downloadJobFile(c *gin.Context) {
	if c.Query("path") == "" {
		abort(c, http.StatusBadRequest, "invalid", "path fehlt.")
		return
	}
	j, rel, ok := s.jobFile(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), gpu.JobFileTimeout)
	defer cancel()
	body, size, err := s.gpu.OpenJobFile(ctx, j, rel)
	if errors.Is(err, gpu.ErrNotFound) {
		abort(c, http.StatusNotFound, "not_found", "Datei nicht gefunden.")
		return
	}
	if err != nil {
		s.fail(c, jobErrors, "file", err)
		return
	}
	defer body.Close()
	name := path.Base(rel)
	c.Header("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(name))
	c.Header("X-Content-Type-Options", "nosniff")
	c.DataFromReader(http.StatusOK, size, "application/octet-stream", body, nil)
}

// uploadJobFile godoc
//
//	@ID				uploadGPUFile
//	@Summary		Upload a file into the own home of a project
//	@Description	For batch jobs without a workspace (dhbw-cloud job submit --files, dhbw-cloud job upload): the body is written as path into the caller's home in the project, where the caller's jobs run, or under project-shared/ into the project's shared folder (not for the role reader, as in a workspace). Missing folders are made, an existing file is replaced once the new one is complete. Only the caller's own home and the shared folder, in an active project with workspace access; at most 5 GB and 30 minutes, Content-Length required. The first request may take a few seconds.
//	@Tags			gpu
//	@Accept			octet-stream
//	@Produce		json
//	@Security		BearerAuth
//	@Param			project	query		string	true	"project slug"
//	@Param			path	query		string	true	"file relative to the home, e.g. jobs/training-1/train.py"
//	@Param			body	body		string	true	"the file's content"	Format(binary)
//	@Success		201		{object}	gpu.JobFile
//	@Failure		400		{object}	ErrorResponse
//	@Failure		409		{object}	ErrorResponse
//	@Failure		411		{object}	ErrorResponse
//	@Failure		413		{object}	ErrorResponse
//	@Router			/v1/gpu/files [put]
func (s *Server) uploadJobFile(c *gin.Context) {
	rel, ok := gpu.JobFilePath(c.Query("path"), "")
	switch {
	case !ok || rel == "":
		abort(c, http.StatusBadRequest, "invalid", "Pfad einer Datei im Home angeben.")
		return
	case c.Request.ContentLength < 0:
		abort(c, http.StatusLengthRequired, "length_required", "Content-Length fehlt.")
		return
	case c.Request.ContentLength > gpu.JobFileMaxBytes:
		abort(c, http.StatusRequestEntityTooLarge, "too_large", "Höchstens 5 GB je Datei.")
		return
	}
	ctx := c.Request.Context()
	a := s.jobActor(c)
	project := c.Query("project")
	mine, err := s.ws.ForPerson(ctx, a.tokens, a.person, false)
	if err != nil {
		s.fail(c, jobErrors, "upload", err)
		return
	}
	hps := hubProjects(ctx, s.ws, mine)
	i := slices.IndexFunc(hps, func(p HubProject) bool { return p.Slug == project })
	if i < 0 {
		s.fail(c, jobErrors, "upload", &workspace.Refusal{Reason: "Kein aktives Projekt " + project + " mit Workspace-Zugang."})
		return
	}
	sharedRO := hps[i].SharedReadOnly
	ctx, cancel := context.WithTimeout(ctx, gpu.JobFileTimeout)
	defer cancel()
	f, err := s.gpu.PutJobFile(ctx, project, a.person, rel, sharedRO, c.Request.Body, c.Request.ContentLength)
	if errors.Is(err, gpu.ErrNotFound) {
		// The files pod refuses a path that leaves the home through a symbolic link.
		abort(c, http.StatusBadRequest, "invalid", "Pfad einer Datei im Home angeben.")
		return
	}
	if err != nil {
		s.fail(c, jobErrors, "upload", err)
		return
	}
	c.JSON(http.StatusCreated, f)
}
