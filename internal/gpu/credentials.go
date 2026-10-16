package gpu

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"sort"

	"github.com/pfisterer/llm-management-api/internal/workspace"
)

// The projects' credentials on the nodes (workspace/credentials.go): per project with credentials the pull secret ws-pull-<slug> (Docker config, the pods' imagePullSecrets) and ws-git-<slug> (git's credential store, mounted only into the checkout's init container), in the workspace namespace. The policy workspace-pods lets a pod name only those of its own project.

const credentialsLabel = "dhbw.cloud/credentials"

// EnsureCredentialSecrets brings the projects' secrets to want: created or changed where they differ, removed for projects without credentials.
func (s *Service) EnsureCredentialSecrets(ctx context.Context, want map[string]workspace.ProjectSecrets) error {
	if s.k8s == nil {
		return nil
	}
	ns := s.kernelNamespace()
	base := "/api/v1/namespaces/" + ns + "/secrets"
	var have struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Data map[string]string `json:"data"`
		} `json:"items"`
	}
	if err := s.k8s.do(ctx, http.MethodGet, base+"?labelSelector="+url.QueryEscape(credentialsLabel+"=true"), nil, &have); err != nil {
		return err
	}
	type secret struct {
		typ  string
		data map[string]string
	}
	wanted := map[string]secret{}
	for slug, ps := range want {
		wanted[workspace.PullSecretName(slug)] = secret{"kubernetes.io/dockerconfigjson", map[string]string{".dockerconfigjson": base64.StdEncoding.EncodeToString(ps.DockerConfig)}}
		wanted[workspace.GitSecretName(slug)] = secret{"Opaque", map[string]string{"credentials": base64.StdEncoding.EncodeToString(ps.GitCredentials)}}
	}
	var errs []error
	current := map[string]map[string]string{}
	for _, it := range have.Items {
		current[it.Metadata.Name] = it.Data
		if _, ok := wanted[it.Metadata.Name]; !ok {
			if err := s.k8s.do(ctx, http.MethodDelete, base+"/"+it.Metadata.Name, nil, nil); err != nil && !errors.Is(err, ErrNotFound) {
				errs = append(errs, err)
			}
		}
	}
	names := make([]string, 0, len(wanted))
	for n := range wanted {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		w := wanted[name]
		data, exists := current[name]
		if exists && reflect.DeepEqual(data, w.data) {
			continue
		}
		obj := map[string]any{
			"apiVersion": "v1", "kind": "Secret", "type": w.typ,
			"metadata": map[string]any{"name": name, "namespace": ns, "labels": map[string]any{credentialsLabel: "true", managedLabel: managedValue}},
			"data":     w.data,
		}
		method, path := http.MethodPost, base
		if exists {
			method, path = http.MethodPut, base+"/"+name
		}
		if err := s.k8s.do(ctx, method, path, obj, nil); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
