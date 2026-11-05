package webserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pfisterer/llm-management-api/internal/gpu"
	"github.com/pfisterer/llm-management-api/internal/workspace"
	"go.uber.org/zap"
)

// fakeHubWhoami answers JupyterHub's /hub/api/user for the tokens "t-<e-mail>".
func fakeHubWhoami(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		email, ok := strings.CutPrefix(r.Header.Get("Authorization"), "token t-")
		if r.URL.Path != "/hub/api/user" || !ok {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"name": email})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// workspaceEnv is the job environment with the hub listener: workspaces (server pods by address) that call the main API's job routes with their hub token.
func workspaceEnv(t *testing.T) (*jobEnv, http.Handler) {
	hub := fakeHubWhoami(t)
	e := newJobEnv(t, func(_ *Options, cfg *gpu.Config) { cfg.JupyterURL = hub.URL })
	e.kube.servers = map[string][2]string{"10.42.0.10": {jzMember, "t-kurs"}, "10.42.0.11": {jzAdmin, "t-kurs"}, "10.42.0.12": {jzManager, "t-kurs"}}
	r := GPUHubRouter(GPUHubOptions{Access: e.opts.Access, Workspaces: e.ws, Token: "shared", Kernels: e.gpu, API: e.api, Log: zap.NewNop().Sugar()}, true)
	return e, r
}

// fromWorkspace calls the hub listener as the server pod at ip with the authorization header as given.
func fromWorkspace(t *testing.T, h http.Handler, method, path, ip, auth string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rd)
	req.RemoteAddr = ip + ":40000"
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Act-As", jzOutsider)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// A workspace reaches the job routes with its own hub token (todo job_submit "Anmeldung im Workspace"): as its person, in that person's projects, never as admin; only from the server pod the token belongs to, and nothing else of the main API.
func TestWorkspaceJobRights(t *testing.T) {
	e, h := workspaceEnv(t)
	jobO := e.submit(t, jzOther, SubmitJobRequest{Project: "t-kurs", Command: "o"})
	jobB := e.submit(t, jzOutsider, SubmitJobRequest{Project: "t-fremd", Command: "b"})

	member := "token t-" + jzMember
	code, body := fromWorkspace(t, h, "POST", "/v1/gpu/jobs", "10.42.0.10", member, SubmitJobRequest{Project: "t-kurs", Command: "python a.py"})
	var j gpu.Job
	if _ = json.Unmarshal(body, &j); code != http.StatusCreated || j.Person != jzMember {
		t.Fatalf("submit from the member's workspace: %d %s", code, body)
	}
	for _, c := range []struct {
		name, method, path, ip, auth string
		body                         any
		want                         int
	}{
		{"own job", "GET", "/v1/gpu/jobs/" + j.ID, "10.42.0.10", member, nil, http.StatusOK},
		{"own log", "GET", "/v1/gpu/jobs/" + j.ID + "/log", "10.42.0.10", member, nil, http.StatusOK},
		{"other member's job", "GET", "/v1/gpu/jobs/" + jobO, "10.42.0.10", member, nil, http.StatusNotFound},
		{"start options", "GET", "/v1/gpu/start-options", "10.42.0.10", member, nil, http.StatusOK},
		{"foreign project", "POST", "/v1/gpu/jobs", "10.42.0.10", member, SubmitJobRequest{Project: "t-fremd", Command: "x"}, http.StatusConflict},
		// X-Act-As is ignored here: the person stays the server's.
		{"cancel the outsider's job", "DELETE", "/v1/gpu/jobs/" + jobB, "10.42.0.10", member, nil, http.StatusNotFound},
		// An admin's workspace has the admin's project rights, not the platform role.
		{"admin's workspace, other's job", "GET", "/v1/gpu/jobs/" + jobO, "10.42.0.11", "token t-" + jzAdmin, nil, http.StatusNotFound},
		{"admin's workspace, foreign project's job", "GET", "/v1/gpu/jobs/" + jobB, "10.42.0.11", "token t-" + jzAdmin, nil, http.StatusNotFound},
		{"admin's workspace, cancel", "DELETE", "/v1/gpu/jobs/" + jobB, "10.42.0.11", "token t-" + jzAdmin, nil, http.StatusNotFound},
		// A manager's workspace sees the project's jobs, as the manager does.
		{"manager's workspace", "GET", "/v1/gpu/jobs/" + jobO, "10.42.0.12", "token t-" + jzManager, nil, http.StatusOK},
		// The token of another person, or from another address, or none.
		{"member's token from the manager's server", "GET", "/v1/gpu/jobs/" + j.ID, "10.42.0.12", member, nil, http.StatusUnauthorized},
		{"member's token from an unknown address", "GET", "/v1/gpu/jobs/" + j.ID, "10.42.0.99", member, nil, http.StatusUnauthorized},
		{"unknown token", "GET", "/v1/gpu/jobs", "10.42.0.10", "token nope", nil, http.StatusUnauthorized},
		{"bearer instead of token", "GET", "/v1/gpu/jobs", "10.42.0.10", "Bearer t-" + jzMember, nil, http.StatusUnauthorized},
		{"shared hub token", "GET", "/v1/gpu/jobs", "10.42.0.10", "Bearer shared", nil, http.StatusUnauthorized},
		{"no token", "GET", "/v1/gpu/jobs", "10.42.0.10", "", nil, http.StatusUnauthorized},
		// Nothing but the job routes and the start options.
		{"keys", "GET", "/v1/keys", "10.42.0.10", member, nil, http.StatusNotFound},
		{"tokens", "POST", "/v1/tokens", "10.42.0.10", member, map[string]string{"name": "x"}, http.StatusNotFound},
		{"projects", "GET", "/v1/workspaces/projects", "10.42.0.10", member, nil, http.StatusNotFound},
		{"fleet", "GET", "/v1/gpu/fleet", "10.42.0.10", member, nil, http.StatusNotFound},
	} {
		if code, body := fromWorkspace(t, h, c.method, c.path, c.ip, c.auth, c.body); code != c.want {
			t.Errorf("%s: want %d, got %d %s", c.name, c.want, code, body)
		}
	}
	if r, _ := e.jobs.Job(context.Background(), jobB); r.State == gpu.JobCancelled {
		t.Error("the outsider's job was cancelled from another workspace")
	}
}

// Batch jobs get the person's LLM key by the rule of their workspaces (todo job_submit "LLM-Key im Job"): from a Secret owned by the Job, never in the Job itself; llm_project chooses another or none, a chosen one without key is refused.
func TestJobLLMKey(t *testing.T) {
	ctx := context.Background()
	f, lite := newFakeLite(t)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"id":"agent"},{"id":"chat-fast"},{"id":"embedding"}]}`)
	}))
	t.Cleanup(gateway.Close)
	e := newJobEnv(t, func(o *Options, _ *gpu.Config) {
		o.Keys, o.MasterKey, o.APIURL, o.Gateway = testKeys(lite.URL), "master", "https://api.llm.example/v1", gateway.URL
	})
	p, _ := e.store.Get(ctx, "t-kurs")
	p.LLMMTokens = 50
	if err := e.store.Update(ctx, &p); err != nil {
		t.Fatal(err)
	}
	secretOf := func(id string) map[string]any {
		e.kube.mu.Lock()
		defer e.kube.mu.Unlock()
		return e.kube.objects["/api/v1/namespaces/jupyterhub/secrets/"+id+"-llm"]
	}
	submit := func(who string, req SubmitJobRequest) (int, gpu.Job, []byte) {
		code, body := call(t, e.h, "POST", "/v1/gpu/jobs", "oidc:"+who, "", req)
		var j gpu.Job
		_ = json.Unmarshal(body, &j)
		return code, j, body
	}

	code, j, body := submit(jzMember, SubmitJobRequest{Project: "t-kurs", Command: "python a.py"})
	if code != http.StatusCreated || j.LLMProject != "t-kurs" {
		t.Fatalf("job with the project's LLM: %d %s", code, body)
	}
	sec := secretOf(j.ID)
	key, _ := sec["stringData"].(map[string]any)["OPENAI_API_KEY"].(string)
	if key == "" || f.keyBy(key) == nil || f.keyBy(key)["team_id"] != "t-kurs" {
		t.Fatalf("secret: %v", sec)
	}
	if owner := fmt.Sprint(sec["metadata"].(map[string]any)["ownerReferences"]); owner != "<nil>" && !strings.Contains(owner, j.ID) {
		t.Errorf("secret owner: %s", owner)
	}
	obj := e.kube.jobObject(j.ID)
	if strings.Contains(obj, key) {
		t.Errorf("the key is in the Job: %s", obj)
	}
	for _, want := range []string{`"name":"OPENAI_API_KEY","valueFrom":{"secretKeyRef":{"key":"OPENAI_API_KEY","name":"` + j.ID + `-llm"}}`, `"name":"OPENAI_BASE_URL","value":"https://api.llm.example/v1"`, `"name":"ANTHROPIC_AUTH_TOKEN","valueFrom":{"secretKeyRef":{"key":"OPENAI_API_KEY","name":"` + j.ID + `-llm"}}`, `"name":"ANTHROPIC_BASE_URL","value":"https://api.llm.example"`, `"name":"ANTHROPIC_MODEL","value":"agent"`, `"name":"ANTHROPIC_DEFAULT_HAIKU_MODEL","value":"chat-fast"`, `"name":"CLAUDE_CODE_MAX_CONTEXT_TOKENS","value":"32768"`, `"terminationGracePeriodSeconds":120`, `"name":"DHBW_JOB_GRACE_SECONDS","value":"120"`} {
		if !strings.Contains(obj, want) {
			t.Errorf("job lacks %s: %s", want, obj)
		}
	}
	if strings.Contains(string(body), key) {
		t.Errorf("the key is in the answer: %s", body)
	}

	// Without LLM, chosen: no Secret, no variable.
	if code, j, body := submit(jzMember, SubmitJobRequest{Project: "t-kurs", Command: "x", LLMProject: "none"}); code != http.StatusCreated || j.LLMProject != "" || secretOf(j.ID) != nil || strings.Contains(e.kube.jobObject(j.ID), "OPENAI_API_KEY") || strings.Contains(e.kube.jobObject(j.ID), "ANTHROPIC") {
		t.Errorf("without LLM: %d %s", code, body)
	}
	// Another person's LLM project, chosen: refused. Left to the project, a person without LLM project gets a job without key.
	if code, _, body := submit(jzMember, SubmitJobRequest{Project: "t-kurs", Command: "x", LLMProject: "t-fremd"}); code != http.StatusConflict {
		t.Errorf("foreign LLM project: %d %s", code, body)
	}
	if code, j, body := submit(jzOutsider, SubmitJobRequest{Project: "t-fremd", Command: "x"}); code != http.StatusCreated || j.LLMProject != "" || secretOf(j.ID) != nil {
		t.Errorf("no LLM project at all: %d %s", code, body)
	}
	// The project's setting "none" holds for jobs as for workspaces.
	p, _ = e.store.Get(ctx, "t-kurs")
	p.DefaultLLMProject = workspace.NoLLM
	if err := e.store.Update(ctx, &p); err != nil {
		t.Fatal(err)
	}
	if code, j, body := submit(jzMember, SubmitJobRequest{Project: "t-kurs", Command: "x"}); code != http.StatusCreated || j.LLMProject != "" || secretOf(j.ID) != nil {
		t.Errorf("project without LLM: %d %s", code, body)
	}
}

// fakeFilesServer stands for files_server.py (tested itself in package gpu): a folder listing and one file for every path, and an upload of any path.
func fakeFilesServer(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			_, _ = io.WriteString(w, `{"ok":true}`)
		case "/list":
			_, _ = io.WriteString(w, `{"entries":[{"name":"out.txt","type":"file","size":6,"mtime":0}]}`)
		case "/file":
			if r.Method == http.MethodPut {
				b, _ := io.ReadAll(r.Body)
				w.WriteHeader(http.StatusCreated)
				_, _ = fmt.Fprintf(w, `{"size":%d,"mtime":0}`, len(b))
				return
			}
			w.Header().Set("Content-Length", "6")
			_, _ = io.WriteString(w, "result")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Who reads which files of a job (todo job_files): only who sees the job; its person the whole home while a member of the project, everybody else (managers, admins, the person after leaving) only the job's folder; paths never leave it.
func TestJobFilesRights(t *testing.T) {
	ctx := context.Background()
	e, hub := workspaceEnv(t)
	files := fakeFilesServer(t)
	e.gpu.SetFilesAddr(func(string) string { return files.URL })
	jobA := e.submit(t, jzMember, SubmitJobRequest{Project: "t-kurs", Command: "a", Name: "train"})

	const ok, bad, hidden = http.StatusOK, http.StatusBadRequest, http.StatusNotFound
	type want struct{ folder, home, escape, download int }
	matrix := map[string]want{
		"member (owner of jobA)":    {ok, ok, bad, ok},
		"other member":              {hidden, hidden, hidden, hidden},
		"reader":                    {hidden, hidden, hidden, hidden},
		"manager (group)":           {ok, bad, bad, ok},
		"outsider (other project)":  {hidden, hidden, hidden, hidden},
		"no project":                {hidden, hidden, hidden, hidden},
		"fleet admin, no project":   {hidden, hidden, hidden, hidden},
		"platform admin":            {ok, bad, bad, ok},
		"admin as member":           {ok, ok, bad, ok},
		"admin as outsider":         {hidden, hidden, hidden, hidden},
		"member's token":            {ok, ok, bad, ok},
		"member's read-only token":  {ok, ok, bad, ok},
		"manager's read-only token": {ok, bad, bad, ok},
		"admin's token":             {ok, bad, bad, ok},
	}
	base := "/v1/gpu/jobs/" + jobA + "/files"
	for _, a := range e.actors(t) {
		w, found := matrix[a.name]
		if !found {
			t.Fatalf("no expectation for %s", a.name)
		}
		for _, c := range []struct {
			what, path string
			want       int
		}{
			{"job folder", base, w.folder},
			{"another folder of the home", base + "?path=data", w.home},
			{"out of the home", base + "?path=../other/.ssh", w.escape},
			{"download", base + "/download?path=jobs/train/out.txt", w.download},
		} {
			code, body := call(t, e.h, "GET", c.path, a.bearer, a.actAs, nil)
			if code != c.want {
				t.Errorf("%s: %s: want %d, got %d %s", a.name, c.what, c.want, code, body)
			}
			if c.what == "download" && code == ok && string(body) != "result" {
				t.Errorf("%s: download: %q", a.name, body)
			}
		}
	}
	// The job folder by default; a manager cannot climb out of it.
	code, body := call(t, e.h, "GET", base, "oidc:"+jzManager, "", nil)
	var list gpu.JobFiles
	if _ = json.Unmarshal(body, &list); code != ok || list.Path != "jobs/train" || len(list.Files) != 1 || list.Files[0].Path != "jobs/train/out.txt" {
		t.Errorf("listing: %d %s", code, body)
	}
	if code, body := call(t, e.h, "GET", base+"/download?path=jobs/train/../../data/x", "oidc:"+jzManager, "", nil); code != bad {
		t.Errorf("manager climbing out of the job folder: %d %s", code, body)
	}
	if code, _ := call(t, e.h, "GET", base+"/download", "oidc:"+jzMember, "", nil); code != bad {
		t.Errorf("download without path: %d", code)
	}
	// From the member's workspace, as the member.
	if code, body := fromWorkspace(t, hub, "GET", base+"?path=data", "10.42.0.10", "token t-"+jzMember, nil); code != ok {
		t.Errorf("files from the workspace: %d %s", code, body)
	}
	// After leaving the project, the person reads only the job's folder.
	p, _ := e.store.Get(ctx, "t-kurs")
	p.Members = []workspace.Member{{Token: "group:kurs-leitung", Role: workspace.RoleManager}}
	if err := e.store.Update(ctx, &p); err != nil {
		t.Fatal(err)
	}
	if code, _ := call(t, e.h, "GET", base+"?path=data", "oidc:"+jzMember, "", nil); code != bad {
		t.Errorf("former member, home: %d, want 400", code)
	}
	if code, _ := call(t, e.h, "GET", base, "oidc:"+jzMember, "", nil); code != ok {
		t.Errorf("former member, job folder: %d, want 200", code)
	}
}

// Who uploads into which home (todo job_files, --files): whoever may submit a job into the project, into their own home there and never another's, and into the project's shared folder unless a reader (as in a workspace); acting on behalf uploads into the person's home, read-only tokens nothing. Paths stay in the home; a body needs its length.
func TestJobUploadRights(t *testing.T) {
	e, hub := workspaceEnv(t)
	files := fakeFilesServer(t)
	e.gpu.SetFilesAddr(func(string) string { return files.URL })
	const created, refused, forbidden = http.StatusCreated, http.StatusConflict, http.StatusForbidden
	// The same as submitting (TestJobRightsWrite): into t-kurs and into t-fremd.
	matrix := map[string][2]int{
		"member (owner of jobA)":    {created, refused},
		"other member":              {created, refused},
		"reader":                    {created, refused},
		"manager (group)":           {created, refused},
		"outsider (other project)":  {refused, created},
		"no project":                {forbidden, forbidden},
		"fleet admin, no project":   {forbidden, forbidden},
		"platform admin":            {refused, refused},
		"admin as member":           {created, refused},
		"admin as outsider":         {refused, created},
		"member's token":            {created, refused},
		"member's read-only token":  {forbidden, forbidden},
		"manager's read-only token": {forbidden, forbidden},
		"admin's token":             {refused, refused},
	}
	person := map[string]string{"other member": jzOther, "reader": jzReader, "manager (group)": jzManager, "outsider (other project)": jzOutsider, "admin as member": jzMember, "admin as outsider": jzOutsider}
	upload := func(path, bearer, actAs, body string) (int, []byte) {
		req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+bearer)
		if actAs != "" {
			req.Header.Set("X-Act-As", actAs)
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		return rec.Code, rec.Body.Bytes()
	}
	// uploadPods are the homes (subPath) the upload pods created so far write into.
	uploadPods := func() map[string]bool {
		e.kube.mu.Lock()
		defer e.kube.mu.Unlock()
		out := map[string]bool{}
		for _, o := range e.kube.objects {
			labels, _ := o["metadata"].(map[string]any)["labels"].(map[string]any)
			if labels["app"] == "dhbw-files" && labels["component"] == "upload" {
				c := o["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
				out[c["volumeMounts"].([]any)[0].(map[string]any)["subPath"].(string)] = true
			}
		}
		return out
	}
	for _, a := range e.actors(t) {
		w, found := matrix[a.name]
		if !found {
			t.Fatalf("no expectation for %s", a.name)
		}
		for i, project := range []string{"t-kurs", "t-fremd"} {
			code, body := upload("/v1/gpu/files?project="+project+"&path=jobs/run/train.py", strings.TrimPrefix(a.bearer, "Bearer "), a.actAs, "print(1)")
			if code != w[i] {
				t.Errorf("%s: upload into %s: want %d, got %d %s", a.name, project, w[i], code, body)
			}
			if code == created {
				var f gpu.JobFile
				if _ = json.Unmarshal(body, &f); f.Path != "jobs/run/train.py" || f.Size != 8 {
					t.Errorf("%s: answer %s", a.name, body)
				}
				who := person[a.name]
				if who == "" {
					who = jzMember
				}
				if !uploadPods()["homes/"+gpu.HubUserSlug(who)] {
					t.Errorf("%s: no upload pod of %s: %v", a.name, who, uploadPods())
				}
			}
		}
	}
	for _, c := range []struct {
		path string
		want int
	}{
		{"/v1/gpu/files?project=t-kurs&path=../other/x", http.StatusBadRequest},
		{"/v1/gpu/files?project=t-kurs&path=/etc/passwd", http.StatusBadRequest},
		{"/v1/gpu/files?project=t-kurs", http.StatusBadRequest},
		{"/v1/gpu/files?project=t-kurs&path=project-shared", http.StatusBadRequest},
		{"/v1/gpu/files?project=t-unbekannt&path=x", refused},
	} {
		if code, body := upload(c.path, "oidc:"+jzMember, "", "x"); code != c.want {
			t.Errorf("%s: want %d, got %d %s", c.path, c.want, code, body)
		}
	}
	// The shared folder: members write it, readers only read it (the reader's upload pod mounts it read-only).
	if code, body := upload("/v1/gpu/files?project=t-kurs&path=project-shared/daten/a.csv", "oidc:"+jzMember, "", "1,2"); code != created {
		t.Errorf("member into project-shared: %d %s", code, body)
	}
	if code, body := upload("/v1/gpu/files?project=t-kurs&path=project-shared/a.csv", "oidc:"+jzReader, "", "1,2"); code != http.StatusBadRequest || !strings.Contains(string(body), "Lesend") {
		t.Errorf("reader into project-shared: %d %s", code, body)
	}
	if code, body := upload("/v1/gpu/files?project=t-kurs&path=notes/a.csv", "oidc:"+jzReader, "", "1,2"); code != created {
		t.Errorf("reader into the own home: %d %s", code, body)
	}
	e.kube.mu.Lock()
	for _, o := range e.kube.objects {
		labels, _ := o["metadata"].(map[string]any)["labels"].(map[string]any)
		if labels["component"] != "upload" {
			continue
		}
		mounts := o["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["volumeMounts"].([]any)
		home, shared := mounts[0].(map[string]any), mounts[1].(map[string]any)
		if home["readOnly"] != false || shared["subPath"] != "project-shared" || shared["readOnly"] != (home["subPath"] == "homes/"+gpu.HubUserSlug(jzReader)) {
			t.Errorf("upload pod mounts: %v", mounts)
		}
	}
	e.kube.mu.Unlock()
	// Without Content-Length (chunked).
	req := httptest.NewRequest(http.MethodPut, "/v1/gpu/files?project=t-kurs&path=x", io.MultiReader(strings.NewReader("x")))
	req.ContentLength = -1
	req.Header.Set("Authorization", "Bearer oidc:"+jzMember)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusLengthRequired {
		t.Errorf("without length: %d %s", rec.Code, rec.Body)
	}
	// From the member's workspace, as the member.
	wreq := httptest.NewRequest(http.MethodPut, "/v1/gpu/files?project=t-kurs&path=data/a.csv", strings.NewReader("1,2"))
	wreq.RemoteAddr = "10.42.0.10:40000"
	wreq.Header.Set("Authorization", "token t-"+jzMember)
	rec = httptest.NewRecorder()
	hub.ServeHTTP(rec, wreq)
	if rec.Code != created {
		t.Errorf("upload from the workspace: %d %s", rec.Code, rec.Body)
	}
}

// A job may yield its GPU although the budget would run it guaranteed (todo job_cli, --preemptible); without that it is guaranteed as the budget says.
func TestJobVoluntarilyPreemptible(t *testing.T) {
	ctx := context.Background()
	e := newJobEnv(t)
	p, _ := e.store.Get(ctx, "t-kurs")
	p.BatchPriority = workspace.BatchGuaranteed
	if err := e.store.Update(ctx, &p); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		preemptible, want bool
	}{{false, true}, {true, false}} {
		code, body := call(t, e.h, "POST", "/v1/gpu/jobs", "oidc:"+jzMember, "", SubmitJobRequest{Project: "t-kurs", Command: "true", Preemptible: c.preemptible})
		var j gpu.Job
		if _ = json.Unmarshal(body, &j); code != http.StatusCreated || j.Guaranteed != c.want {
			t.Errorf("preemptible %v: %d guaranteed %v, want %v: %s", c.preemptible, code, j.Guaranteed, c.want, body)
		}
		_, _ = call(t, e.h, "DELETE", "/v1/gpu/jobs/"+j.ID, "oidc:"+jzAdmin, "", nil)
	}
}
