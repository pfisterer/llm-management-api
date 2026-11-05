package gpu

import (
	"context"
	"errors"
	"fmt"
	"math"
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
		UID  string `json:"uid"`
	} `json:"metadata"`
	Spec struct {
		Suspend               *bool  `json:"suspend"`
		ActiveDeadlineSeconds *int64 `json:"activeDeadlineSeconds"`
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

// evictedAt is when Kueue last evicted the job (its Evicted condition), so a waiting job's hours end there and not at the sync that saw it wait; nil without one.
func (w jobWorkload) evictedAt() *time.Time {
	for _, c := range w.Status.Conditions {
		if c.Type == "Evicted" && c.Status == "True" && !c.LastTransitionTime.IsZero() {
			t := c.LastTransitionTime
			return &t
		}
	}
	return nil
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

// liveJob is one job's Job and Workload, read live: found false when the Job is gone. Without a Workload (not made yet, or the read failed) w is empty, as in liveJobs.
func (s *Service) liveJob(ctx context.Context, id string) (j k8sJob, w jobWorkload, found bool, err error) {
	ns := s.kernelNamespace()
	if err := s.k8s.do(ctx, http.MethodGet, "/apis/batch/v1/namespaces/"+ns+"/jobs/"+id, nil, &j); err != nil {
		if errors.Is(err, ErrNotFound) {
			return j, w, false, nil
		}
		return j, w, false, err
	}
	var wls struct {
		Items []jobWorkload `json:"items"`
	}
	// Kueue labels a Job's Workload with the Job's uid.
	if err := s.k8s.do(ctx, http.MethodGet, "/apis/kueue.x-k8s.io/v1beta2/namespaces/"+ns+"/workloads?labelSelector="+url.QueryEscape("kueue.x-k8s.io/job-uid="+j.Metadata.UID), nil, &wls); err == nil {
		for _, item := range wls.Items {
			for _, o := range item.Metadata.OwnerReferences {
				if o.Kind == "Job" && o.Name == id {
					w = item
				}
			}
		}
	}
	return j, w, true, nil
}

// SyncJobs brings the live rows of gpu_jobs to what the cluster says: queued or running, admitted and started, evictions, the GPU hours while it runs, and at its end the state, reason, exit code and the end of its output. A job that is gone without an end (deleted by hand) is cancelled. A job is kept within its MaxHours over all its attempts (limitJobHours). Once an hour it also forgets the output of jobs that ended more than JobLogKeep ago.
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
			j, ok := jobs[r.ID]
			s.syncLive(ctx, &r, j, ok, workloads[r.ID], now)
			if ok && r.live() {
				if err := s.limitJobHours(ctx, &r, j, now); err != nil {
					errs = append(errs, err)
				}
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

// syncLive brings one live row to its Job (found) and Workload; a Job gone without an end we saw (deleted by hand) cancels it, unless it was just submitted and may not be listed yet.
func (s *Service) syncLive(ctx context.Context, r *JobRecord, j k8sJob, found bool, w jobWorkload, now time.Time) {
	if found {
		s.syncJob(ctx, r, j, w, now)
	} else if now.Sub(r.Submitted) > time.Minute {
		countHours(r, now)
		r.State, r.Reason, r.Ended = JobCancelled, "Deleted", &now
	}
}

// refreshJob brings a live row to the cluster's view right now, as SyncJobs does once a minute (without limitJobHours, which stays with it): the job's own Job and Workload, then the row is saved. On any error the row stays as it was; ended rows are returned as they are.
func (s *Service) refreshJob(ctx context.Context, r JobRecord) JobRecord {
	if !r.live() || s.k8s == nil || !s.JobsEnabled() {
		return r
	}
	j, w, found, err := s.liveJob(ctx, r.ID)
	if err != nil {
		s.log.Debugw("batch job: live status", "job", r.ID, "error", err)
		return r
	}
	before := r
	s.syncLive(ctx, &r, j, found, w, time.Now().UTC())
	if r == before {
		return r
	}
	// A cancel or the sync may have ended the row meanwhile: an ended row is not made live again.
	if cur, err := s.jobs.Job(ctx, r.ID); err != nil || !cur.live() {
		if err != nil {
			return before
		}
		return cur
	}
	if err := s.jobs.SaveJob(ctx, r); err != nil {
		s.log.Warnw("batch job: saving the live status", "job", r.ID, "error", err)
	}
	return r
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
	// Waiting: not admitted yet, or evicted and in the queue again. An evicted one ran until its eviction, not until this sync.
	until := now
	if at := w.evictedAt(); at != nil && at.Before(now) {
		until = *at
	}
	countHours(r, until)
	r.SeenRunning = nil
	r.State = JobQueued
	if r.Started != nil || r.Evictions > 0 {
		// Evicted: its pod goes with the eviction and the next attempt's has nothing yet, so the row keeps the end of the attempt's output while the pod still answers (during its grace period).
		if tail := s.lastRunTail(ctx, r.ID); tail != "" {
			r.LogTail = tail
		}
	}
}

// JobMaxHoursUsed is the reason of a job ended by the service because it ran its MaxHours over all its attempts (limitJobHours); a job that runs out of time within one attempt ends with Kubernetes' DeadlineExceeded.
const JobMaxHoursUsed = "MaxHoursUsed"

// jobOverrun is how long a running job may pass its MaxHours over all attempts before the service ends it: its own deadline (activeDeadlineSeconds, which Kubernetes may have restarted) and the grace after TERM go first, and the sync runs every minute.
func (s *Service) jobOverrun() time.Duration {
	return time.Duration(s.jobGraceSeconds())*time.Second + 2*time.Minute
}

// limitJobHours keeps a live job within its MaxHours over all its attempts. Kubernetes restarts a job's deadline (activeDeadlineSeconds) when Kueue admits it again after an eviction, so a job evicted late would get its whole MaxHours again, beyond what it reserved of the budget. A waiting job that already ran gets what is left as its deadline before it starts again; one with nothing left, or one that runs past its MaxHours (admitted again before a sync saw it wait), is ended with JobMaxHoursUsed.
func (s *Service) limitJobHours(ctx context.Context, r *JobRecord, j k8sJob, now time.Time) error {
	ran := r.ranHours(now)
	if ran <= 0 {
		return nil
	}
	left := time.Duration((r.MaxHours - ran) * float64(time.Hour))
	if r.State == JobRunning {
		if left > -s.jobOverrun() {
			return nil
		}
	} else if left >= time.Minute {
		secs := int64(math.Ceil(left.Seconds()))
		if j.Spec.ActiveDeadlineSeconds != nil && *j.Spec.ActiveDeadlineSeconds <= secs {
			return nil
		}
		// Best effort: should it fail, the job is ended once it runs past its MaxHours.
		if err := s.k8s.do(ctx, http.MethodPatch, "/apis/batch/v1/namespaces/"+s.kernelNamespace()+"/jobs/"+r.ID, map[string]any{"spec": map[string]any{"activeDeadlineSeconds": secs}}, nil); err != nil {
			s.log.Warnw("batch job: deadline for the rest of its hours", "job", r.ID, "seconds", secs, "error", err)
		}
		return nil
	}
	if err := s.stopJob(ctx, r, JobFailed, JobMaxHoursUsed, now); err != nil {
		return err
	}
	s.log.Infow("batch job ended: its maximum run time over all attempts is used", "job", r.ID, "project", r.Project, "max_hours", r.MaxHours, "evictions", r.Evictions)
	return nil
}

// ranHours is how long the job has run over all its attempts (its GPU hours per GPU), up to now while it runs.
func (r JobRecord) ranHours(now time.Time) float64 {
	h := r.GPUHours
	if r.State == JobRunning && r.SeenRunning != nil && now.After(*r.SeenRunning) {
		h += float64(r.GPUs) * now.Sub(*r.SeenRunning).Hours()
	}
	return h / float64(max(r.GPUs, 1))
}

// stopJob deletes the job with its pod (Kubernetes sends TERM and gives it jobGraceSeconds) and ends its row with state and reason; the row keeps what it used and, while the pod still answers, the end of its output.
func (s *Service) stopJob(ctx context.Context, r *JobRecord, state, reason string, now time.Time) error {
	if code, tail := s.jobOutcome(ctx, r.ID); tail != "" {
		r.ExitCode, r.LogTail = code, tail
	}
	err := s.k8s.do(ctx, http.MethodDelete, "/apis/batch/v1/namespaces/"+s.kernelNamespace()+"/jobs/"+r.ID+"?propagationPolicy=Background", nil, nil)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	countHours(r, now)
	r.State, r.Reason, r.Ended = state, reason, &now
	return nil
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
	pods, err := s.jobPods(ctx, id)
	if err != nil {
		return nil, err
	}
	return newestPod(pods, false), nil
}

// jobPods are the pods of the job: the running or ended one, and after an eviction the old one while it ends and the next attempt's while it waits to start.
func (s *Service) jobPods(ctx context.Context, id string) ([]jobPodInfo, error) {
	var pods struct {
		Items []jobPodInfo `json:"items"`
	}
	if err := s.k8s.do(ctx, http.MethodGet, "/api/v1/namespaces/"+s.kernelNamespace()+"/pods?labelSelector="+url.QueryEscape("job-name="+id), nil, &pods); err != nil {
		return nil, err
	}
	return pods.Items, nil
}

// newestPod is the newest of the pods, with started only one that got past Pending (it has output); nil without one.
func newestPod(pods []jobPodInfo, started bool) *jobPodInfo {
	var newest *jobPodInfo
	for i, p := range pods {
		if started && (p.Status.Phase == "Pending" || p.Status.Phase == "") {
			continue
		}
		if newest == nil || p.Metadata.CreationTimestamp.After(newest.Metadata.CreationTimestamp) {
			newest = &pods[i]
		}
	}
	return newest
}

// lastRunTail is the end of the output of the job's newest pod that ran; empty when there is none or it does not answer.
func (s *Service) lastRunTail(ctx context.Context, id string) string {
	if s.k8s == nil {
		return ""
	}
	pods, err := s.jobPods(ctx, id)
	if err != nil {
		return ""
	}
	if p := newestPod(pods, true); p != nil {
		return s.podLogTail(ctx, p.Metadata.Name, 5000)
	}
	return ""
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

// GetJob is one job from gpu_jobs; a waiting or running one brought to the cluster's view first (refreshJob), so that an eviction or an end shows at once and not only after the next sync.
func (s *Service) GetJob(ctx context.Context, id string) (Job, error) {
	if s.jobs == nil || !JobID(id) {
		return Job{}, ErrNotFound
	}
	r, err := s.jobs.Job(ctx, id)
	if err != nil {
		return Job{}, err
	}
	return s.refreshJob(ctx, r).job(), nil
}

// JobLog is the end of a job's output: live from its newest pod that ran (running, ending after an eviction, or until Kubernetes deletes the finished job), else what gpu_jobs kept. A live job without such a pod (waiting, evicted, its next pod still Pending) gets what was kept and a line in the style of the job script's "== …" lines saying what it waits for.
func (s *Service) JobLog(ctx context.Context, id string, lines int) (string, error) {
	if s.jobs == nil || !JobID(id) {
		return "", ErrNotFound
	}
	r, err := s.jobs.Job(ctx, id)
	if err != nil {
		return "", err
	}
	if s.k8s == nil {
		return r.LogTail, nil
	}
	pods, err := s.jobPods(ctx, id)
	if err != nil {
		return r.LogTail, nil
	}
	if pod := newestPod(pods, true); pod != nil {
		return s.podLogTail(ctx, pod.Metadata.Name, lines), nil
	}
	if !r.live() {
		return r.LogTail, nil
	}
	return r.LogTail + waitingLine(r, newestPod(pods, false) != nil, time.Now()), nil
}

// waitingLine says what a live job without a pod that ran waits for: its next attempt's pod to start (pending), the GPU again after an eviction, or a GPU at all. The attempt is counted from Kueue's evictions, as the job script's DHBW_JOB_ATTEMPT would count it.
func waitingLine(r JobRecord, pending bool, now time.Time) string {
	ran := r.Started != nil || r.Evictions > 0
	attempt := r.Evictions + 1
	if ran {
		attempt = max(attempt, 2)
	}
	at := now.UTC().Format("2006-01-02T15:04:05-07:00")
	var line string
	switch {
	case pending:
		line = fmt.Sprintf("== %s Versuch %d wird gestartet (Pod wird vorbereitet)", at, attempt)
	case ran:
		line = fmt.Sprintf("== %s wartet nach Verdrängung auf eine GPU, Versuch %d folgt", at, attempt)
	default:
		line = fmt.Sprintf("== %s wartet auf eine GPU", at)
	}
	if r.LogTail != "" && !strings.HasSuffix(r.LogTail, "\n") {
		line = "\n" + line
	}
	return line + "\n"
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
	if err := s.stopJob(ctx, &r, JobCancelled, "Cancelled", time.Now().UTC()); err != nil {
		return Job{}, err
	}
	if err := s.jobs.SaveJob(ctx, r); err != nil {
		return Job{}, err
	}
	s.log.Infow("batch job cancelled", "job", id, "by", by, "project", r.Project)
	return r.job(), nil
}

// JobReservations is what a project's live jobs still hold of its GPU-hours budgets (workspace.SetJobReservations): its GPUs × what is left of its MaxHours over all its attempts, waiting or running (limitJobHours keeps a job evicted and admitted again within it).
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
		left := max(0, r.MaxHours-r.ranHours(now))
		out = append(out, workspace.Reservation{Person: r.Person, GPUClass: r.GPUClass, GPUHours: float64(r.GPUs) * left})
	}
	return out, nil
}
