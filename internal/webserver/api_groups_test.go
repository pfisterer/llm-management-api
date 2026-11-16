package webserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pfisterer/llm-management-api/internal/cluster/gpu"
	"github.com/pfisterer/llm-management-api/internal/cluster/usage"
	"github.com/pfisterer/llm-management-api/internal/cluster/workspace"
)

// Groups of a project (docs/todo.md areas_groups): its managers keep them, members get their group's folder below ~/dhbw/groups in jobs and uploads, nobody another group's.
func TestProjectGroups(t *testing.T) {
	e := newJobEnv(t, func(o *Options, _ *gpu.Config) { o.Usage = usage.NewMemoryStore() })
	files := fakeFilesServer(t)
	e.gpu.SetFilesAddr(func(string) string { return files.URL })
	project := func(email string) workspace.Mine {
		t.Helper()
		code, body := call(t, e.h, "GET", "/v1/workspaces/projects", "oidc:"+email, "", nil)
		var list struct {
			Items []workspace.Mine `json:"items"`
		}
		if err := json.Unmarshal(body, &list); code != http.StatusOK || err != nil {
			t.Fatalf("projects of %s: %d %s", email, code, body)
		}
		for _, m := range list.Items {
			if m.Slug == "t-kurs" {
				return m
			}
		}
		t.Fatalf("%s: no t-kurs in %s", email, body)
		return workspace.Mine{}
	}
	p := project(jzManager).Project
	p.Groups = []workspace.Group{
		{Name: "gruppe-1", Members: []string{"user:" + jzMember}},
		{Name: "material", Members: []string{"user:" + jzMember, "user:" + jzOther}, ReadOnly: true},
	}
	// A member may not change the groups; the project's manager may.
	if code, _ := call(t, e.h, "PUT", "/v1/workspaces/projects/t-kurs", "oidc:"+jzMember, "", p); code == http.StatusOK {
		if got := project(jzManager).Groups; len(got) != 0 {
			t.Fatalf("a member set groups: %+v", got)
		}
	}
	code, body := call(t, e.h, "PUT", "/v1/workspaces/projects/t-kurs", "oidc:"+jzManager, "", p)
	if code != http.StatusOK {
		t.Fatalf("manager sets groups: %d %s", code, body)
	}
	saved := project(jzManager)
	if len(saved.Groups) != 2 || saved.Groups[0].ID == "" {
		t.Fatalf("groups: %+v", saved.Groups)
	}
	areas := func(email string) string {
		out := []string{}
		for _, a := range project(email).Areas {
			ro := "rw"
			if a.ReadOnly {
				ro = "ro"
			}
			out = append(out, a.Mount+":"+ro)
		}
		return strings.Join(out, " ")
	}
	for email, want := range map[string]string{
		jzMember:  "dhbw/project:rw dhbw/groups/gruppe-1:rw dhbw/groups/material:ro",
		jzOther:   "dhbw/project:rw dhbw/groups/material:ro",
		jzReader:  "dhbw/project:rw",
		jzManager: "dhbw/project:rw dhbw/groups/gruppe-1:rw dhbw/groups/material:rw",
	} {
		if got := areas(email); got != want {
			t.Errorf("areas of %s: %q, want %q", email, got, want)
		}
	}
	// Who is in which group only the managers see; a member sees the groups they belong to.
	names := func(email string) string {
		out := []string{}
		for _, g := range project(email).Groups {
			out = append(out, g.Name)
		}
		return strings.Join(out, " ")
	}
	for email, want := range map[string]string{jzManager: "gruppe-1 material", jzMember: "gruppe-1 material", jzOther: "material", jzReader: ""} {
		if got := names(email); got != want {
			t.Errorf("groups %s sees: %q, want %q", email, got, want)
		}
	}
	// A job mounts the person's areas: the member's group, never at another member's job.
	g1 := "groups/" + saved.Groups[0].ID
	memberJob := e.kube.jobObject(e.submit(t, jzMember, SubmitJobRequest{Project: "t-kurs", Command: "x"}))
	otherJob := e.kube.jobObject(e.submit(t, jzOther, SubmitJobRequest{Project: "t-kurs", Command: "x"}))
	if !strings.Contains(memberJob, `{"mountPath":"/home/jovyan/dhbw/groups/gruppe-1","name":"home","subPath":"`+g1+`"}`) {
		t.Errorf("member's job lacks the group: %s", memberJob)
	}
	if strings.Contains(otherJob, g1) {
		t.Errorf("another member's job mounts the group: %s", otherJob)
	}
	upload := func(path, email string) (int, string) {
		req := httptest.NewRequest(http.MethodPut, "/v1/gpu/files?project=t-kurs&path="+path, strings.NewReader("1,2"))
		req.Header.Set("Authorization", "Bearer oidc:"+email)
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	for _, c := range []struct {
		path, email string
		want        int
	}{
		{"dhbw/groups/gruppe-1/a.csv", jzMember, http.StatusCreated},
		{"dhbw/groups/gruppe-1/a.csv", jzOther, http.StatusBadRequest},
		{"dhbw/groups/material/a.csv", jzMember, http.StatusBadRequest},
		{"dhbw/groups/material/a.csv", jzManager, http.StatusCreated},
		{"dhbw/a.csv", jzMember, http.StatusBadRequest},
		{"dhbw/project/a.csv", jzOther, http.StatusCreated},
	} {
		if code, body := upload(c.path, c.email); code != c.want {
			t.Errorf("upload %s as %s: %d %s, want %d", c.path, c.email, code, body, c.want)
		}
	}
	// Archived: the members lose the folder, the managers keep reading it.
	p = saved.Project
	p.Groups[0].Archived = true
	if code, body := call(t, e.h, "PUT", "/v1/workspaces/projects/t-kurs", "oidc:"+jzManager, "", p); code != http.StatusOK {
		t.Fatalf("archive: %d %s", code, body)
	}
	if got := areas(jzMember); got != "dhbw/project:rw dhbw/groups/material:ro" {
		t.Errorf("member after archiving: %q", got)
	}
	if got := areas(jzManager); !strings.Contains(got, "dhbw/groups/gruppe-1:ro") {
		t.Errorf("manager after archiving: %q", got)
	}
}
