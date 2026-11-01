package gpu

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// The slugs as KubeSpawner 7.1 makes them (reference values from its kubespawner/slugs.py): the person's folder in the project's volume and the label of their pods.
func TestHubUserSlug(t *testing.T) {
	for name, want := range map[string][2]string{
		"dennis.pfisterer@dhbw.de":           {"dennis-pfisterer-dhbw-de---c771a8ee", "dennis-pfisterer-dhbw---c771a8ee"},
		"s123456@student.dhbw-mannheim.de":   {"s123456-student-dhbw-mannheim-de---8f5377f5", "s123456-student-dhbw---8f5377f5"},
		"alice":                              {"alice", "alice"},
		"Bob":                                {"bob---cd9fb1e1", "Bob"},
		"a--b":                               {"a-b---90827a2e", "a-b---90827a2e"},
		"9lives@x.de":                        {"x-9lives-x-de---74f8a58b", "x-9lives-x-de---74f8a58b"},
		strings.Repeat("x", 70) + "@dhbw.de": {"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx---7c7f256b", "xxxxxxxxxxxxxxxxxxxxx---7c7f256b"},
	} {
		if got := HubUserSlug(name); got != want[0] {
			t.Errorf("slug %q: %q, want %q", name, got, want[0])
		}
		if got := hubLabel(name); got != want[1] {
			t.Errorf("label %q: %q, want %q", name, got, want[1])
		}
	}
}

func jobTestService(t *testing.T) (*Service, *fakeFleetKube) {
	t.Helper()
	ctx := context.Background()
	s, k := fleetTestService()
	s.k8s = k
	s.cfg.Jobs.InitImage = "reg/platform/base-python:v2.7.2@sha256:abc"
	s.SetJobStore(NewMemoryJobStore())
	if err := s.SeedFleet(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.AssignNode(ctx, NodeAssignment{Node: "gpu-1", Pool: "general", Tier: "small"}); err != nil {
		t.Fatal(err)
	}
	return s, k
}

// A project with batch jobs gets a queue per class that borrows the class's GPUs up to the project's; a guaranteed one evicts preemptible jobs too; queues nobody wants go.
func TestJobQueues(t *testing.T) {
	ctx := context.Background()
	s, k := jobTestService(t)
	k.objects["/apis/kueue.x-k8s.io/v1beta2/clusterqueues/job-old--general-small"] = map[string]any{"metadata": map[string]any{"name": "job-old--general-small", "labels": map[string]any{"dhbw.cloud/queue-kind": "job"}}}
	k.objects["/apis/kueue.x-k8s.io/v1beta2/namespaces/jupyterhub/localqueues/job-old--general-small"] = map[string]any{"metadata": map[string]any{"name": "job-old--general-small"}}
	s.SetJobQueues(func(context.Context) ([]JobQueue, error) {
		return []JobQueue{{Project: "t-a", Class: "general-small", GPUs: 2}, {Project: "t-b", Class: "general-small", Guaranteed: true}, {Project: "t-c", Class: "gone"}}, nil
	})
	if err := s.ReconcileFleet(ctx); err != nil {
		t.Fatal(err)
	}
	a := dump(k.objects["/apis/kueue.x-k8s.io/v1beta2/clusterqueues/job-t-a--general-small"])
	b := dump(k.objects["/apis/kueue.x-k8s.io/v1beta2/clusterqueues/job-t-b--general-small"])
	if !strings.Contains(a, `"borrowingLimit":"2"`) || !strings.Contains(a, `"maxPriorityThreshold":-51`) || !strings.Contains(a, `"cohortName":"class-general-small"`) {
		t.Errorf("preemptible queue: %s", a)
	}
	if strings.Contains(b, `"borrowingLimit"`) || !strings.Contains(b, `"maxPriorityThreshold":-1`) {
		t.Errorf("guaranteed queue: %s", b)
	}
	if k.objects["/apis/kueue.x-k8s.io/v1beta2/namespaces/jupyterhub/localqueues/job-t-a--general-small"] == nil {
		t.Errorf("local queue missing")
	}
	for _, p := range []string{"/apis/kueue.x-k8s.io/v1beta2/clusterqueues/job-old--general-small", "/apis/kueue.x-k8s.io/v1beta2/namespaces/jupyterhub/localqueues/job-old--general-small", "/apis/kueue.x-k8s.io/v1beta2/clusterqueues/job-t-c--gone"} {
		if k.objects[p] != nil {
			t.Errorf("%s kept", p)
		}
	}
}

// inference-<class> moves from gpu-<class> to gap-<class>: a LocalQueue's ClusterQueue cannot change, so it is created anew.
func TestLocalQueueMoves(t *testing.T) {
	k := &fakeFleetKube{objects: map[string]map[string]any{}}
	k.objects["/apis/kueue.x-k8s.io/v1beta2/namespaces/inference/localqueues/inference-x"] = map[string]any{"metadata": map[string]any{"name": "inference-x"}, "spec": map[string]any{"clusterQueue": "gpu-x"}}
	if err := ensureObject(context.Background(), k, inferenceQueueFor("inference", "x")); err != nil {
		t.Fatal(err)
	}
	if q := dump(k.objects["/apis/kueue.x-k8s.io/v1beta2/namespaces/inference/localqueues/inference-x"]); !strings.Contains(q, `"clusterQueue":"gap-x"`) {
		t.Errorf("queue: %s", q)
	}
}

func TestSubmitAndSyncJob(t *testing.T) {
	ctx := context.Background()
	s, k := jobTestService(t)
	sp := JobSpec{Name: "training-1", Person: "dennis.pfisterer@dhbw.de", Project: "t-a", Class: "general-small", GPUs: 1, MaxHours: 2, Image: "reg/envs/t-a:1", Archs: []string{"amd64"},
		SharedReadOnly: true, ScratchGB: 5, StorageGB: 10, Command: "python train.py", WorkingDir: "/home/jovyan/projects/kurs"}
	for name, bad := range map[string]JobSpec{
		"no command":  {GPUs: 1, StorageGB: 1},
		"both":        {GPUs: 1, Command: "x", Notebook: "a.ipynb"},
		"outside":     {GPUs: 1, Command: "x", WorkingDir: "../other"},
		"not ipynb":   {GPUs: 1, Notebook: "a.py"},
		"bad name":    {GPUs: 1, Command: "x", Name: "Mein Job"},
		"two gpus":    {GPUs: 2, Command: "x"},
		"notebook up": {GPUs: 1, Notebook: "../x.ipynb"},
		"no storage":  {GPUs: 1, Command: "x", Project: "t-ohne"},
	} {
		var refused *JobRefused
		if _, err := s.SubmitJob(ctx, bad); !errors.As(err, &refused) {
			t.Errorf("%s: %v", name, err)
		}
	}
	j, err := s.SubmitJob(ctx, sp)
	if err != nil {
		t.Fatal(err)
	}
	if !JobID(j.ID) || j.State != JobQueued || j.WorkingDir != "projects/kurs" {
		t.Fatalf("job %+v", j)
	}
	obj := dump(k.objects["/apis/batch/v1/namespaces/jupyterhub/jobs/"+j.ID])
	for _, want := range []string{
		`"kueue.x-k8s.io/queue-name":"job-t-a--general-small"`, `"kueue.x-k8s.io/priority-class":"batch"`, `"suspend":true`, `"activeDeadlineSeconds":7200`, `"ttlSecondsAfterFinished":86400`,
		`"subPath":"homes/dennis-pfisterer-dhbw-de---c771a8ee"`, `"mountPath":"/home/jovyan/project-shared","name":"home","readOnly":true`, `"claimName":"ws-t-a"`,
		`"nvidia.com/gpu":"1"`, `"ephemeral-storage":"5Gi"`, `"kubernetes.io/arch":"amd64"`, `"component":"job"`, `"dhbw.cloud/profile":"job"`, `"hub.jupyter.org/username":"dennis.pfisterer@dhbw.de"`,
		`"name":"DHBW_JOB_WORKDIR","value":"projects/kurs"`, `"image":"reg/platform/base-python:v2.7.2@sha256:abc"`,
	} {
		if !strings.Contains(obj, want) {
			t.Errorf("job object lacks %s:\n%s", want, obj)
		}
	}
	if k.objects["/apis/kueue.x-k8s.io/v1beta2/clusterqueues/job-t-a--general-small"] == nil {
		t.Errorf("queue not made at submit")
	}
	// Waiting: it reserves its maximum.
	if rs, _ := s.JobReservations(ctx, "t-a"); len(rs) != 1 || rs[0].GPUHours != 2 || rs[0].GPUClass != "general-small" {
		t.Errorf("reservations while waiting: %+v", rs)
	}

	// Kueue admits it, the pod runs since 30 minutes.
	started := time.Now().UTC().Add(-30 * time.Minute)
	jobPath := "/apis/batch/v1/namespaces/jupyterhub/jobs/" + j.ID
	k.objects[jobPath]["spec"].(map[string]any)["suspend"] = false
	k.objects[jobPath]["status"] = map[string]any{"active": 1, "ready": 1, "startTime": started.Format(time.RFC3339)}
	if err := s.SyncJobs(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetJob(ctx, j.ID)
	if got.State != JobRunning || got.Started == nil {
		t.Fatalf("running: %+v", got)
	}
	if rs, _ := s.JobReservations(ctx, "t-a"); len(rs) != 1 || rs[0].GPUHours < 1.49 || rs[0].GPUHours > 1.51 {
		t.Errorf("reservations while running: %+v", rs)
	}

	// It ends with exit code 3.
	k.objects[jobPath]["status"] = map[string]any{"startTime": started.Format(time.RFC3339), "conditions": []any{map[string]any{"type": "Failed", "status": "True", "reason": "BackoffLimitExceeded", "lastTransitionTime": time.Now().UTC().Format(time.RFC3339)}}}
	k.objects["/api/v1/namespaces/jupyterhub/pods/"+j.ID+"-abcde"] = map[string]any{"metadata": map[string]any{"name": j.ID + "-abcde", "creationTimestamp": started.Format(time.RFC3339)},
		"status": map[string]any{"phase": "Failed", "containerStatuses": []any{map[string]any{"name": "job", "state": map[string]any{"terminated": map[string]any{"exitCode": 3}}}}}}
	if err := s.SyncJobs(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetJob(ctx, j.ID)
	if got.State != JobFailed || got.Reason != "BackoffLimitExceeded" || got.ExitCode == nil || *got.ExitCode != 3 || got.Ended == nil || got.GPUHours <= 0 {
		t.Fatalf("ended: %+v", got)
	}
	if rs, _ := s.JobReservations(ctx, "t-a"); len(rs) != 0 {
		t.Errorf("an ended job reserves: %+v", rs)
	}
}

// A job that starts and ends between two syncs is booked from its start to its end.
func TestShortJobHours(t *testing.T) {
	ctx := context.Background()
	s, k := jobTestService(t)
	j, err := s.SubmitJob(ctx, JobSpec{Person: "alice", Project: "t-a", Class: "general-small", GPUs: 1, MaxHours: 1, Image: "img", Command: "true", StorageGB: 5})
	if err != nil {
		t.Fatal(err)
	}
	start, end := time.Now().UTC().Add(-30*time.Minute).Truncate(time.Second), time.Now().UTC().Add(-6*time.Minute).Truncate(time.Second)
	k.objects["/apis/batch/v1/namespaces/jupyterhub/jobs/"+j.ID]["status"] = map[string]any{"startTime": start.Format(time.RFC3339),
		"conditions": []any{map[string]any{"type": "Complete", "status": "True", "lastTransitionTime": end.Format(time.RFC3339)}}}
	if err := s.SyncJobs(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetJob(ctx, j.ID)
	if got.State != JobSucceeded || got.GPUHours < 0.39 || got.GPUHours > 0.41 {
		t.Errorf("short job: %+v", got)
	}
}

// A job before any workspace of its project makes the project's volume as the hub would; a smaller one is grown, never shrunk.
func TestJobProjectVolume(t *testing.T) {
	ctx := context.Background()
	s, k := jobTestService(t)
	if err := s.ensureProjectVolume(ctx, "t-neu", 7); err != nil {
		t.Fatal(err)
	}
	pvc := dump(k.objects["/api/v1/namespaces/jupyterhub/persistentvolumeclaims/ws-t-neu"])
	for _, want := range []string{`"storage":"7Gi"`, `"storageClassName":"juicefs-projects"`, `"ReadWriteMany"`, `"dhbw.cloud/project":"t-neu"`, `"dhbw.cloud/workspace-storage":"true"`} {
		if !strings.Contains(pvc, want) {
			t.Errorf("volume lacks %s: %s", want, pvc)
		}
	}
	if err := s.ensureProjectVolume(ctx, "t-neu", 9); err != nil || k.patched["/api/v1/namespaces/jupyterhub/persistentvolumeclaims/ws-t-neu"] == nil {
		t.Errorf("grow: %v %v", err, k.patched)
	}
	delete(k.patched, "/api/v1/namespaces/jupyterhub/persistentvolumeclaims/ws-t-neu")
	if err := s.ensureProjectVolume(ctx, "t-neu", 3); err != nil || k.patched["/api/v1/namespaces/jupyterhub/persistentvolumeclaims/ws-t-neu"] != nil {
		t.Errorf("shrunk: %v %v", err, k.patched)
	}
}

func TestCancelJob(t *testing.T) {
	ctx := context.Background()
	s, k := jobTestService(t)
	j, err := s.SubmitJob(ctx, JobSpec{Person: "alice", Project: "t-a", Class: "general-small", GPUs: 1, MaxHours: 1, Image: "img", Notebook: "kurs/a.ipynb", StorageGB: 5})
	if err != nil {
		t.Fatal(err)
	}
	obj := dump(k.objects["/apis/batch/v1/namespaces/jupyterhub/jobs/"+j.ID])
	if !strings.Contains(obj, `papermill --log-output '/home/jovyan/kurs/a.ipynb' '/home/jovyan/kurs/a.`+j.ID+`.ipynb'`) || !strings.Contains(obj, `"name":"DHBW_JOB_WORKDIR","value":"kurs"`) {
		t.Errorf("notebook job: %s", obj)
	}
	c, err := s.CancelJob(ctx, j.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if c.State != JobCancelled || k.objects["/apis/batch/v1/namespaces/jupyterhub/jobs/"+j.ID] != nil {
		t.Errorf("cancelled: %+v", c)
	}
	if _, err := s.CancelJob(ctx, "nope", "alice"); err != ErrNotFound {
		t.Errorf("unknown id: %v", err)
	}
}

func TestHomeRel(t *testing.T) {
	for in, want := range map[string]string{"": "", "/home/jovyan": "", "/home/jovyan/a/b": "a/b", "a/./b/": "a/b", "a/../b": "b"} {
		if got, ok := homeRel(in); !ok || got != want {
			t.Errorf("%q: %q %v, want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"../x", "/home/jovyan/../x", "a/../../x"} {
		if got, ok := homeRel(in); ok {
			t.Errorf("%q accepted as %q", in, got)
		}
	}
}

// A job gets what a GPU of its class has after the reserve, at most 2/4 cores and 8 GiB: gpu-1 (general-small) has 8 cores and 24 GiB for one GPU, minus 1 core and 2 GiB.
func TestJobResources(t *testing.T) {
	ctx := context.Background()
	s, _ := jobTestService(t)
	req, lim := s.jobResources(ctx, "general-small")
	if req["cpu"] != "2000m" || lim["cpu"] != "4000m" || req["memory"] != "8128Mi" || lim["memory"] != "8128Mi" {
		t.Errorf("large share: %v %v", req, lim)
	}
	s.fleetCfg.ReserveMemoryGi, s.fleetCfg.ReserveCPU = 20, 6.5
	req, lim = s.jobResources(ctx, "general-small")
	if req["cpu"] != "1500m" || lim["cpu"] != "1500m" || req["memory"] != "4032Mi" {
		t.Errorf("small share: %v %v", req, lim)
	}
	if req, _ := s.jobResources(ctx, "unknown"); req["memory"] != "8128Mi" {
		t.Errorf("class without nodes: %v", req)
	}
}
