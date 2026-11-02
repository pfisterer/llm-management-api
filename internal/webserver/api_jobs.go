package webserver

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/llm-management-api/internal/access"
	"github.com/pfisterer/llm-management-api/internal/gpu"
	"github.com/pfisterer/llm-management-api/internal/workspace"
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
}

// JobLogResponse is the end of a job's output: live from its pod, or what was kept when it ended.
type JobLogResponse struct {
	Log string `json:"log"`
}

// jobActor is who acts on jobs: the person (hub user name), their tokens for the project roles, and whether they are an admin of the service.
type jobActor struct {
	person string
	tokens []string
	admin  bool
}

// submitJob checks the job against the project (membership, batch jobs, class, limits, budget with reservations) and submits it with the project's environment as a start sees it. Check and submit hold the project's job lock: requests sent together cannot all pass the check before any of them is saved.
func submitJob(ctx context.Context, ws *workspace.Service, g *gpu.Service, a jobActor, req SubmitJobRequest) (gpu.Job, error) {
	var job gpu.Job
	err := g.LockProjectJobs(ctx, req.Project, func(ctx context.Context) error {
		var err error
		job, err = checkAndSubmitJob(ctx, ws, g, a, req)
		return err
	})
	return job, err
}

func checkAndSubmitJob(ctx context.Context, ws *workspace.Service, g *gpu.Service, a jobActor, req SubmitJobRequest) (gpu.Job, error) {
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
	return g.SubmitJob(ctx, gpu.JobSpec{Name: req.Name, Person: a.person, Project: req.Project, Class: grant.GPUClass, GPUs: 1, MaxHours: grant.MaxHours, Guaranteed: grant.Guaranteed,
		Image: hp.ProjectImage, Archs: hp.ImageArchs, PullSecret: hp.PullSecret, SharedReadOnly: hp.SharedReadOnly, ScratchGB: hp.ScratchGB, StorageGB: hp.StorageGB,
		Command: req.Command, Notebook: req.Notebook, WorkingDir: req.WorkingDir})
}

// mayManage says whether the actor may see and cancel all jobs of the project: an admin or one of its managers.
func mayManage(ctx context.Context, ws *workspace.Service, a jobActor, project string) bool {
	if a.admin {
		return true
	}
	p, err := ws.Get(ctx, project)
	return err == nil && p.RoleOf(a.tokens) == workspace.RoleManager
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
	cl := caller(c)
	return jobActor{person: cl.Email, tokens: cl.Tokens, admin: cl.Role.AtLeast(access.RoleAdmin)}
}

func (s *Server) jobError(c *gin.Context, what string, err error) {
	var refusal *workspace.Refusal
	var refused *gpu.JobRefused
	switch {
	case errors.As(err, &refusal):
		abort(c, http.StatusConflict, "refused", refusal.Reason)
	case errors.As(err, &refused):
		abort(c, http.StatusBadRequest, "invalid", refused.Reason)
	case errors.Is(err, gpu.ErrNotFound):
		abort(c, http.StatusNotFound, "not_found", "Job nicht gefunden.")
	default:
		s.log.Errorw("batch job "+what, "error", err)
		abort(c, http.StatusBadGateway, "gpu_cluster", "GPU-Cluster gerade nicht erreichbar.")
	}
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
//	@Success		200		{array}		gpu.Job
//	@Failure		502		{object}	ErrorResponse
//	@Router			/v1/gpu/jobs [get]
func (s *Server) listJobs(c *gin.Context) {
	ctx := c.Request.Context()
	a := s.jobActor(c)
	f := gpu.JobFilter{Project: c.Query("project"), Person: a.person}
	if f.Project != "" && mayManage(ctx, s.ws, a, f.Project) {
		f.Person = ""
	}
	jobs, err := s.gpu.Jobs(ctx, f)
	if err != nil {
		s.jobError(c, "list", err)
		return
	}
	c.JSON(http.StatusOK, jobs)
}

// submitJob godoc
//
//	@ID				submitGPUJob
//	@Summary		Submit a batch job
//	@Description	Checks the job against the project (membership, batch jobs and GPU class allowed, jobs per person, maximum run time, budget: used plus what running and waiting jobs reserve plus GPUs × max_hours of this one must fit), then queues it in the project's queue. A refusal is 409 with its reason. The job runs in the project's environment with the person's home and the shared folder; its output also goes to ~/jobs/<name>/output-<attempt>.log. A preemptible job (the project's batch_priority) is evicted for a workspace and starts again from the beginning.
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
	j, err := submitJob(c.Request.Context(), s.ws, s.gpu, s.jobActor(c), req)
	if err != nil {
		s.jobError(c, "submit", err)
		return
	}
	c.JSON(http.StatusCreated, j)
}

// getJob godoc
//
//	@ID			getGPUJob
//	@Summary	One batch job
//	@Tags		gpu
//	@Produce	json
//	@Security	BearerAuth
//	@Param		id	path		string	true	"job id"
//	@Success	200	{object}	gpu.Job
//	@Failure	404	{object}	ErrorResponse
//	@Router		/v1/gpu/jobs/{id} [get]
func (s *Server) getJob(c *gin.Context) {
	j, err := visibleJob(c.Request.Context(), s.ws, s.gpu, s.jobActor(c), c.Param("id"))
	if err != nil {
		s.jobError(c, "get", err)
		return
	}
	c.JSON(http.StatusOK, j)
}

// getJobLog godoc
//
//	@ID				getGPUJobLog
//	@Summary		The end of a batch job's output
//	@Description	Live from its pod while it runs and until Kubernetes deletes the finished job (a day), then the last 64 KB kept for 30 days. The whole output of every attempt stays in ~/jobs/<name>/.
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
		s.jobError(c, "log", err)
		return
	}
	lines, _ := strconv.Atoi(c.DefaultQuery("lines", "200"))
	log, err := s.gpu.JobLog(ctx, j.ID, min(max(lines, 1), 5000))
	if err != nil {
		s.jobError(c, "log", err)
		return
	}
	c.JSON(http.StatusOK, JobLogResponse{Log: log})
}

// cancelJob godoc
//
//	@ID				cancelGPUJob
//	@Summary		Cancel a batch job
//	@Description	Ends a waiting or running job; what it used stays booked. The person's own jobs, all of the project for its managers and admins. An ended job is returned as it is.
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
		s.jobError(c, "cancel", err)
		return
	}
	j, err = s.gpu.CancelJob(ctx, j.ID, a.person)
	if err != nil {
		s.jobError(c, "cancel", err)
		return
	}
	c.JSON(http.StatusOK, j)
}
