package gpu

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
)

// Course images (docs/bauplan.md 10.12): the image of a project's "CPU + GPU-Kernel", notebook and GPU kernel alike. Built from a Git repository the way Binder reads it — apt.txt, environment.yml, requirements.txt, postBuild, in binder/ or .binder/ if there is one — but on notebook-base (Config.CourseBase) instead of repo2docker's own base, so the result has everything the profile needs and the label cloud.dhbw.base. A repository with a Dockerfile is not a course build: its own FROM decides, and it is added as an ordinary environment.

// courseScript is the build job's script for a course image: it writes the Dockerfile from the repository's files and builds it with BuildKit.
const courseScript = `set -euo pipefail
git clone --quiet --no-checkout "$GIT_URL" /tmp/repo
git -C /tmp/repo checkout --quiet "$GIT_COMMIT"
dir=/tmp/repo
for d in binder .binder; do if [ -d "/tmp/repo/$d" ]; then dir="/tmp/repo/$d"; break; fi; done
if [ -f "$dir/Dockerfile" ]; then
  echo "dhbw: Das Repository hat ein Dockerfile. Ein Kursimage entsteht aus requirements.txt, environment.yml, apt.txt und postBuild; ein eigenes Dockerfile (FROM …/platform/notebook-base) als normale Umgebung hinzufügen."
  exit 1
fi
if [ ! -f "$dir/requirements.txt" ] && [ ! -f "$dir/environment.yml" ] && [ ! -f "$dir/apt.txt" ] && [ ! -f "$dir/postBuild" ]; then
  echo "dhbw: Weder requirements.txt noch environment.yml, apt.txt oder postBuild gefunden (im Hauptverzeichnis, binder/ oder .binder/)."
  exit 1
fi
mkdir -p /tmp/dockerfile
{
  echo "FROM $BASE_IMAGE"
  if [ -f "$dir/apt.txt" ]; then
    echo 'USER root'
    echo 'COPY apt.txt /tmp/dhbw/apt.txt'
    echo 'RUN apt-get update && sed -e "s/#.*//" /tmp/dhbw/apt.txt | xargs -r apt-get install -y --no-install-recommends && rm -rf /var/lib/apt/lists/*'
    echo 'USER ${NB_UID}'
  fi
  if [ -f "$dir/environment.yml" ]; then
    echo 'COPY --chown=${NB_UID}:${NB_GID} environment.yml /tmp/dhbw/environment.yml'
    echo 'RUN mamba env update -n base -f /tmp/dhbw/environment.yml && mamba clean --all -f -y'
  fi
  if [ -f "$dir/requirements.txt" ]; then
    echo 'COPY --chown=${NB_UID}:${NB_GID} requirements.txt /tmp/dhbw/requirements.txt'
    echo 'RUN pip install --no-cache-dir -r /tmp/dhbw/requirements.txt'
  fi
  if [ -f "$dir/postBuild" ]; then
    echo 'COPY --chown=${NB_UID}:${NB_GID} postBuild /tmp/dhbw/postBuild'
    echo 'RUN bash /tmp/dhbw/postBuild'
  fi
  echo 'RUN fix-permissions "${CONDA_DIR}" && fix-permissions "/home/${NB_USER}"'
  echo "LABEL org.dhbw.git.url=\"$GIT_URL\" org.dhbw.git.branch=\"$GIT_BRANCH\""
} > /tmp/dockerfile/Dockerfile
cat /tmp/dockerfile/Dockerfile
buildctl-daemonless.sh build --progress=plain --frontend dockerfile.v0 --local context="$dir" --local dockerfile=/tmp/dockerfile \
  --output "type=image,name=$IMAGE,push=true" \
  --export-cache "type=registry,ref=$R2D_BUILDKIT_CACHE-course,mode=max" --import-cache "type=registry,ref=$R2D_BUILDKIT_CACHE-course"`

var tagUnsafe = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

// courseTag is the tag of a course image: the commit and the release of its base, so a new base is a new image.
func courseTag(commit, base string) string {
	v := base[strings.LastIndex(base, ":")+1:]
	if i := strings.LastIndex(base, "@"); i >= 0 {
		v = strings.TrimPrefix(base[i+1:], "sha256:")[:12]
	}
	return commit[:12] + "-" + tagUnsafe.ReplaceAllString(v, "-")
}

// SetImageReplaced is told when a course image rebuilt on a new base is ready, with the old and the new image (the projects follow).
func (s *Service) SetImageReplaced(f func(ctx context.Context, old, new string) error) {
	s.replaced = f
}

// RebaseCourseImages rebuilds the course images that are on an older base than Config.CourseBase, and follows the rebuilds under way: a ready one hands its new image to the projects (SetImageReplaced). Run periodically.
func (s *Service) RebaseCourseImages(ctx context.Context) error {
	if s.cfg.CourseBase == "" {
		return nil
	}
	envs, err := s.store.List(ctx, "")
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range envs {
		if e.Base == "" {
			continue
		}
		e = s.refresh(ctx, e)
		if e.Base == s.cfg.CourseBase || e.Status == StatusBuilding {
			continue
		}
		old := e.Image
		if e.Status == StatusFailed && e.Replaces != "" {
			old = e.Replaces // a failed rebuild: the projects still use the image before it
		}
		slug := slugOf(e.Image)
		tag := courseTag(e.Commit, s.cfg.CourseBase)
		e.Base, e.Image, e.Replaces = s.cfg.CourseBase, fmt.Sprintf("%s/%s/%s:%s", s.cfg.Registry, s.cfg.EnvsProject, slug, tag), old
		if err := s.startBuildIfMissing(ctx, &e, slug, tag, false); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", e.Image, err))
			continue
		}
		if e.Status == StatusReady && s.replaced != nil {
			// Built before (someone else's environment of the same repository): move the projects now.
			if err := s.replaced(ctx, e.Replaces, e.Image); err != nil {
				errs = append(errs, err)
			} else {
				e.Replaces = ""
			}
		}
		if _, err := s.store.Save(ctx, e); err != nil {
			errs = append(errs, err)
		}
		s.log.Infow("course image rebuilt on new base", "old", old, "new", e.Image, "status", e.Status)
	}
	return errors.Join(errs...)
}

// PrepullName is the DaemonSet that keeps the course images on the CPU and GPU nodes, so that the first start of a server or GPU kernel in a course does not wait for a pull of several GB.
const PrepullName = "kernel-image-prepull"

// prepullDaemonSet pulls every image as an init container (which ends at once) and keeps the pod with the first one, so the images count as in use and the kubelet does not clean them up.
func prepullDaemonSet(ns string, images []string) map[string]any {
	sum := sha256.Sum256([]byte(strings.Join(images, "\n")))
	security := map[string]any{"runAsNonRoot": true, "runAsUser": 1000, "allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []any{"ALL"}}}
	resources := map[string]any{"requests": map[string]any{"cpu": "1m", "memory": "8Mi"}, "limits": map[string]any{"memory": "32Mi"}}
	var inits []any
	for i, img := range images {
		inits = append(inits, map[string]any{"name": fmt.Sprintf("pull-%d", i), "image": img, "imagePullPolicy": "IfNotPresent",
			"command": []any{"true"}, "securityContext": security, "resources": resources})
	}
	labels := map[string]any{"app": PrepullName}
	return map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "DaemonSet",
		"metadata": map[string]any{
			"name": PrepullName, "namespace": ns, "labels": labels,
			"annotations": map[string]any{"dhbw.cloud/images": hex.EncodeToString(sum[:8])},
		},
		"spec": map[string]any{
			"selector":       map[string]any{"matchLabels": labels},
			"updateStrategy": map[string]any{"type": "RollingUpdate", "rollingUpdate": map[string]any{"maxUnavailable": "100%"}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": labels},
				"spec": map[string]any{
					"automountServiceAccountToken": false,
					"enableServiceLinks":           false,
					"affinity": map[string]any{"nodeAffinity": map[string]any{"requiredDuringSchedulingIgnoredDuringExecution": map[string]any{
						"nodeSelectorTerms": []any{map[string]any{"matchExpressions": []any{map[string]any{"key": "dhbw.cloud/node-type", "operator": "In", "values": []any{"cpu", "gpu"}}}}},
					}}},
					"tolerations":    []any{map[string]any{"key": "dhbw.cloud/gpu-lab", "operator": "Equal", "value": "true", "effect": "NoSchedule"}},
					"initContainers": inits,
					"containers": []any{map[string]any{"name": "hold", "image": images[0], "imagePullPolicy": "IfNotPresent",
						"command": []any{"sleep", "infinity"}, "securityContext": security, "resources": resources}},
				},
			},
		},
	}
}

// daemonSets is what EnsurePrepull needs of the Kubernetes API.
type daemonSets interface {
	getDaemonSet(ctx context.Context, ns, name string) (map[string]any, error)
	putDaemonSet(ctx context.Context, ns, name string, ds map[string]any, create bool) error
}

func (k *kube) getDaemonSet(ctx context.Context, ns, name string) (map[string]any, error) {
	var out map[string]any
	err := k.do(ctx, http.MethodGet, "/apis/apps/v1/namespaces/"+ns+"/daemonsets/"+name, nil, &out)
	return out, err
}

func (k *kube) putDaemonSet(ctx context.Context, ns, name string, ds map[string]any, create bool) error {
	if create {
		return k.do(ctx, http.MethodPost, "/apis/apps/v1/namespaces/"+ns+"/daemonsets", ds, nil)
	}
	return k.do(ctx, http.MethodPut, "/apis/apps/v1/namespaces/"+ns+"/daemonsets/"+name, ds, nil)
}

// EnsurePrepull keeps the DaemonSet PrepullName at the given images plus notebook-base; it changes it only when the list changed.
func (s *Service) EnsurePrepull(ctx context.Context, images []string) error {
	ds, ok := s.pods.(daemonSets)
	if !ok || s.cfg.CourseBase == "" {
		return nil
	}
	ns := s.cfg.WorkspaceNamespace
	if ns == "" {
		ns = "jupyterhub"
	}
	list := []string{s.cfg.CourseBase}
	for _, img := range images {
		if img = strings.TrimSpace(img); img != "" && !slices.Contains(list, img) {
			list = append(list, img)
		}
	}
	slices.Sort(list[1:])
	want := prepullDaemonSet(ns, list)
	cur, err := ds.getDaemonSet(ctx, ns, PrepullName)
	if errors.Is(err, ErrNotFound) {
		return ds.putDaemonSet(ctx, ns, PrepullName, want, true)
	}
	if err != nil {
		return err
	}
	meta, _ := cur["metadata"].(map[string]any)
	annotations, _ := meta["annotations"].(map[string]any)
	wantAnn := want["metadata"].(map[string]any)["annotations"].(map[string]any)["dhbw.cloud/images"]
	if annotations["dhbw.cloud/images"] == wantAnn {
		return nil
	}
	want["metadata"].(map[string]any)["resourceVersion"] = meta["resourceVersion"]
	return ds.putDaemonSet(ctx, ns, PrepullName, want, false)
}
