package webserver

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pfisterer/cloud-self-service-golib/token"
	"github.com/pfisterer/llm-management-api/internal/gpu"
	"github.com/pfisterer/llm-management-api/internal/roleprovider"
	"github.com/pfisterer/llm-management-api/internal/usage"
	"github.com/pfisterer/llm-management-api/internal/workspace"
	"go.uber.org/zap"
)

// Rights matrix of the batch-job API /v1/gpu/jobs (docs/architektur.md "Batch-Jobs", todo job_submit "Test der API-Rechte"): for every operation, who may — member (own jobs, others' jobs in the same project), reader, manager, outsider (other project, no project), fleet admin, platform admin, acting on behalf (X-Act-As), API tokens (writable, read-only). The GPU cluster is a fake Kubernetes API over TLS, the rest is the real server.

// jobKube is a fake Kubernetes API for the job routes: it keeps what was created, answers a project volume as existing and big enough, and lists no pods.
type jobKube struct {
	mu      sync.Mutex
	objects map[string]map[string]any
	// onPVC runs on every read of a project volume, before the answer (the race test holds requests there).
	onPVC func()
	// servers are the running workspace servers by pod address: whose they are and their project (the workspace caller's lookup).
	servers map[string][2]string
}

func (k *jobKube) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	p := r.URL.Path
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/pods") && strings.Contains(r.URL.Query().Get("labelSelector"), "singleuser-server"):
		ip, _ := strings.CutPrefix(strings.Split(r.URL.Query().Get("fieldSelector"), ",")[0], "status.podIP=")
		items := []map[string]any{}
		if who, ok := k.servers[ip]; ok {
			items = append(items, map[string]any{
				"metadata": map[string]any{"name": "jupyter-x", "labels": map[string]any{"component": "singleuser-server", "dhbw.cloud/project": who[1], "dhbw.cloud/profile": "cpu"}, "annotations": map[string]any{"hub.jupyter.org/username": who[0]}},
				"spec":     map[string]any{"containers": []any{map[string]any{"name": "notebook", "image": "reg/envs/x:1"}}},
				"status":   map[string]any{"phase": "Running", "podIP": ip},
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
		return
	case r.Method == http.MethodGet && strings.Contains(p, "/pods/dhbw-files-"):
		_, _ = fmt.Fprint(w, `{"status":{"phase":"Running","podIP":"127.0.0.1"}}`)
		return
	case r.Method == http.MethodGet && strings.Contains(p, "/persistentvolumeclaims/ws-"):
		if k.onPVC != nil {
			k.onPVC()
		}
		_, _ = fmt.Fprint(w, `{"spec":{"resources":{"requests":{"storage":"100Gi"}}}}`)
		return
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/pods"):
		_, _ = fmt.Fprint(w, `{"items":[]}`)
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		if o, ok := k.objects[p]; ok {
			_ = json.NewEncoder(w).Encode(o)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprint(w, `{}`)
	case http.MethodPost:
		var o map[string]any
		_ = json.NewDecoder(r.Body).Decode(&o)
		name, _ := o["metadata"].(map[string]any)["name"].(string)
		k.objects[p+"/"+name] = o
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprint(w, `{}`)
	case http.MethodDelete:
		delete(k.objects, p)
		_, _ = fmt.Fprint(w, `{}`)
	default:
		_, _ = fmt.Fprint(w, `{}`)
	}
}

// jobObject is the batch/v1 Job created for a job id, as JSON.
func (k *jobKube) jobObject(id string) string {
	k.mu.Lock()
	defer k.mu.Unlock()
	b, _ := json.Marshal(k.objects["/apis/batch/v1/namespaces/jupyterhub/jobs/"+id])
	return string(b)
}

// People of the matrix. Members are listed by user token, the manager through a group.
const (
	jzMember   = "member@dhbw.de"   // member of t-kurs
	jzOther    = "other@dhbw.de"    // member of t-kurs too
	jzReader   = "reader@dhbw.de"   // reader of t-kurs
	jzManager  = "manager@dhbw.de"  // manager of t-kurs through group:kurs-leitung
	jzOutsider = "outsider@dhbw.de" // member of t-fremd only
	jzNobody   = "nobody@dhbw.de"   // no project
	jzFleet    = "fleet@dhbw.de"    // fleet admin, no project
	jzAdmin    = "admin@dhbw.de"    // platform admin (ADMIN_TOKENS), no project
)

type jobEnv struct {
	h      http.Handler
	api    *Server
	opts   Options
	gpu    *gpu.Service
	kube   *jobKube
	ws     *workspace.Service
	store  *workspace.MemoryStore
	jobs   *gpu.MemoryJobStore
	tokens *token.Service
}

// jobProject is an active project with storage, one GPU class and batch jobs.
func jobProject(slug, name string, members ...workspace.Member) *workspace.Project {
	return &workspace.Project{Slug: slug, Name: name, Members: members, Profiles: []string{"cpu"}, GPUClasses: []string{"general-small"}, ProjectImage: "reg/envs/" + slug + ":1",
		StorageGB: 10, Limits: workspace.Limits{JobsPerPerson: 10, JobMaxHours: 4}}
}

func newJobEnv(t *testing.T, opts ...func(*Options, *gpu.Config)) *jobEnv {
	t.Helper()
	ctx := context.Background()
	kube := &jobKube{objects: map[string]map[string]any{}}
	srv := httptest.NewUnstartedServer(kube)
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	cfg := gpu.Config{KubeAPIURL: srv.URL, KubeCA: ca, Registry: "reg", Jobs: gpu.JobConfig{InitImage: "reg/platform/base-python@sha256:abc"}}
	var o Options
	for _, f := range opts {
		f(&o, &cfg)
	}
	g, err := gpu.New(cfg, gpu.NewMemoryStore(), zap.NewNop().Sugar())
	if err != nil {
		t.Fatal(err)
	}
	jobs := gpu.NewMemoryJobStore()
	g.SetJobStore(jobs)

	store := workspace.NewMemoryStore()
	ws := workspace.NewService(store, workspace.Config{}, func(context.Context) ([]workspace.Running, error) { return nil, nil })
	ws.SetUsage(usage.NewMemoryStore())
	ws.SetJobReservations(g.JobReservations)
	for _, p := range []*workspace.Project{
		jobProject("t-kurs", "Kurs ML",
			workspace.Member{Token: "user:" + jzMember, Role: workspace.RoleMember}, workspace.Member{Token: "user:" + jzOther},
			workspace.Member{Token: "user:" + jzReader, Role: workspace.RoleReader}, workspace.Member{Token: "group:kurs-leitung", Role: workspace.RoleManager}),
		jobProject("t-fremd", "Geheimes Forschungsprojekt", workspace.Member{Token: "user:" + jzOutsider}),
	} {
		if err := store.Create(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	roles := &roleprovider.Mock{Memberships: map[string][]string{jzManager: {"group:kurs-leitung"}}}
	tokens := token.NewService(APITokenPrefix, token.NewMemoryStore())
	o.Verifier, o.Access, o.Log, o.GPU, o.Workspaces = fakeVerifier{}, accessFor(t, roles, []string{"user:" + jzAdmin}, []string{"user:" + jzFleet}), zap.NewNop().Sugar(), g, ws
	o.Tokens = TokenOptions{Service: tokens, TTL: token.TTLPolicy{Default: time.Hour, Max: time.Hour}, MaxPerUser: 10}
	if o.Quota.Projects == nil {
		o.Quota = testQuota
		o.Quota.Projects = ws
	}
	srvAPI := New(o)
	h := srvAPI.Router()
	return &jobEnv{h: h, api: srvAPI, opts: o, gpu: g, kube: kube, ws: ws, store: store, jobs: jobs, tokens: tokens}
}

// submit sends a job as a signed-in person and returns its id; it fails the test unless created.
func (e *jobEnv) submit(t *testing.T, email string, req SubmitJobRequest) string {
	t.Helper()
	code, body := call(t, e.h, "POST", "/v1/gpu/jobs", "oidc:"+email, "", req)
	var j gpu.Job
	if _ = json.Unmarshal(body, &j); code != http.StatusCreated || !gpu.JobID(j.ID) {
		t.Fatalf("submit as %s: %d %s", email, code, body)
	}
	return j.ID
}

func (e *jobEnv) token(t *testing.T, email string, readOnly bool) string {
	t.Helper()
	tok, err := e.tokens.Issue(context.Background(), email, token.IssueOptions{TTL: time.Hour, ReadOnly: readOnly})
	if err != nil {
		t.Fatal(err)
	}
	return tok.Secret
}

// jobActorCase is somebody calling: a bearer (signed in or API token) and an optional X-Act-As.
type jobActorCase struct {
	name, bearer, actAs string
}

func (e *jobEnv) actors(t *testing.T) []jobActorCase {
	return []jobActorCase{
		{"member (owner of jobA)", "oidc:" + jzMember, ""},
		{"other member", "oidc:" + jzOther, ""},
		{"reader", "oidc:" + jzReader, ""},
		{"manager (group)", "oidc:" + jzManager, ""},
		{"outsider (other project)", "oidc:" + jzOutsider, ""},
		{"no project", "oidc:" + jzNobody, ""},
		{"fleet admin, no project", "oidc:" + jzFleet, ""},
		{"platform admin", "oidc:" + jzAdmin, ""},
		{"admin as member", "oidc:" + jzAdmin, jzMember},
		{"admin as outsider", "oidc:" + jzAdmin, jzOutsider},
		{"member's token", e.token(t, jzMember, false), ""},
		{"member's read-only token", e.token(t, jzMember, true), ""},
		{"manager's read-only token", e.token(t, jzManager, true), ""},
		{"admin's token", e.token(t, jzAdmin, false), ""},
	}
}

// The matrix of reading: details, log and the project's list. Invisible jobs are 404 like unknown ones (no oracle for ids); people without any project read like everybody else (their own jobs only, so that one removed from their only project still sees the jobs they left running), the rest of /v1/gpu is 403 for them.
func TestJobRightsRead(t *testing.T) {
	e := newJobEnv(t)
	jobA := e.submit(t, jzMember, SubmitJobRequest{Project: "t-kurs", Command: "python a.py"})
	jobO := e.submit(t, jzOther, SubmitJobRequest{Project: "t-kurs", Command: "python o.py"})
	jobR := e.submit(t, jzReader, SubmitJobRequest{Project: "t-kurs", Command: "python r.py"})
	jobB := e.submit(t, jzOutsider, SubmitJobRequest{Project: "t-fremd", Command: "python b.py"})
	_ = jobR

	const ok, hidden, forbidden = http.StatusOK, http.StatusNotFound, http.StatusForbidden
	// want: get/log of jobA (member's), jobO (other member's), jobB (other project's), an unknown id; and how many jobs ?project=t-kurs lists (-1 = 403).
	type want struct{ a, o, b, unknown, listKurs, listMine int }
	matrix := map[string]want{
		"member (owner of jobA)":    {ok, hidden, hidden, hidden, 1, 1},
		"other member":              {hidden, ok, hidden, hidden, 1, 1},
		"reader":                    {hidden, hidden, hidden, hidden, 1, 1},
		"manager (group)":           {ok, ok, hidden, hidden, 3, 0},
		"outsider (other project)":  {hidden, hidden, ok, hidden, 0, 1},
		"no project":                {hidden, hidden, hidden, hidden, 0, 0},
		"fleet admin, no project":   {hidden, hidden, hidden, hidden, 0, 0},
		"platform admin":            {ok, ok, ok, hidden, 3, 0},
		"admin as member":           {ok, hidden, hidden, hidden, 1, 1},
		"admin as outsider":         {hidden, hidden, ok, hidden, 0, 1},
		"member's token":            {ok, hidden, hidden, hidden, 1, 1},
		"member's read-only token":  {ok, hidden, hidden, hidden, 1, 1},
		"manager's read-only token": {ok, ok, hidden, hidden, 3, 0},
		"admin's token":             {ok, ok, ok, hidden, 3, 0},
	}
	for _, a := range e.actors(t) {
		w, found := matrix[a.name]
		if !found {
			t.Fatalf("no expectation for %s", a.name)
		}
		for _, c := range []struct {
			id   string
			want int
		}{{jobA, w.a}, {jobO, w.o}, {jobB, w.b}, {"j-0000000000", w.unknown}} {
			if code, body := call(t, e.h, "GET", "/v1/gpu/jobs/"+c.id, a.bearer, a.actAs, nil); code != c.want {
				t.Errorf("%s: GET job %s: want %d, got %d %s", a.name, c.id, c.want, code, body)
			}
			if code, body := call(t, e.h, "GET", "/v1/gpu/jobs/"+c.id+"/log", a.bearer, a.actAs, nil); code != c.want {
				t.Errorf("%s: GET log of %s: want %d, got %d %s", a.name, c.id, c.want, code, body)
			}
		}
		for _, l := range []struct {
			path string
			want int
		}{{"/v1/gpu/jobs?project=t-kurs", w.listKurs}, {"/v1/gpu/jobs", w.listMine}} {
			code, body := call(t, e.h, "GET", l.path, a.bearer, a.actAs, nil)
			if l.want < 0 {
				if code != forbidden {
					t.Errorf("%s: list %s: want 403, got %d %s", a.name, l.path, code, body)
				}
				continue
			}
			var jobs []gpu.Job
			if _ = json.Unmarshal(body, &jobs); code != ok || len(jobs) != l.want {
				t.Errorf("%s: list %s: want %d jobs, got %d %s", a.name, l.path, l.want, code, body)
			}
		}
	}
}

// The matrix of writing: submitting into the own and a foreign project, cancelling the own and others' jobs. Read-only tokens write nothing; membership, not the platform role, lets one submit (people without any project not at all); acting on behalf has exactly the person's rights.
func TestJobRightsWrite(t *testing.T) {
	const created, ok, hidden, refused, forbidden = http.StatusCreated, http.StatusOK, http.StatusNotFound, http.StatusConflict, http.StatusForbidden
	type want struct{ submitKurs, submitFremd, cancelA, cancelO, cancelB int }
	matrix := map[string]want{
		"member (owner of jobA)":    {created, refused, ok, hidden, hidden},
		"other member":              {created, refused, hidden, ok, hidden},
		"reader":                    {created, refused, hidden, hidden, hidden},
		"manager (group)":           {created, refused, ok, ok, hidden},
		"outsider (other project)":  {refused, created, hidden, hidden, ok},
		"no project":                {forbidden, forbidden, hidden, hidden, hidden},
		"fleet admin, no project":   {forbidden, forbidden, hidden, hidden, hidden},
		"platform admin":            {refused, refused, ok, ok, ok},
		"admin as member":           {created, refused, ok, hidden, hidden},
		"admin as outsider":         {refused, created, hidden, hidden, ok},
		"member's token":            {created, refused, ok, hidden, hidden},
		"member's read-only token":  {forbidden, forbidden, forbidden, forbidden, forbidden},
		"manager's read-only token": {forbidden, forbidden, forbidden, forbidden, forbidden},
		"admin's token":             {refused, refused, ok, ok, ok},
	}
	e := newJobEnv(t)
	for _, a := range e.actors(t) {
		w, found := matrix[a.name]
		if !found {
			t.Fatalf("no expectation for %s", a.name)
		}
		// Fresh live jobs for every caller, so that a cancel shows its effect.
		jobA := e.submit(t, jzMember, SubmitJobRequest{Project: "t-kurs", Command: "a"})
		jobO := e.submit(t, jzOther, SubmitJobRequest{Project: "t-kurs", Command: "o"})
		jobB := e.submit(t, jzOutsider, SubmitJobRequest{Project: "t-fremd", Command: "b"})

		for _, s := range []struct {
			project string
			want    int
		}{{"t-kurs", w.submitKurs}, {"t-fremd", w.submitFremd}} {
			code, body := call(t, e.h, "POST", "/v1/gpu/jobs", a.bearer, a.actAs, SubmitJobRequest{Project: s.project, Command: "true"})
			if code != s.want {
				t.Errorf("%s: submit into %s: want %d, got %d %s", a.name, s.project, s.want, code, body)
			}
			if code == created {
				var j gpu.Job
				_ = json.Unmarshal(body, &j)
				// The job is the person's acted for (or the caller's), never the admin's behind X-Act-As.
				who := a.actAs
				if who == "" {
					who = map[string]string{"reader": jzReader, "manager (group)": jzManager, "other member": jzOther, "outsider (other project)": jzOutsider}[a.name]
					if who == "" {
						who = jzMember
					}
				}
				if j.Person != who {
					t.Errorf("%s: job submitted for %q, want %q", a.name, j.Person, who)
				}
			}
		}
		for _, c := range []struct {
			id   string
			want int
		}{{jobA, w.cancelA}, {jobO, w.cancelO}, {jobB, w.cancelB}} {
			code, body := call(t, e.h, "DELETE", "/v1/gpu/jobs/"+c.id, a.bearer, a.actAs, nil)
			if code != c.want {
				t.Errorf("%s: cancel %s: want %d, got %d %s", a.name, c.id, c.want, code, body)
			}
			r, _ := e.jobs.Job(context.Background(), c.id)
			if cancelled := r.State == gpu.JobCancelled; cancelled != (c.want == ok) {
				t.Errorf("%s: cancel %s answered %d but the job is %s", a.name, c.id, code, r.State)
			}
		}
		// Clean up the remaining live jobs, so that jobs_per_person never decides here.
		for _, id := range []string{jobA, jobO, jobB} {
			_, _ = call(t, e.h, "DELETE", "/v1/gpu/jobs/"+id, "oidc:"+jzAdmin, "", nil)
		}
		live, _ := e.jobs.Jobs(context.Background(), gpu.JobFilter{Live: true})
		for _, r := range live {
			_, _ = call(t, e.h, "DELETE", "/v1/gpu/jobs/"+r.ID, "oidc:"+jzAdmin, "", nil)
		}
	}
}

// An API token cannot act on somebody's behalf, also not an admin's.
func TestJobActAsWithTokenRefused(t *testing.T) {
	e := newJobEnv(t)
	tok := e.token(t, jzAdmin, false)
	if code, body := call(t, e.h, "POST", "/v1/gpu/jobs", tok, jzMember, SubmitJobRequest{Project: "t-kurs", Command: "true"}); code != http.StatusForbidden {
		t.Errorf("admin token with X-Act-As: want 403, got %d %s", code, body)
	}
	if code, body := call(t, e.h, "GET", "/v1/gpu/jobs", "oidc:"+jzManager, jzMember, nil); code != http.StatusForbidden {
		t.Errorf("a manager (no admin) with X-Act-As: want 403, got %d %s", code, body)
	}
}

// What a project allows decides, whoever submits: no batch jobs, expired, LLM only, no storage, a class it does not have, longer than its maximum, more jobs than jobs_per_person, more than its GPU-hours budget with what live jobs reserve.
func TestJobProjectRules(t *testing.T) {
	ctx := context.Background()
	e := newJobEnv(t)
	past := time.Now().AddDate(0, 0, -3)
	m := workspace.Member{Token: "user:" + jzMember}
	for _, p := range []*workspace.Project{
		func() *workspace.Project {
			p := jobProject("t-nojobs", "Ohne Jobs", m)
			p.Limits.JobsPerPerson = 0
			return p
		}(),
		func() *workspace.Project { p := jobProject("t-alt", "Abgelaufen", m); p.EndsAt = &past; return p }(),
		func() *workspace.Project { p := jobProject("t-llm", "Nur LLM", m); p.NoWorkspaces = true; return p }(),
		func() *workspace.Project { p := jobProject("t-ohne", "Ohne Speicher", m); p.StorageGB = 0; return p }(),
		func() *workspace.Project {
			p := jobProject("t-eins", "Ein Job", m)
			p.Limits.JobsPerPerson = 1
			return p
		}(),
		func() *workspace.Project {
			p := jobProject("t-budget", "Budget", m)
			p.Budgets = []workspace.Budget{{Unit: "gpu_hours", Window: "total", Amount: 5}}
			return p
		}(),
	} {
		if err := e.store.Create(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		name string
		req  SubmitJobRequest
		want int
	}{
		{"no batch jobs", SubmitJobRequest{Project: "t-nojobs", Command: "x"}, http.StatusConflict},
		{"expired", SubmitJobRequest{Project: "t-alt", Command: "x"}, http.StatusConflict},
		{"LLM only", SubmitJobRequest{Project: "t-llm", Command: "x"}, http.StatusConflict},
		{"no storage", SubmitJobRequest{Project: "t-ohne", Command: "x"}, http.StatusConflict},
		{"unknown project", SubmitJobRequest{Project: "t-gibtsnicht", Command: "x"}, http.StatusConflict},
		{"other class", SubmitJobRequest{Project: "t-kurs", GPUClass: "general-large", Command: "x"}, http.StatusConflict},
		{"too long", SubmitJobRequest{Project: "t-kurs", MaxHours: 5, Command: "x"}, http.StatusConflict},
		{"negative hours", SubmitJobRequest{Project: "t-kurs", MaxHours: -1, Command: "x"}, http.StatusConflict},
		{"first of one", SubmitJobRequest{Project: "t-eins", Command: "x"}, http.StatusCreated},
		{"second of one", SubmitJobRequest{Project: "t-eins", Command: "x"}, http.StatusConflict},
		{"4 of 5 budget hours", SubmitJobRequest{Project: "t-budget", Command: "x"}, http.StatusCreated},
		{"4 more with 4 reserved", SubmitJobRequest{Project: "t-budget", Command: "x"}, http.StatusConflict},
		{"1 more with 4 reserved", SubmitJobRequest{Project: "t-budget", MaxHours: 1, Command: "x"}, http.StatusCreated},
	} {
		if code, body := call(t, e.h, "POST", "/v1/gpu/jobs", "oidc:"+jzMember, "", c.req); code != c.want {
			t.Errorf("%s: want %d, got %d %s", c.name, c.want, code, body)
		}
	}
	// The reservation is the project's: another member is refused too (budget for the whole project).
	if err := e.store.Update(ctx, func() *workspace.Project {
		p, _ := e.store.Get(ctx, "t-budget")
		p.Members = append(p.Members, workspace.Member{Token: "user:" + jzOther})
		return &p
	}()); err != nil {
		t.Fatal(err)
	}
	if code, body := call(t, e.h, "POST", "/v1/gpu/jobs", "oidc:"+jzOther, "", SubmitJobRequest{Project: "t-budget", MaxHours: 1, Command: "x"}); code != http.StatusConflict {
		t.Errorf("other member over the project's budget: want 409, got %d %s", code, body)
	}
}

// Paths stay in the home and nothing of a request reaches the shell unquoted except the command itself (which is the person's own, in their own container): working_dir, notebook and name are checked or quoted; a reader's job mounts the shared folder read-only.
func TestJobInputsAndMounts(t *testing.T) {
	e := newJobEnv(t)
	for _, c := range []SubmitJobRequest{
		{Project: "t-kurs", Command: "x", WorkingDir: "../other"},
		{Project: "t-kurs", Command: "x", WorkingDir: "/etc"},
		{Project: "t-kurs", Command: "x", WorkingDir: "/home/jovyan/../../etc"},
		{Project: "t-kurs", Notebook: "../homes/x/a.ipynb"},
		{Project: "t-kurs", Notebook: "/home/jovyan/../other/a.ipynb"},
		{Project: "t-kurs", Notebook: "a.py"},
		{Project: "t-kurs", Command: "x", Name: "../x"},
		{Project: "t-kurs", Command: "x", Name: "a;rm -rf ~"},
		{Project: "t-kurs", Command: "x", Name: "a/b"},
		{Project: "t-kurs", Command: strings.Repeat("x", 8193)},
		{Project: "t-kurs", Command: "x", Notebook: "a.ipynb"},
	} {
		if code, body := call(t, e.h, "POST", "/v1/gpu/jobs", "oidc:"+jzMember, "", c); code != http.StatusBadRequest {
			t.Errorf("%+v: want 400, got %d %s", c, code, body)
		}
	}
	// A notebook path with quotes and command substitution stays one quoted argument of papermill.
	id := e.submit(t, jzMember, SubmitJobRequest{Project: "t-kurs", Notebook: "kurs/a'$(id)`x`.ipynb", Name: "nb"})
	obj := e.kube.jobObject(id)
	if !strings.Contains(obj, `papermill --log-output '/home/jovyan/kurs/a'\\''$(id)`+"`x`"+`.ipynb'`) {
		t.Errorf("notebook not quoted: %s", obj)
	}
	// The working directory goes as an environment variable, never into the script.
	id = e.submit(t, jzMember, SubmitJobRequest{Project: "t-kurs", Command: "x", WorkingDir: `a"; touch /tmp/pwn; "b`})
	if obj := e.kube.jobObject(id); !strings.Contains(obj, `"name":"DHBW_JOB_WORKDIR","value":"a\"; touch /tmp/pwn; \"b"`) {
		t.Errorf("working dir: %s", obj)
	}

	// Mounts: the person's own home folder in the project's volume; the shared folder read-only for a reader only.
	memberJob := e.kube.jobObject(e.submit(t, jzMember, SubmitJobRequest{Project: "t-kurs", Command: "x"}))
	readerJob := e.kube.jobObject(e.submit(t, jzReader, SubmitJobRequest{Project: "t-kurs", Command: "x"}))
	if !strings.Contains(memberJob, `"subPath":"`+"homes/"+gpu.HubUserSlug(jzMember)+`"`) || !strings.Contains(readerJob, `"subPath":"`+"homes/"+gpu.HubUserSlug(jzReader)+`"`) {
		t.Errorf("home mounts:\n%s\n%s", memberJob, readerJob)
	}
	if strings.Contains(memberJob, `"readOnly":true`) {
		t.Errorf("a member's shared folder is read-only: %s", memberJob)
	}
	if !strings.Contains(readerJob, `"mountPath":"/home/jovyan/project-shared","name":"home","readOnly":true`) {
		t.Errorf("a reader's shared folder is writable: %s", readerJob)
	}
	for _, o := range []string{memberJob, readerJob} {
		for _, want := range []string{`"automountServiceAccountToken":false`, `"allowPrivilegeEscalation":false`, `"runAsUser":1000`, `"claimName":"ws-t-kurs"`} {
			if !strings.Contains(o, want) {
				t.Errorf("job lacks %s: %s", want, o)
			}
		}
	}
	// Acting on behalf, the job runs in the person's home, not the admin's.
	code, body := call(t, e.h, "POST", "/v1/gpu/jobs", "oidc:"+jzAdmin, jzReader, SubmitJobRequest{Project: "t-kurs", Command: "x"})
	var j gpu.Job
	if _ = json.Unmarshal(body, &j); code != http.StatusCreated {
		t.Fatalf("admin as reader: %d %s", code, body)
	}
	if o := e.kube.jobObject(j.ID); !strings.Contains(o, "homes/"+gpu.HubUserSlug(jzReader)) || !strings.Contains(o, `"readOnly":true`) {
		t.Errorf("job on behalf of a reader: %s", o)
	}
}
