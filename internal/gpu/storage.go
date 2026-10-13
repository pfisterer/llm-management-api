package gpu

import (
	"context"
	"net/http"
	"net/url"
)

// Volume is a project's workspace volume (PVC ws-<slug> in the hub's namespace), labelled by the hub with the project's slug.
type Volume struct {
	PVC     string
	Project string
}

// storageKube is the part of the Kubernetes client the project volumes need.
type storageKube interface {
	listPVCs(ctx context.Context, ns, selector string) ([]Volume, error)
	deletePVC(ctx context.Context, ns, name string) error
}

func (k *kube) listPVCs(ctx context.Context, ns, selector string) ([]Volume, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := k.do(ctx, http.MethodGet, "/api/v1/namespaces/"+ns+"/persistentvolumeclaims?labelSelector="+url.QueryEscape(selector), nil, &list); err != nil {
		return nil, err
	}
	out := make([]Volume, 0, len(list.Items))
	for _, it := range list.Items {
		out = append(out, Volume{PVC: it.Metadata.Name, Project: it.Metadata.Labels["dhbw.cloud/project"]})
	}
	return out, nil
}

func (k *kube) deletePVC(ctx context.Context, ns, name string) error {
	return k.do(ctx, http.MethodDelete, "/api/v1/namespaces/"+ns+"/persistentvolumeclaims/"+url.PathEscape(name), nil, nil)
}

func (s *Service) workspaceNamespace() string {
	if s.cfg.WorkspaceNamespace == "" {
		return "jupyterhub"
	}
	return s.cfg.WorkspaceNamespace
}

// ProjectVolumes lists the projects' volumes (label dhbw.cloud/workspace-storage=true); nil without a cluster.
func (s *Service) ProjectVolumes(ctx context.Context) ([]Volume, error) {
	k, ok := s.pods.(storageKube)
	if !ok {
		return nil, nil
	}
	return k.listPVCs(ctx, s.workspaceNamespace(), "dhbw.cloud/workspace-storage=true")
}

// DeleteVolume deletes a project's volume. Kubernetes keeps it until the last pod using it has ended; the StorageClass then removes its directory (JuiceFS keeps it in its trash for a few days).
func (s *Service) DeleteVolume(ctx context.Context, pvc string) error {
	k, ok := s.pods.(storageKube)
	if !ok {
		return nil
	}
	err := k.deletePVC(ctx, s.workspaceNamespace(), pvc)
	if err == ErrNotFound {
		return nil
	}
	return err
}
