package gpu

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/pfisterer/llm-management-api/internal/cluster/workspace"
)

// The projects' credentials on the nodes (workspace/credentials.go): per project with credentials the pull secret ws-pull-<slug> (Docker config, the pods' imagePullSecrets) and ws-git-<slug> (git's credential store, mounted only into the checkout's init container), in the workspace namespace. The policy workspace-pods lets a pod name only those of its own project.
//
// The service never reads secrets in the workspace namespace (no get, list or watch in its Role, docs/architektur.md "Secrets des KI-Dienstes"): that right would show it the hub's secret (OIDC client secret, cookie secret, service tokens) and Helm's release secrets. What exists is known from the database instead: the wanted secrets are written (POST, on 409 a PUT without resourceVersion), those of the projects without credentials deleted (404 is fine). The admission policy credential-secrets lets the service write and delete only names starting with ws-pull- and ws-git- there (and create a batch job's LLM key, jobs.go).

const credentialsLabel = "dhbw.cloud/credentials"

// credentialRefresh is how long a written secret is trusted to be as written: after that it is written again, so one deleted by hand comes back without a restart.
const credentialRefresh = time.Hour

// credentialState is what this process wrote and deleted (in memory, empty after a restart, when everything is written and deleted once more): the checksum and time of each written secret, and the names known to be gone. It keeps the five-minute sync from writing every secret and deleting every absent one at every pass.
type credentialState struct {
	mu      sync.Mutex
	written map[string]credentialWrite
	absent  map[string]bool
}

type credentialWrite struct {
	sum [32]byte
	at  time.Time
}

// EnsureCredentialSecrets brings the projects' secrets to want: written where this process has not written them as they are within credentialRefresh, and deleted for the projects in without (projects without credentials, and deleted ones; a slug in both counts as wanted).
func (s *Service) EnsureCredentialSecrets(ctx context.Context, want map[string]workspace.ProjectSecrets, without []string) error {
	if s.k8s == nil {
		return nil
	}
	ns := s.workspaceNamespace()
	base := "/api/v1/namespaces/" + ns + "/secrets"
	type secret struct {
		typ  string
		data map[string]string
	}
	wanted := map[string]secret{}
	for slug, ps := range want {
		wanted[workspace.PullSecretName(slug)] = secret{"kubernetes.io/dockerconfigjson", map[string]string{".dockerconfigjson": base64.StdEncoding.EncodeToString(ps.DockerConfig)}}
		wanted[workspace.GitSecretName(slug)] = secret{"Opaque", map[string]string{"credentials": base64.StdEncoding.EncodeToString(ps.GitCredentials)}}
	}
	st := &s.credState
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.written == nil {
		st.written, st.absent = map[string]credentialWrite{}, map[string]bool{}
	}
	now := time.Now()
	var errs []error
	names := make([]string, 0, len(wanted))
	for n := range wanted {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		w := wanted[name]
		sum := credentialSum(w.typ, w.data)
		if c, ok := st.written[name]; ok && c.sum == sum && now.Sub(c.at) < credentialRefresh {
			continue
		}
		obj := map[string]any{
			"apiVersion": "v1", "kind": "Secret", "type": w.typ,
			"metadata": map[string]any{"name": name, "namespace": ns, "labels": map[string]any{credentialsLabel: "true", managedLabel: managedValue}},
			"data":     w.data,
		}
		err := s.k8s.do(ctx, http.MethodPost, base, obj, nil)
		if errors.Is(err, errAlreadyExists) {
			// Secrets may be updated without a resourceVersion: the whole object replaces the one there.
			err = s.k8s.do(ctx, http.MethodPut, base+"/"+name, obj, nil)
		}
		if err != nil {
			delete(st.written, name)
			errs = append(errs, err)
			continue
		}
		st.written[name] = credentialWrite{sum, now}
		delete(st.absent, name)
	}
	var gone []string
	for _, slug := range without {
		for _, name := range []string{workspace.PullSecretName(slug), workspace.GitSecretName(slug)} {
			if _, keep := wanted[name]; !keep && !st.absent[name] {
				gone = append(gone, name)
			}
		}
	}
	sort.Strings(gone)
	for _, name := range gone {
		if err := s.k8s.do(ctx, http.MethodDelete, base+"/"+name, nil, nil); err != nil && !errors.Is(err, ErrNotFound) {
			errs = append(errs, err)
			continue
		}
		st.absent[name] = true
		delete(st.written, name)
	}
	return errors.Join(errs...)
}

// credentialSum is a checksum of a secret's type and data, so a changed one is written again.
func credentialSum(typ string, data map[string]string) [32]byte {
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	h.Write([]byte(typ))
	for _, k := range keys {
		h.Write([]byte{0})
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write([]byte(data[k]))
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}
