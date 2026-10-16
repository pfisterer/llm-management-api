package gpu

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/pfisterer/llm-management-api/internal/workspace"
)

// fakeSecrets is the secrets API of one namespace.
type fakeSecrets struct {
	items map[string]map[string]any
	calls []string
}

func (f *fakeSecrets) do(_ context.Context, method, path string, body, out any) error {
	f.calls = append(f.calls, method+" "+path)
	name := path[strings.LastIndex(path, "/")+1:]
	switch method {
	case http.MethodGet:
		var items []any
		for _, it := range f.items {
			items = append(items, it)
		}
		b, _ := json.Marshal(map[string]any{"items": items})
		return json.Unmarshal(b, out)
	case http.MethodPost, http.MethodPut:
		b, _ := json.Marshal(body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		f.items[m["metadata"].(map[string]any)["name"].(string)] = m
	case http.MethodDelete:
		delete(f.items, name)
	}
	return nil
}

// A project with credentials gets its pull and Git secret, unchanged ones are not written again, and those of a project without credentials go.
func TestEnsureCredentialSecrets(t *testing.T) {
	s, _, _, _ := testService()
	f := &fakeSecrets{items: map[string]map[string]any{}}
	s.k8s = f
	ctx := context.Background()
	want := map[string]workspace.ProjectSecrets{"kurs": {DockerConfig: []byte(`{"auths":{}}`), GitCredentials: []byte("https://u:t@gitlab.dhbw.de\n")}}
	if err := s.EnsureCredentialSecrets(ctx, want); err != nil {
		t.Fatal(err)
	}
	pull, git := f.items["ws-pull-kurs"], f.items["ws-git-kurs"]
	if pull == nil || pull["type"] != "kubernetes.io/dockerconfigjson" || git == nil || git["type"] != "Opaque" || len(f.items) != 2 {
		t.Fatalf("secrets %v", f.items)
	}
	f.calls = nil
	if err := s.EnsureCredentialSecrets(ctx, want); err != nil || len(f.calls) != 1 {
		t.Fatalf("unchanged written again: %v %v", err, f.calls)
	}
	if err := s.EnsureCredentialSecrets(ctx, map[string]workspace.ProjectSecrets{}); err != nil || len(f.items) != 0 {
		t.Fatalf("not removed: %v %v", err, f.items)
	}
}
