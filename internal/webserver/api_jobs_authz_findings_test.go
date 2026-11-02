package webserver

import (
	"context"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pfisterer/llm-management-api/internal/gpu"
	"github.com/pfisterer/llm-management-api/internal/workspace"
)

// Findings of the rights test of the batch-job API (todo job_submit, security_tests): each test asserts the behaviour wanted and fails today. They are skipped so that the suite stays green; JOB_AUTHZ_FINDINGS=1 runs them (go test ./internal/webserver -run Befund). Once a finding is fixed, remove the skip and keep the test as a regression test.

func befund(t *testing.T, what string) {
	t.Helper()
	if os.Getenv("JOB_AUTHZ_FINDINGS") == "" {
		t.Skip("Befund: " + what + " (JOB_AUTHZ_FINDINGS=1 zeigt ihn)")
	}
}

// Befund 1: check and submit are not atomic. Requests of the same person that arrive together all pass CheckJob (jobs_per_person, budget with reservations) before any of them is saved, so a script with parallel requests gets more jobs than jobs_per_person and more GPU hours than the budget. Here three at once into a project with one job per person: all three are created.
func TestBefundJobSubmitRace(t *testing.T) {
	ctx := context.Background()
	e := newJobEnv(t)
	p := jobProject("t-eins", "Ein Job", workspace.Member{Token: "user:" + jzMember})
	p.Limits.JobsPerPerson = 1
	p.Budgets = []workspace.Budget{{Unit: "gpu_hours", Window: "total", Amount: 4}}
	if err := e.store.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	// Hold every request at the project volume (after CheckJob, before the job is saved) until all three are there, at most a second (a fixed service lets only one through at a time).
	const n = 3
	var mu sync.Mutex
	arrived := 0
	all := make(chan struct{})
	e.kube.onPVC = func() {
		mu.Lock()
		arrived++
		if arrived == n {
			close(all)
		}
		mu.Unlock()
		select {
		case <-all:
		case <-time.After(time.Second):
		}
	}
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], _ = call(t, e.h, "POST", "/v1/gpu/jobs", "oidc:"+jzMember, "", SubmitJobRequest{Project: "t-eins", Command: "x"})
		}()
	}
	wg.Wait()
	live, _ := e.jobs.Jobs(ctx, gpu.JobFilter{Project: "t-eins", Live: true})
	if len(live) != 1 {
		t.Errorf("jobs_per_person 1 and a budget of 4 GPU hours (one job of 4 h), %d parallel requests: %d jobs live, answers %v", n, len(live), codes)
	}
}

// Befund 2: the refusal tells a foreign project from one that does not exist and names it. Anybody with access to workspaces can probe slugs (POST /v1/gpu/jobs) and learns which exist and their names ("Kein Mitglied im Projekt „…“" against „Projekt … gibt es nicht“).
func TestBefundJobForeignProjectDisclosed(t *testing.T) {
	befund(t, "Ablehnung beim Abschicken in ein fremdes Projekt verrät Existenz und Namen des Projekts")
	e := newJobEnv(t)
	_, foreign := call(t, e.h, "POST", "/v1/gpu/jobs", "oidc:"+jzMember, "", SubmitJobRequest{Project: "t-fremd", Command: "x"})
	_, unknown := call(t, e.h, "POST", "/v1/gpu/jobs", "oidc:"+jzMember, "", SubmitJobRequest{Project: "t-gibtsnicht", Command: "x"})
	if strings.Contains(string(foreign), "Geheimes Forschungsprojekt") {
		t.Errorf("the refusal names the foreign project: %s", foreign)
	}
	if strings.ReplaceAll(string(foreign), "t-fremd", "X") != strings.ReplaceAll(string(unknown), "t-gibtsnicht", "X") {
		t.Errorf("foreign and unknown project answer differently:\n%s\n%s", foreign, unknown)
	}
}

// Befund 3: once a person is in no active project any more (removed, project ended), requireWorkspace refuses the whole /v1/gpu: their job that still waits or runs can neither be seen nor cancelled by them, it keeps using the project's GPU hours. Their own jobs should stay visible and cancellable.
func TestBefundJobOwnJobAfterRemoval(t *testing.T) {
	befund(t, "nach Entfernen aus dem (einzigen) Projekt kann die Person ihren laufenden Job weder sehen noch abbrechen")
	ctx := context.Background()
	e := newJobEnv(t)
	id := e.submit(t, jzOutsider, SubmitJobRequest{Project: "t-fremd", Command: "sleep 1d"})
	p, _ := e.store.Get(ctx, "t-fremd")
	p.Members = nil
	if err := e.store.Update(ctx, &p); err != nil {
		t.Fatal(err)
	}
	if code, body := call(t, e.h, "GET", "/v1/gpu/jobs/"+id, "oidc:"+jzOutsider, "", nil); code != http.StatusOK {
		t.Errorf("own job after removal: want 200, got %d %s", code, body)
	}
	if code, body := call(t, e.h, "DELETE", "/v1/gpu/jobs/"+id, "oidc:"+jzOutsider, "", nil); code != http.StatusOK {
		t.Errorf("cancel own job after removal: want 200, got %d %s", code, body)
	}
}
