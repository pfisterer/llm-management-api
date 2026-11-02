package gpu

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/pfisterer/llm-management-api/internal/workspace"
)

// The live side of batch jobs: Kueue's and Kubernetes' view merged into the rows of gpu_jobs (SyncJobs), the queue position, the output and cancelling.

// k8sJob is the part of a batch/v1 Job the sync reads.
type k8sJob struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Suspend *bool `json:"suspend"`
	} `json:"spec"`
	Status struct {
		Active         int        `json:"active"`
		Ready          *int       `json:"ready"`
		StartTime      *time.Time `json:"startTime"`
		CompletionTime *time.Time `json:"completionTime"`
		Conditions     []struct {
			Type               string    `json:"type"`
			Status             string    `json:"status"`
			Reason             string    `json:"reason"`
			LastTransitionTime time.Time `json:"lastTransitionTime"`
		} `json:"conditions"`
	} `json:"status"`
}

// ended is the job's end: Complete or Failed with its reason and time; ok false while it lives.
func (j k8sJob) ended() (state, reason string, at time.Time, ok bool) {
	for _, c := range j.Status.Conditions {
		if c.Status != "True" {
			continue
		}
		switch c.Type {
		case "Complete":
			return JobSucceeded, "", c.LastTransitionTime, true
		case "Failed":
			return JobFailed, c.Reason, c.LastTransitionTime, true
		}
	}
	return "", "", time.Time{}, false
}

// running: admitted by Kueue (not suspended) with a ready pod.
func (j k8sJob) running() bool {
	if j.Spec.Suspend != nil && *j.Spec.Suspend {
		return false
	}
	if j.Status.Ready != nil {
		return *j.Status.Ready > 0
	}
	return j.Status.Active > 0
}

// jobWorkload is the part of a job's Kueue Workload the sync reads.
type jobWorkload struct {
	Metadata struct {
		Name            string `json:"name"`
		OwnerReferences []struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"ownerReferences"`
	} `json:"metadata"`
	Status struct {
		Admission  any `json:"admission"`
		Conditions []struct {
			Type               string    `json:"type"`
			Status             string    `json:"status"`
			LastTransitionTime time.Time `json:"lastTransitionTime"`
		} `json:"conditions"`
		SchedulingStats *struct {
			Evictions []struct {
				Count int `json:"count"`
			} `json:"evictions"`
		} `json:"schedulingStats"`
	} `json:"status"`
}

func (w jobWorkload) evictions() int {
	n := 0
	if w.Status.SchedulingStats != nil {
		for _, e := range w.Status.SchedulingStats.Evictions {
			n += e.Count
		}
	}
	return n
}

func (w jobWorkload) admittedAt() *time.Time {
	if w.Status.Admission == nil {
		return nil
	}
	for _, c := range w.Status.Conditions {
		if c.Type == "Admitted" && c.Status == "True" && !c.LastTransitionTime.IsZero() {
			t := c.LastTransitionTime
			return &t
		}
	}
	t := time.Now().UTC()
	return &t
}

// liveJobs are the cluster's batch jobs and their Workloads, by job id.
func (s *Service) liveJobs(ctx context.Context) (map[string]k8sJob, map[string]jobWorkload, error) {
	ns := s.kernelNamespace()
	var jobs struct {
		Items []k8sJob `json:"items"`
	}
	if err := s.k8s.do(ctx, http.MethodGet, "/apis/batch/v1/namespaces/"+ns+"/jobs?labelSelector="+url.QueryEscape("app="+jobApp), nil, &jobs); err != nil {
		return nil, nil, err
	}
	byID := map[string]k8sJob{}
	for _, j := range jobs.Items {
		byID[j.Metadata.Name] = j
	}
	var wls struct {
		Items []jobWorkload `json:"items"`
	}
	workloads := map[string]jobWorkload{}
	if err := s.k8s.do(ctx, http.MethodGet, "/apis/kueue.x-k8s.io/v1beta2/namespaces/"+ns+"/workloads", nil, &wls); err == nil {
		for _, w := range wls.Items {
			for _, o := range w.Metadata.OwnerReferences {
				if o.Kind == "Job" {
					workloads[o.Name] = w
				}
			}
		}
	}
	return byID, workloads, nil
}

// SyncJobs brings the live rows of gpu_jobs to what the cluster says: queued or running, admitted and started, evictions, the GPU hours while it runs, and at its end the state, reason, exit code and the end of its output. A job that is gone without an end (deleted by hand) is cancelled. Once an hour it also forgets the output of jobs that ended more than JobLogKeep ago.
func (s *Service) SyncJobs(ctx context.Context) error {
	if !s.JobsEnabled() {
		return nil
	}
	live, err := s.jobs.Jobs(ctx, JobFilter{Live: true})
	if err != nil {
		return err
	}
	if len(live) > 0 {
		jobs, workloads, err := s.liveJobs(ctx)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		var errs []error
		for _, r := range live {
			before := r
			if j, ok := jobs[r.ID]; ok {
				s.syncJob(ctx, &r, j, workloads[r.ID], now)
			} else if now.Sub(r.Submitted) > time.Minute {
				// Gone without an end we saw (deleted by hand); a job just submitted may not be listed yet.
				countHours(&r, now)
				r.State, r.Reason, r.Ended = JobCancelled, "Deleted", &now
			}
			if r != before {
				if err := s.jobs.SaveJob(ctx, r); err != nil {
					errs = append(errs, err)
				}
			}
		}
		if err := errors.Join(errs...); err != nil {
			return err
		}
	}
	if time.Since(s.jobLogsPurged) > time.Hour {
		s.jobLogsPurged = time.Now()
		return s.purgeJobLogs(ctx)
	}
	return nil
}

func (s *Service) syncJob(ctx context.Context, r *JobRecord, j k8sJob, w jobWorkload, now time.Time) {
	r.Evictions = max(r.Evictions, w.evictions())
	if r.Admitted == nil {
		r.Admitted = w.admittedAt()
	}
	if state, reason, at, ok := j.ended(); ok {
		if r.SeenRunning == nil && j.Status.StartTime != nil && at.After(*j.Status.StartTime) {
			// Started and ended between two syncs: it ran from the start of its attempt.
			r.SeenRunning = j.Status.StartTime
		}
		countHours(r, at)
		r.State, r.Reason, r.Ended = state, reason, &at
		if r.Started == nil && j.Status.StartTime != nil {
			r.Started = j.Status.StartTime
		}
		code, tail := s.jobOutcome(ctx, r.ID)
		r.ExitCode, r.LogTail = code, tail
		s.log.Infow("batch job ended", "job", r.ID, "state", state, "reason", reason, "gpu_hours", r.GPUHours, "evictions", r.Evictions)
		return
	}
	if j.running() {
		// The job's start is its first; Kubernetes sets startTime anew when Kueue admits it again after an eviction, and the deadline counts from there.
		attempt := j.Status.StartTime
		if attempt == nil {
			attempt = &now
		}
		if r.Started == nil {
			r.Started = attempt
		}
		r.AttemptStarted = attempt
		if r.SeenRunning == nil {
			// Running since the attempt's start, which may be before this sync saw it.
			r.SeenRunning = attempt
		}
		countHours(r, now)
		r.SeenRunning = &now
		r.State = JobRunning
		return
	}
	// Waiting: not admitted yet, or evicted and in the queue again.
	countHours(r, now)
	r.SeenRunning = nil
	r.State = JobQueued
}

// countHours adds the GPU hours since the sync last saw the job running, up to until.
func countHours(r *JobRecord, until time.Time) {
	if r.SeenRunning != nil && until.After(*r.SeenRunning) {
		r.GPUHours += float64(r.GPUs) * until.Sub(*r.SeenRunning).Hours()
		r.SeenRunning = &until
	}
}

// jobOutcome is the exit code of the job's last pod and the end of its output (JobLogTail).
func (s *Service) jobOutcome(ctx context.Context, id string) (*int, string) {
	pod, err := s.jobPod(ctx, id)
	if err != nil || pod == nil {
		return nil, ""
	}
	var code *int
	for _, c := range pod.Status.ContainerStatuses {
		if c.Name == "job" && c.State.Terminated != nil {
			v := c.State.Terminated.ExitCode
			code = &v
		}
	}
	return code, s.podLogTail(ctx, pod.Metadata.Name, 5000)
}

// jobPodInfo is the part of a job's pod the service reads.
type jobPodInfo struct {
	Metadata struct {
		Name              string    `json:"name"`
		CreationTimestamp time.Time `json:"creationTimestamp"`
	} `json:"metadata"`
	Status struct {
		Phase             string            `json:"phase"`
		ContainerStatuses []containerStatus `json:"containerStatuses"`
	} `json:"status"`
}

// jobPod is the newest pod of the job; nil when it has none (waiting, deleted).
func (s *Service) jobPod(ctx context.Context, id string) (*jobPodInfo, error) {
	var pods struct {
		Items []jobPodInfo `json:"items"`
	}
	if err := s.k8s.do(ctx, http.MethodGet, "/api/v1/namespaces/"+s.kernelNamespace()+"/pods?labelSelector="+url.QueryEscape("job-name="+id), nil, &pods); err != nil {
		return nil, err
	}
	var newest *jobPodInfo
	for i, p := range pods.Items {
		if newest == nil || p.Metadata.CreationTimestamp.After(newest.Metadata.CreationTimestamp) {
			newest = &pods.Items[i]
		}
	}
	return newest, nil
}

// podLogTail is the last lines of the job container's output, at most JobLogTail bytes.
func (s *Service) podLogTail(ctx context.Context, pod string, lines int) string {
	var log string
	q := url.Values{"container": {"job"}, "tailLines": {strconv.Itoa(lines)}, "limitBytes": {strconv.Itoa(4 * JobLogTail)}}
	if err := s.k8s.do(ctx, http.MethodGet, "/api/v1/namespaces/"+s.kernelNamespace()+"/pods/"+pod+"/log?"+q.Encode(), nil, &log); err != nil {
		return ""
	}
	if len(log) > JobLogTail {
		log = log[len(log)-JobLogTail:]
		if i := strings.IndexByte(log, '\n'); i >= 0 {
			log = log[i+1:]
		}
	}
	return log
}

// purgeJobLogs forgets the output of jobs that ended more than JobLogKeep ago; the rest of the row stays with the project.
func (s *Service) purgeJobLogs(ctx context.Context) error {
	all, err := s.jobs.Jobs(ctx, JobFilter{})
	if err != nil {
		return err
	}
	cut := time.Now().Add(-JobLogKeep)
	for _, r := range all {
		if r.LogTail != "" && r.Ended != nil && r.Ended.Before(cut) {
			r.LogTail = ""
			if err := s.jobs.SaveJob(ctx, r); err != nil {
				return err
			}
		}
	}
	return nil
}

// Jobs lists jobs from gpu_jobs, newest first; a waiting one with its place in its queue.
func (s *Service) Jobs(ctx context.Context, f JobFilter) ([]Job, error) {
	if s.jobs == nil {
		return []Job{}, nil
	}
	rows, err := s.jobs.Jobs(ctx, f)
	if err != nil {
		return nil, err
	}
	out := make([]Job, 0, len(rows))
	positions := map[string]map[string]int{} // queue → workload → place
	for _, r := range rows {
		j := r.job()
		if r.State == JobQueued && s.k8s != nil {
			queue := JobQueueName(r.Project, r.GPUClass)
			if s.cfg.Kernels.NoGPU {
				queue = s.cfg.Kernels.Queue
			}
			if _, ok := positions[queue]; !ok {
				positions[queue] = s.queuePositions(ctx, queue)
			}
			j.Position = positions[queue][r.ID]
		}
		out = append(out, j)
	}
	return out, nil
}

// queuePositions are the places of the jobs waiting in a LocalQueue (1 = next), by job id; Kueue's visibility API, best effort.
func (s *Service) queuePositions(ctx context.Context, queue string) map[string]int {
	var pending struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Position int `json:"positionInLocalQueue"`
		} `json:"items"`
	}
	out := map[string]int{}
	if err := s.k8s.do(ctx, http.MethodGet, "/apis/visibility.kueue.x-k8s.io/v1beta2/namespaces/"+s.kernelNamespace()+"/localqueues/"+queue+"/pendingworkloads", nil, &pending); err != nil {
		return out
	}
	// The visibility API names the Workload only, not its Job: a Job's Workload is "job-<job name>-<hash>" (Kueue's naming), and job ids contain no "-" after their prefix "j-".
	for _, w := range pending.Items {
		name := strings.TrimPrefix(w.Metadata.Name, "job-")
		if i := strings.LastIndex(name, "-"); i > 0 && JobID(name[:i]) {
			out[name[:i]] = w.Position + 1
		}
	}
	return out
}

// GetJob is one job from gpu_jobs.
func (s *Service) GetJob(ctx context.Context, id string) (Job, error) {
	if s.jobs == nil || !JobID(id) {
		return Job{}, ErrNotFound
	}
	r, err := s.jobs.Job(ctx, id)
	if err != nil {
		return Job{}, err
	}
	return r.job(), nil
}

// JobLog is the end of a job's output: live from its pod while there is one (running, or until Kubernetes deletes the finished job), else what gpu_jobs kept.
func (s *Service) JobLog(ctx context.Context, id string, lines int) (string, error) {
	if s.jobs == nil || !JobID(id) {
		return "", ErrNotFound
	}
	r, err := s.jobs.Job(ctx, id)
	if err != nil {
		return "", err
	}
	if s.k8s != nil {
		if pod, err := s.jobPod(ctx, id); err == nil && pod != nil && pod.Status.Phase != "Pending" {
			return s.podLogTail(ctx, pod.Metadata.Name, lines), nil
		}
	}
	return r.LogTail, nil
}

// CancelJob ends a waiting or running job: Kubernetes deletes it with its pod; the row keeps what it used and, while the pod still answers, the end of its output.
func (s *Service) CancelJob(ctx context.Context, id, by string) (Job, error) {
	if s.jobs == nil || !JobID(id) {
		return Job{}, ErrNotFound
	}
	r, err := s.jobs.Job(ctx, id)
	if err != nil {
		return Job{}, err
	}
	if !r.live() {
		return r.job(), nil
	}
	now := time.Now().UTC()
	if code, tail := s.jobOutcome(ctx, id); tail != "" {
		r.ExitCode, r.LogTail = code, tail
	}
	err = s.k8s.do(ctx, http.MethodDelete, "/apis/batch/v1/namespaces/"+s.kernelNamespace()+"/jobs/"+id+"?propagationPolicy=Background", nil, nil)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Job{}, err
	}
	countHours(&r, now)
	r.State, r.Reason, r.Ended = JobCancelled, "Cancelled", &now
	if err := s.jobs.SaveJob(ctx, r); err != nil {
		return Job{}, err
	}
	s.log.Infow("batch job cancelled", "job", id, "by", by, "project", r.Project)
	return r.job(), nil
}

// JobReservations is what a project's live jobs still hold of its GPU-hours budgets (workspace.SetJobReservations): a waiting job its GPUs × its maximum, a running one × what is left of it (its deadline restarts after an eviction, so a job evicted later reserves its maximum again).
func (s *Service) JobReservations(ctx context.Context, project string) ([]workspace.Reservation, error) {
	if s.jobs == nil {
		return nil, nil
	}
	live, err := s.jobs.Jobs(ctx, JobFilter{Project: project, Live: true})
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := make([]workspace.Reservation, 0, len(live))
	for _, r := range live {
		left := r.MaxHours
		if r.State == JobRunning && r.AttemptStarted != nil {
			left = max(0, r.MaxHours-now.Sub(*r.AttemptStarted).Hours())
		}
		out = append(out, workspace.Reservation{Person: r.Person, GPUClass: r.GPUClass, GPUHours: float64(r.GPUs) * left})
	}
	return out, nil
}
