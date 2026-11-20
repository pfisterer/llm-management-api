package gpu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/pfisterer/llm-management-api/internal/cluster/workspace"
)

// fakeSecrets is the secrets API of one namespace with the service's rights there: create, update and delete, never get or list.
type fakeSecrets struct {
	items map[string]map[string]any
	calls []string
}

func (f *fakeSecrets) do(_ context.Context, method, path string, body, out any) error {
	f.calls = append(f.calls, method+" "+path)
	name := path[strings.LastIndex(path, "/")+1:]
	switch method {
	case http.MethodPost, http.MethodPut:
		b, _ := json.Marshal(body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		meta := m["metadata"].(map[string]any)
		if _, rv := meta["resourceVersion"]; rv {
			return errors.New("resourceVersion sent")
		}
		n := meta["name"].(string)
		if _, ok := f.items[n]; ok && method == http.MethodPost {
			return errAlreadyExists
		}
		if _, ok := f.items[n]; !ok && method == http.MethodPut {
			return ErrNotFound
		}
		if method == http.MethodPut && n != name {
			return errors.New("name and path differ")
		}
		f.items[n] = m
	case http.MethodDelete:
		if _, ok := f.items[name]; !ok {
			return ErrNotFound
		}
		delete(f.items, name)
	default:
		return fmt.Errorf("forbidden: %s %s", method, path)
	}
	return nil
}

// A project with credentials gets its pull and Git secret, unchanged ones are not written again, a changed one is replaced, those of a project without credentials go, and nothing is ever read.
func TestEnsureCredentialSecrets(t *testing.T) {
	s, _, _, _ := testService()
	f := &fakeSecrets{items: map[string]map[string]any{
		// Left over from before (e.g. the service restarted): replaced without being read.
		"ws-git-kurs": {"metadata": map[string]any{"name": "ws-git-kurs"}, "data": map[string]any{"credentials": "alt"}},
		// A project without credentials any more.
		"ws-pull-frei": {"metadata": map[string]any{"name": "ws-pull-frei"}},
		// Not ours: never touched.
		"hub": {"metadata": map[string]any{"name": "hub"}},
	}}
	s.k8s = f
	ctx := context.Background()
	want := map[string]workspace.ProjectSecrets{"kurs": {DockerConfig: []byte(`{"auths":{}}`), GitCredentials: []byte("https://u:t@gitlab.dhbw.de\n")}}
	if err := s.EnsureCredentialSecrets(ctx, want, []string{"frei", "leer"}); err != nil {
		t.Fatal(err)
	}
	pull, git := f.items["ws-pull-kurs"], f.items["ws-git-kurs"]
	if pull == nil || pull["type"] != "kubernetes.io/dockerconfigjson" || git == nil || git["type"] != "Opaque" || len(f.items) != 3 || f.items["hub"] == nil {
		t.Fatalf("secrets %v", f.items)
	}
	if got := git["data"].(map[string]any)["credentials"]; got == "alt" {
		t.Fatalf("old Git secret not replaced")
	}
	wantCalls := []string{
		"POST /api/v1/namespaces/jupyterhub/secrets", "PUT /api/v1/namespaces/jupyterhub/secrets/ws-git-kurs", "POST /api/v1/namespaces/jupyterhub/secrets",
		"DELETE /api/v1/namespaces/jupyterhub/secrets/ws-git-frei", "DELETE /api/v1/namespaces/jupyterhub/secrets/ws-git-leer", "DELETE /api/v1/namespaces/jupyterhub/secrets/ws-pull-frei", "DELETE /api/v1/namespaces/jupyterhub/secrets/ws-pull-leer",
	}
	if !slices.Equal(f.calls, wantCalls) {
		t.Fatalf("calls\n%v\nwant\n%v", f.calls, wantCalls)
	}
	// The next pass writes and deletes nothing it already did.
	f.calls = nil
	if err := s.EnsureCredentialSecrets(ctx, want, []string{"frei", "leer"}); err != nil || len(f.calls) != 0 {
		t.Fatalf("unchanged written again: %v %v", err, f.calls)
	}
	// Changed credentials are written again; a secret deleted by hand comes back after credentialRefresh.
	want["kurs"] = workspace.ProjectSecrets{DockerConfig: []byte(`{"auths":{"x":{}}}`), GitCredentials: want["kurs"].GitCredentials}
	if err := s.EnsureCredentialSecrets(ctx, want, nil); err != nil || !slices.Equal(f.calls, []string{"POST /api/v1/namespaces/jupyterhub/secrets", "PUT /api/v1/namespaces/jupyterhub/secrets/ws-pull-kurs"}) {
		t.Fatalf("changed: %v %v", err, f.calls)
	}
	delete(f.items, "ws-git-kurs")
	w := s.credState.written["ws-git-kurs"]
	w.at = w.at.Add(-credentialRefresh)
	s.credState.written["ws-git-kurs"] = w
	if err := s.EnsureCredentialSecrets(ctx, want, nil); err != nil || f.items["ws-git-kurs"] == nil {
		t.Fatalf("not restored: %v %v", err, f.items)
	}
	// The project loses its credentials (or is deleted): both secrets go.
	f.calls = nil
	if err := s.EnsureCredentialSecrets(ctx, map[string]workspace.ProjectSecrets{}, []string{"frei", "kurs", "leer"}); err != nil || len(f.items) != 1 || f.items["hub"] == nil || len(f.calls) != 2 {
		t.Fatalf("not removed: %v %v %v", err, f.items, f.calls)
	}
}

// A failed delete is tried again at the next pass; a slug both wanted and without keeps its secrets.
func TestEnsureCredentialSecretsRetry(t *testing.T) {
	s, _, _, _ := testService()
	f := &failingSecrets{fakeSecrets: fakeSecrets{items: map[string]map[string]any{"ws-pull-weg": {"metadata": map[string]any{"name": "ws-pull-weg"}}}}, fail: true}
	s.k8s = f
	ctx := context.Background()
	if err := s.EnsureCredentialSecrets(ctx, nil, []string{"weg"}); err == nil {
		t.Fatal("error swallowed")
	}
	f.fail = false
	if err := s.EnsureCredentialSecrets(ctx, nil, []string{"weg"}); err != nil || len(f.items) != 0 {
		t.Fatalf("not retried: %v %v", err, f.items)
	}
	want := map[string]workspace.ProjectSecrets{"beide": {DockerConfig: []byte(`{}`), GitCredentials: []byte("x")}}
	if err := s.EnsureCredentialSecrets(ctx, want, []string{"beide"}); err != nil || len(f.items) != 2 {
		t.Fatalf("wanted secrets deleted: %v %v", err, f.items)
	}
}

type failingSecrets struct {
	fakeSecrets
	fail bool
}

func (f *failingSecrets) do(ctx context.Context, method, path string, body, out any) error {
	if f.fail && method == http.MethodDelete {
		return errors.New("kubernetes DELETE: 500")
	}
	return f.fakeSecrets.do(ctx, method, path, body, out)
}
