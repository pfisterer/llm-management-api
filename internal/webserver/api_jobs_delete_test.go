package webserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/pfisterer/llm-management-api/internal/cluster/gpu"
	"github.com/pfisterer/llm-management-api/internal/cluster/usage"
)

// DELETE on a live job cancels it (200 with the job), on an ended one it deletes the row (204): the job's person and the project's managers may, an outsider gets 404 like for any job they cannot see. The usage, where the hours are booked, stays as it was.
func TestDeleteEndedJob(t *testing.T) {
	u := usage.NewMemoryStore()
	day := usage.Day(time.Now())
	booked := []usage.Row{{Day: day, Project: "t-kurs", Person: jzMember, Profile: "gpu", GPUClass: "mannheim-large", GPUHours: 2.5}}
	if err := u.Add(context.Background(), booked); err != nil {
		t.Fatal(err)
	}
	e := newJobEnv(t, func(o *Options, _ *gpu.Config) { o.Usage = u })
	ctx := context.Background()

	own := e.submit(t, jzMember, SubmitJobRequest{Project: "t-kurs", Command: "python a.py"})
	managed := e.submit(t, jzOther, SubmitJobRequest{Project: "t-kurs", Command: "python o.py"})

	// Live: the first DELETE cancels and returns the job.
	for _, id := range []string{own, managed} {
		code, body := call(t, e.h, "DELETE", "/v1/gpu/jobs/"+id, "oidc:"+jzManager, "", nil)
		var j gpu.Job
		if _ = json.Unmarshal(body, &j); code != http.StatusOK || j.State != gpu.JobCancelled {
			t.Fatalf("cancel %s: %d %s", id, code, body)
		}
	}
	// Ended: an outsider sees nothing, so nothing is deleted.
	if code, body := call(t, e.h, "DELETE", "/v1/gpu/jobs/"+own, "oidc:"+jzOutsider, "", nil); code != http.StatusNotFound {
		t.Fatalf("outsider deletes: %d %s", code, body)
	}
	if _, err := e.jobs.Job(ctx, own); err != nil {
		t.Fatalf("job gone after the outsider's DELETE: %v", err)
	}
	// The person deletes their own, a manager another member's.
	for _, c := range []struct{ id, who string }{{own, jzMember}, {managed, jzManager}} {
		if code, body := call(t, e.h, "DELETE", "/v1/gpu/jobs/"+c.id, "oidc:"+c.who, "", nil); code != http.StatusNoContent {
			t.Fatalf("%s deletes %s: %d %s", c.who, c.id, code, body)
		}
		if _, err := e.jobs.Job(ctx, c.id); !errors.Is(err, gpu.ErrNotFound) {
			t.Fatalf("%s still there: %v", c.id, err)
		}
	}
	// Deleted is deleted: a second DELETE is 404.
	if code, _ := call(t, e.h, "DELETE", "/v1/gpu/jobs/"+own, "oidc:"+jzMember, "", nil); code != http.StatusNotFound {
		t.Fatalf("second delete: %d", code)
	}
	rows, err := u.Range(ctx, day, day.Add(24*time.Hour))
	if err != nil || len(rows) != 1 || rows[0].GPUHours != 2.5 {
		t.Fatalf("usage changed by deleting jobs: %+v %v", rows, err)
	}
}

// The bulk delete needs state=ended, deletes what the caller sees with the same filter as the list, and leaves live jobs alone.
func TestDeleteEndedJobsBulk(t *testing.T) {
	e := newJobEnv(t)
	ctx := context.Background()
	ended1 := e.submit(t, jzMember, SubmitJobRequest{Project: "t-kurs", Command: "python a.py"})
	ended2 := e.submit(t, jzOther, SubmitJobRequest{Project: "t-kurs", Command: "python b.py"})
	live := e.submit(t, jzMember, SubmitJobRequest{Project: "t-kurs", Command: "python c.py"})
	for _, id := range []string{ended1, ended2} {
		if code, body := call(t, e.h, "DELETE", "/v1/gpu/jobs/"+id, "oidc:"+jzAdmin, "", nil); code != http.StatusOK {
			t.Fatalf("cancel %s: %d %s", id, code, body)
		}
	}
	if code, _ := call(t, e.h, "DELETE", "/v1/gpu/jobs", "oidc:"+jzMember, "", nil); code != http.StatusBadRequest {
		t.Fatalf("without state: %d, want 400", code)
	}
	// The member without project filter: only their own ended job.
	code, body := call(t, e.h, "DELETE", "/v1/gpu/jobs?state=ended", "oidc:"+jzMember, "", nil)
	var r DeleteJobsResponse
	if _ = json.Unmarshal(body, &r); code != http.StatusOK || r.Deleted != 1 {
		t.Fatalf("member's bulk delete: %d %s", code, body)
	}
	if _, err := e.jobs.Job(ctx, ended2); err != nil {
		t.Fatalf("the other member's job was deleted by the member: %v", err)
	}
	// The manager with the project: the other member's ended job; the live one stays.
	code, body = call(t, e.h, "DELETE", "/v1/gpu/jobs?state=ended&project=t-kurs", "oidc:"+jzManager, "", nil)
	if _ = json.Unmarshal(body, &r); code != http.StatusOK || r.Deleted != 1 {
		t.Fatalf("manager's bulk delete: %d %s", code, body)
	}
	if j, err := e.jobs.Job(ctx, live); err != nil || j.State != gpu.JobQueued {
		t.Fatalf("live job touched: %+v %v", j, err)
	}
	_, _ = call(t, e.h, "DELETE", "/v1/gpu/jobs/"+live, "oidc:"+jzAdmin, "", nil)
}

// The service refuses to delete a live job; the store deletes only ended rows.
func TestDeleteJobRefusesLive(t *testing.T) {
	e := newJobEnv(t)
	id := e.submit(t, jzMember, SubmitJobRequest{Project: "t-kurs", Command: "python a.py"})
	if err := e.gpu.DeleteJob(context.Background(), id, jzMember); !errors.Is(err, gpu.ErrJobLive) {
		t.Fatalf("delete of a live job: %v", err)
	}
	if ok, err := e.jobs.DeleteJob(context.Background(), id); ok || err != nil {
		t.Fatalf("store deleted a live row: %v %v", ok, err)
	}
	_, _ = call(t, e.h, "DELETE", "/v1/gpu/jobs/"+id, "oidc:"+jzAdmin, "", nil)
}
