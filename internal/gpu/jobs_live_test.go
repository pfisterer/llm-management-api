package gpu

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// failingJobKube fails every read of a Job: the live status of GetJob is best effort.
type failingJobKube struct{ *fakeFleetKube }

func (f failingJobKube) do(ctx context.Context, method, path string, body, out any) error {
	if method == "GET" && strings.HasPrefix(path, "/apis/batch/v1/") {
		return errors.New("kube unreachable")
	}
	return f.fakeFleetKube.do(ctx, method, path, body, out)
}

// GetJob shows an eviction at once, without waiting for SyncJobs; the row keeps the end of the evicted attempt's output, and the log in the gap says what the job waits for.
func TestJobLiveStatusAndGapLog(t *testing.T) {
	ctx := context.Background()
	s, k := jobTestService(t)
	k.logs = map[string]string{}
	j, err := s.SubmitJob(ctx, JobSpec{Person: "alice", Project: "t-a", Class: "general-small", GPUs: 1, MaxHours: 2, Image: "img", Command: "python train.py", StorageGB: 5})
	if err != nil {
		t.Fatal(err)
	}
	jobPath := "/apis/batch/v1/namespaces/jupyterhub/jobs/" + j.ID
	podPath := "/api/v1/namespaces/jupyterhub/pods/" + j.ID + "-aaaaa"
	wlPath := "/apis/kueue.x-k8s.io/v1beta2/namespaces/jupyterhub/workloads/job-" + j.ID + "-12345"
	k.objects[jobPath]["metadata"].(map[string]any)["uid"] = "uid-1"

	// Waiting for a GPU, never ran.
	if log, _ := s.JobLog(ctx, j.ID, 200); !strings.Contains(log, "wartet auf eine GPU") || strings.Contains(log, "Verdrängung") {
		t.Errorf("log while waiting: %q", log)
	}

	// Kueue admits it and the pod runs: GetJob says so before any sync.
	started := time.Now().UTC().Add(-10 * time.Minute)
	k.objects[jobPath]["spec"].(map[string]any)["suspend"] = false
	k.objects[jobPath]["status"] = map[string]any{"active": 1, "ready": 1, "startTime": started.Format(time.RFC3339)}
	k.objects[podPath] = map[string]any{"metadata": map[string]any{"name": j.ID + "-aaaaa", "creationTimestamp": started.Format(time.RFC3339)}, "status": map[string]any{"phase": "Running"}}
	k.logs[j.ID+"-aaaaa"] = "== 2026-11-05T10:00:00+00:00 Job x (" + j.ID + "), Versuch 1\nSchritt 123\n"
	got, err := s.GetJob(ctx, j.ID)
	if err != nil || got.State != JobRunning || got.Started == nil {
		t.Fatalf("running: %+v %v", got, err)
	}
	if log, _ := s.JobLog(ctx, j.ID, 200); !strings.Contains(log, "Schritt 123") || strings.Contains(log, "wartet") {
		t.Errorf("live log: %q", log)
	}

	// A workspace evicts it: Kueue suspends the Job, the pod ends within its grace period. The hours count until the GetJob above saw it running.
	evicted := time.Now().UTC().Add(-time.Minute)
	k.objects[jobPath]["spec"].(map[string]any)["suspend"] = true
	k.objects[jobPath]["status"] = map[string]any{"active": 1}
	k.objects[wlPath] = map[string]any{"metadata": map[string]any{"name": "job-" + j.ID + "-12345", "labels": map[string]any{"kueue.x-k8s.io/job-uid": "uid-1"}, "ownerReferences": []any{map[string]any{"kind": "Job", "name": j.ID}}},
		"status": map[string]any{"conditions": []any{map[string]any{"type": "Evicted", "status": "True", "lastTransitionTime": evicted.Format(time.RFC3339)}}, "schedulingStats": map[string]any{"evictions": []any{map[string]any{"count": 1}}}}}
	k.logs[j.ID+"-aaaaa"] += "== 2026-11-05T10:10:00+00:00 Signal erhalten (Verdrängung, Abbruch oder Höchstlaufzeit), Exit-Code des Befehls 143\n"
	got, _ = s.GetJob(ctx, j.ID)
	if got.State != JobQueued || got.Evictions != 1 || got.GPUHours < 0.16 || got.GPUHours > 0.17 {
		t.Fatalf("evicted: %+v", got)
	}
	if r, _ := s.jobs.Job(ctx, j.ID); !strings.Contains(r.LogTail, "Signal erhalten") {
		t.Errorf("tail of the evicted attempt not kept: %q", r.LogTail)
	}

	// The pod is gone: the kept output and a line for the gap.
	delete(k.objects, podPath)
	log, _ := s.JobLog(ctx, j.ID, 200)
	if !strings.Contains(log, "Schritt 123") || !strings.HasSuffix(log, "wartet nach Verdrängung auf eine GPU, Versuch 2 folgt\n") {
		t.Errorf("log in the gap: %q", log)
	}
	// The next attempt's pod waits to start.
	k.objects[podPath+"b"] = map[string]any{"metadata": map[string]any{"name": j.ID + "-aaaaab", "creationTimestamp": time.Now().UTC().Format(time.RFC3339)}, "status": map[string]any{"phase": "Pending"}}
	if log, _ := s.JobLog(ctx, j.ID, 200); !strings.Contains(log, "Schritt 123") || !strings.Contains(log, "Versuch 2 wird gestartet") {
		t.Errorf("log with a pending pod: %q", log)
	}

	// Kubernetes cannot be read: the row as it is.
	s.k8s = failingJobKube{k}
	if got, err := s.GetJob(ctx, j.ID); err != nil || got.State != JobQueued || got.Evictions != 1 {
		t.Errorf("without kube: %+v %v", got, err)
	}
	s.k8s = k

	// It ends: GetJob takes the end from the Job, the exit code and output from its pod.
	ended := time.Now().UTC()
	delete(k.objects, podPath+"b")
	k.objects[podPath] = map[string]any{"metadata": map[string]any{"name": j.ID + "-aaaaa", "creationTimestamp": started.Format(time.RFC3339)},
		"status": map[string]any{"phase": "Succeeded", "containerStatuses": []any{map[string]any{"name": "job", "state": map[string]any{"terminated": map[string]any{"exitCode": 0}}}}}}
	k.logs[j.ID+"-aaaaa"] = "== Versuch 2\nfertig\n"
	k.objects[jobPath]["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Complete", "status": "True", "lastTransitionTime": ended.Format(time.RFC3339)}}}
	got, _ = s.GetJob(ctx, j.ID)
	if got.State != JobSucceeded || got.ExitCode == nil || *got.ExitCode != 0 {
		t.Fatalf("ended: %+v", got)
	}
	delete(k.objects, podPath)
	if log, _ := s.JobLog(ctx, j.ID, 200); log != "== Versuch 2\nfertig\n" {
		t.Errorf("log after the end: %q", log)
	}
}

// A cancel between the live read and the save is not undone by GetJob.
func TestJobLiveStatusKeepsCancel(t *testing.T) {
	ctx := context.Background()
	s, k := jobTestService(t)
	j, err := s.SubmitJob(ctx, JobSpec{Person: "alice", Project: "t-a", Class: "general-small", GPUs: 1, MaxHours: 1, Image: "img", Command: "true", StorageGB: 5})
	if err != nil {
		t.Fatal(err)
	}
	jobPath := "/apis/batch/v1/namespaces/jupyterhub/jobs/" + j.ID
	k.objects[jobPath]["spec"].(map[string]any)["suspend"] = false
	k.objects[jobPath]["status"] = map[string]any{"active": 1, "ready": 1, "startTime": time.Now().UTC().Format(time.RFC3339)}
	r, _ := s.jobs.Job(ctx, j.ID)
	now := time.Now().UTC()
	cancelled := r
	cancelled.State, cancelled.Reason, cancelled.Ended = JobCancelled, "Cancelled", &now
	if err := s.jobs.SaveJob(ctx, cancelled); err != nil {
		t.Fatal(err)
	}
	if got := s.refreshJob(ctx, r); got.State != JobCancelled {
		t.Errorf("refresh of a stale row: %+v", got.job())
	}
	if got, _ := s.jobs.Job(ctx, j.ID); got.State != JobCancelled {
		t.Errorf("cancel undone: %+v", got.job())
	}
}
