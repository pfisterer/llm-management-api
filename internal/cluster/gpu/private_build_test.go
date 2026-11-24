package gpu

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/pfisterer/llm-management-api/internal/cluster/workspace"
	"go.uber.org/zap"
)

// A repository that is not public is built with the project's access: into the private project, in a repository of the project (never shared), with the private builder secret, and cloned by the init container clone, the only one that sees the access (in a Secret owned by the job, never in the job).
func TestPrivateProjectImageBuild(t *testing.T) {
	b := &fakeBuilds{status: map[string]jobStatus{}}
	i := &fakeImages{have: map[string]bool{}}
	refs := func(_ context.Context, _ string, auth *workspace.Basic) (gitRefs, error) {
		if auth == nil || auth.Token != "tok-s3cr3t" {
			return gitRefs{}, errors.New("repository not found")
		}
		return gitRefs{Refs: map[string]string{"refs/heads/main": "0123456789abcdef0123456789abcdef01234567"}, DefaultBranch: "main"}, nil
	}
	cfg := Config{Registry: "reg", JupyterURL: "https://hub", PrivateEnvsProject: "envs-private", PrivatePullToken: "pull", EgressGuard: "10.43.0.1:443", GitHosts: []string{"gitlab.dhbw.de"}}
	s := newService(cfg, NewMemoryStore(), b, i, &fakeHub{}, refs, zap.NewNop().Sugar())
	s.SetBases([]workspace.Base{{Image: testBase}})
	ctx := context.Background()

	// Without an access of the project: refused, with a hint where to add one.
	if _, err := s.BuildProjectImage(ctx, "kurs", testBase, "https://gitlab.dhbw.de/o/geheim", "", false); err == nil || !strings.Contains(err.Error(), "Zugang des Projekts für gitlab.dhbw.de") {
		t.Fatalf("no access: %v", err)
	}
	s.SetGitAuth(func(_ context.Context, project, host string) *workspace.Basic {
		if project == "kurs" && host == "gitlab.dhbw.de" {
			return &workspace.Basic{Username: "deploy", Token: "tok-s3cr3t"}
		}
		return nil
	})
	got, err := s.BuildProjectImage(ctx, "kurs", testBase, "https://gitlab.dhbw.de/o/geheim", "", false)
	if err != nil || got.Status != "building" || got.Image != "reg/envs-private/kurs--gitlab-dhbw-de-o-geheim:0123456789ab-pytorch-v2.0.0" {
		t.Fatalf("build %+v %v", got, err)
	}
	if len(i.project) == 0 || i.project[len(i.project)-1] != "envs-private" {
		t.Errorf("looked up in %v", i.project)
	}
	job := b.jobs[b.created[0]]
	raw, _ := json.Marshal(job)
	if strings.Contains(string(raw), "tok-s3cr3t") || strings.Contains(string(raw), "deploy:") {
		t.Errorf("the access is in the job: %s", raw)
	}
	spec := job["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	inits := spec["initContainers"].([]any)
	if len(inits) != 2 || inits[0].(map[string]any)["name"] != "dhbw-egress-guard" || inits[1].(map[string]any)["name"] != "clone" {
		t.Fatalf("init containers %v", inits)
	}
	build := spec["containers"].([]any)[0].(map[string]any)
	for _, m := range build["volumeMounts"].([]any) {
		if m.(map[string]any)["name"] == "git" {
			t.Error("the build container sees the access")
		}
	}
	if !strings.Contains(string(raw), `"secretName":"harbor-envs-private-builder"`) || !strings.Contains(string(raw), `reg/envs-private/buildcache:kurs--gitlab-dhbw-de-o-geheim`) || !strings.Contains(string(raw), `"PRECLONED"`) {
		t.Errorf("job %s", raw)
	}
	if len(b.secrets) != 1 {
		t.Fatalf("secrets %v", b.secrets)
	}
	sec := b.secrets[0]
	meta := sec["metadata"].(map[string]any)
	owner := meta["ownerReferences"].([]any)[0].(map[string]any)
	if meta["name"] != b.created[0]+"-git" || owner["name"] != b.created[0] || owner["uid"] != "uid-"+b.created[0] || sec["stringData"].(map[string]any)["credentials"] != "https://deploy:tok-s3cr3t@gitlab.dhbw.de\n" {
		t.Errorf("secret %v", sec)
	}

	// Another project with the same repository and its own access builds its own image.
	s.SetGitAuth(func(context.Context, string, string) *workspace.Basic { return &workspace.Basic{Username: "x", Token: "tok-s3cr3t"} })
	other, err := s.BuildProjectImage(ctx, "zweiter", testBase, "https://gitlab.dhbw.de/o/geheim", "", false)
	if err != nil || !strings.HasPrefix(other.Image, "reg/envs-private/zweiter--") || len(b.created) != 2 {
		t.Fatalf("other %+v %v %v", other, err, b.created)
	}

	// Not set up: refused.
	s.cfg.PrivatePullToken = ""
	if _, err := s.BuildProjectImage(ctx, "dritter", testBase, "https://gitlab.dhbw.de/o/geheim", "", false); err == nil || !strings.Contains(err.Error(), "nicht eingerichtet") {
		t.Fatalf("off: %v", err)
	}
}
