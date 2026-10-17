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

	"github.com/pfisterer/llm-management-api/internal/workspace"
)

// Project images from Git (docs/bauplan.md 10.12): the environment of a project's servers in every profile. Built from a Git repository the way Binder reads it — apt.txt, environment.yml, requirements.txt, postBuild, in binder/ or .binder/ if there is one — on the base the project chose (dhbw-workspace-images: Python, PyTorch, TensorFlow), so the result has everything the profiles need and the label cloud.dhbw.base. A repository with a Dockerfile is refused: such an image is built by its owners and entered in the project as a finished image.

// projectImageScript is the build job's script: it writes the Dockerfile from the repository's files and builds it with BuildKit.
const projectImageScript = `set -euo pipefail
git clone --quiet --no-checkout "$GIT_URL" /tmp/repo
git -C /tmp/repo checkout --quiet "$GIT_COMMIT"
dir=/tmp/repo
for d in binder .binder; do if [ -d "/tmp/repo/$d" ]; then dir="/tmp/repo/$d"; break; fi; done
if [ -f "$dir/Dockerfile" ]; then
  echo "dhbw: Das Repository hat ein Dockerfile. Aus Git wird das Projekt-Image aus requirements.txt, environment.yml, apt.txt und postBuild gebaut; ein eigenes Image (FROM …/platform/notebook-base) selbst bauen und im Projekt als fertiges Image eintragen."
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
    # The base keeps its Python: a python pin in the file (e.g. python=3.12) cannot be met by updating the base environment, so it is dropped.
    echo 'RUN sed -i -E "/^[[:space:]]*-[[:space:]]*python([[:space:]]*[<>=!~].*)?[[:space:]]*$/d" /tmp/dhbw/environment.yml && mamba env update -n base -f /tmp/dhbw/environment.yml && mamba clean --all -f -y'
  fi
  if [ -f "$dir/requirements.txt" ]; then
    echo 'COPY --chown=${NB_UID}:${NB_GID} requirements.txt /tmp/dhbw/requirements.txt'
    echo 'RUN pip install --no-cache-dir -r /tmp/dhbw/requirements.txt'
  fi
  if [ -f "$dir/postBuild" ]; then
    echo 'COPY --chown=${NB_UID}:${NB_GID} postBuild /tmp/dhbw/postBuild'
    echo 'RUN bash /tmp/dhbw/postBuild'
  fi
  # As root, as in the Jupyter images: the person may not change the group of files the base or a step left to root (e.g. ~/.ipython).
  echo 'USER root'
  echo 'RUN fix-permissions "${CONDA_DIR}" && fix-permissions "/home/${NB_USER}"'
  echo 'USER ${NB_UID}'
  echo "LABEL org.dhbw.git.url=\"$GIT_URL\" org.dhbw.git.branch=\"$GIT_BRANCH\""
} > /tmp/dockerfile/Dockerfile
cat /tmp/dockerfile/Dockerfile
buildctl-daemonless.sh build --progress=plain --frontend dockerfile.v0 --local context="$dir" --local dockerfile=/tmp/dockerfile \
  --output "type=image,name=$IMAGE,push=true" \
  --export-cache "type=registry,ref=$R2D_BUILDKIT_CACHE-course,mode=max" --import-cache "type=registry,ref=$R2D_BUILDKIT_CACHE-course"`

var tagUnsafe = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

// projectImageTag is the tag of a project image: the commit, its base and the base's release, so another base or a new release is a new image (0123456789ab-pytorch-v2.0.0).
func projectImageTag(commit, base string) string {
	repo, v := splitImage(base)
	name := strings.TrimPrefix(repo[strings.LastIndex(repo, "/")+1:], "base-")
	return commit[:12] + "-" + tagUnsafe.ReplaceAllString(name+"-"+v, "-")
}

// splitImage splits an image into its repository and its tag (or digest without "sha256:").
func splitImage(image string) (string, string) {
	if i := strings.LastIndex(image, "@"); i >= 0 {
		return image[:i], strings.TrimPrefix(image[i+1:], "sha256:")
	}
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		return image[:i], image[i+1:]
	}
	return image, "latest"
}

// SetBases sets the current bases of the projects' environments at their release: what a newer release of a base rebuilds to (RebaseProjectImages), and what is pulled onto the nodes (EnsurePrepull).
func (s *Service) SetBases(images []string) { s.bases = slices.Clone(images) }

// currentBase is the current release of the base an image was built on; false when that base is no longer offered.
func (s *Service) currentBase(base string) (string, bool) {
	repo, _ := splitImage(base)
	for _, b := range s.bases {
		if r, _ := splitImage(b); r == repo {
			return b, true
		}
	}
	return "", false
}

// projectOwner is the owner of a project's image builds.
func projectOwner(slug string) string { return "project:" + slug }

// ProjectOf is the project slug of a build's owner; false for anything else.
func ProjectOf(e Environment) (string, bool) { return strings.CutPrefix(e.Owner, "project:") }

// SetBuildReady is told when a project image build is ready (also a rebuild on a new base), so the project uses the new image.
func (s *Service) SetBuildReady(f func(ctx context.Context, e Environment) error) { s.ready = f }

// latest is the project's current build: the newest of its owner; nil = none.
func (s *Service) latest(ctx context.Context, slug string) (*Environment, error) {
	envs, err := s.store.List(ctx, projectOwner(slug))
	if err != nil || len(envs) == 0 {
		return nil, err
	}
	return &envs[0], nil // newest first
}

func buildOf(e Environment) workspace.ImageBuild {
	return workspace.ImageBuild{Status: string(e.Status), Image: e.Image, Commit: e.Commit, Branch: e.Branch, Message: e.Message, UpdatedAt: e.UpdatedAt}
}

// BuildProjectImage builds the project's image from a repository on base: an unchanged base, repository and ref keep the current build unless rebuild is set, which resolves the ref again (a branch's newest commit) and builds a failed image anew. Older builds of the project are forgotten; their images stay in Harbor until its retention removes them.
func (s *Service) BuildProjectImage(ctx context.Context, slug, base, gitURL, ref string, rebuild bool) (workspace.ImageBuild, error) {
	cur, err := s.latest(ctx, slug)
	if err != nil {
		return workspace.ImageBuild{}, err
	}
	canonical, _, err := s.normaliseRepo(gitURL)
	if err != nil {
		return workspace.ImageBuild{}, err
	}
	ref = strings.TrimSpace(ref)
	if cur != nil && !rebuild && cur.GitURL == canonical && cur.Ref == ref && cur.Base == base {
		return buildOf(s.refresh(ctx, *cur)), nil
	}
	e, err := s.create(ctx, projectOwner(slug), base, canonical, ref, slug)
	if err != nil {
		return workspace.ImageBuild{}, err
	}
	if rebuild && cur != nil && cur.Image == e.Image && cur.Status == StatusFailed {
		// The same commit failed before: build it again instead of reporting the old failure.
		if err := s.startBuildIfMissing(ctx, &e, slugOf(e.Image), tagOf(e.Image), true); err != nil {
			return workspace.ImageBuild{}, err
		}
	}
	if e, err = s.store.Save(ctx, e); err != nil {
		return workspace.ImageBuild{}, err
	}
	if err := s.forgetOthers(ctx, slug, e.ID); err != nil {
		return workspace.ImageBuild{}, err
	}
	return buildOf(e), nil
}

// CheckGitBranch checks a repository for a project's course material (workspace.Project.MaterialGitURL): public, on an allowed Git host, with that branch (empty = its default branch). The servers' checkout follows a branch, so a tag or commit is refused.
func (s *Service) CheckGitBranch(ctx context.Context, gitURL, branch string, auth *workspace.Basic) (string, error) {
	canonical, _, err := s.normaliseRepo(gitURL)
	if err != nil {
		return "", err
	}
	refs, err := s.refs(ctx, canonical, auth)
	if err != nil {
		return "", err
	}
	if branch == "" {
		if refs.DefaultBranch == "" {
			return "", errors.New("das Repository hat keinen Standard-Branch")
		}
		return canonical, nil
	}
	if refs.Refs["refs/heads/"+branch] == "" {
		return "", fmt.Errorf("Branch %q nicht gefunden (Material folgt einem Branch, nicht einem Tag oder Commit)", branch)
	}
	return canonical, nil
}

func (s *Service) forgetOthers(ctx context.Context, slug string, keep uint) error {
	envs, err := s.store.List(ctx, projectOwner(slug))
	if err != nil {
		return err
	}
	for _, e := range envs {
		if e.ID != keep {
			if err := s.store.Delete(ctx, e.ID); err != nil && !errors.Is(err, ErrNotFound) {
				return err
			}
		}
	}
	return nil
}

// ProjectImageBuild is the state of the project's current build, followed up first (a finished build hands its image to the project); nil = none.
func (s *Service) ProjectImageBuild(ctx context.Context, slug string) (*workspace.ImageBuild, error) {
	cur, err := s.latest(ctx, slug)
	if err != nil || cur == nil {
		return nil, err
	}
	b := buildOf(s.refresh(ctx, *cur))
	return &b, nil
}

// ProjectImageLog is the tail of the current build's log.
func (s *Service) ProjectImageLog(ctx context.Context, slug string) (workspace.BuildLog, error) {
	cur, err := s.latest(ctx, slug)
	if err != nil {
		return workspace.BuildLog{}, err
	}
	if cur == nil || cur.JobName == "" {
		return workspace.BuildLog{}, ErrNotFound
	}
	state, since, err := s.builds.jobState(ctx, s.cfg.BuildNamespace, cur.JobName)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return workspace.BuildLog{}, err
	}
	// A pod that has not started yet has no log; its state says why.
	log, err := s.builds.jobLog(ctx, s.cfg.BuildNamespace, cur.JobName, 400)
	if err != nil {
		log = ""
	}
	return workspace.BuildLog{Log: log, State: state, Since: since}, nil
}

// ForgetProjectImage drops the project's builds (the project no longer builds from Git, or is gone).
func (s *Service) ForgetProjectImage(ctx context.Context, slug string) error {
	return s.forgetOthers(ctx, slug, 0)
}

// RebaseProjectImages rebuilds the project images whose base has a newer release (SetBases), and follows the builds under way: a ready one hands its image to its project (SetBuildReady). Run periodically.
func (s *Service) RebaseProjectImages(ctx context.Context) error {
	envs, err := s.store.List(ctx, "")
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range envs {
		if _, ok := ProjectOf(e); !ok {
			continue
		}
		e = s.refresh(ctx, e)
		base, ok := s.currentBase(e.Base)
		if !ok || e.Base == base || e.Status == StatusBuilding {
			continue
		}
		slug := slugOf(e.Image)
		tag := projectImageTag(e.Commit, base)
		e.Base, e.Image = base, fmt.Sprintf("%s/%s/%s:%s", s.cfg.Registry, s.cfg.EnvsProject, slug, tag)
		if err := s.startBuildIfMissing(ctx, &e, slug, tag, false); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", e.Image, err))
			continue
		}
		if e.Status == StatusReady && s.ready != nil {
			// Built before (another project with the same repository): the project follows now.
			if err := s.ready(ctx, e); err != nil {
				errs = append(errs, err)
			}
		}
		if _, err := s.store.Save(ctx, e); err != nil {
			errs = append(errs, err)
		}
		s.log.Infow("project image rebuilt on new base", "owner", e.Owner, "image", e.Image, "status", e.Status)
	}
	return errors.Join(errs...)
}

// PrepullName is the DaemonSet that keeps the course images on the CPU and GPU nodes, so that the first start of a server or GPU kernel in a course does not wait for a pull of several GB.
const PrepullName = "kernel-image-prepull"

// prepullSpecRevision changes with the DaemonSet's spec (2: amd64 nodes only; 3: images as image volumes).
const prepullSpecRevision = "3"

// prepullDaemonSet keeps hold (a base, which has sleep) running and mounts every other image as an image volume: the kubelet pulls them and keeps them while the pod runs, but never starts them, so an image without a shell (distroless) or with a broken entrypoint holds up nothing. Images whose registry does not answer are left out before (workspace.Service.ProjectImages), since the pod starts only when all its volumes are pulled.
func prepullDaemonSet(ns, name, arch, hold string, images, pullSecrets []string) map[string]any {
	// The spec revision goes into the hash, so a change of the spec below reaches a DaemonSet whose images stayed the same.
	sum := sha256.Sum256([]byte(prepullSpecRevision + "\n" + hold + "\n" + strings.Join(images, "\n") + "\n" + strings.Join(pullSecrets, "\n")))
	security := map[string]any{"runAsNonRoot": true, "runAsUser": 1000, "allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": map[string]any{"drop": []any{"ALL"}}}
	resources := map[string]any{"requests": map[string]any{"cpu": "1m", "memory": "8Mi"}, "limits": map[string]any{"memory": "32Mi"}}
	var volumes, mounts []any
	for i, img := range images {
		name := fmt.Sprintf("image-%d", i)
		volumes = append(volumes, map[string]any{"name": name, "image": map[string]any{"reference": img, "pullPolicy": "IfNotPresent"}})
		// The kubelet pulls only volumes a container mounts.
		mounts = append(mounts, map[string]any{"name": name, "mountPath": "/prepull/" + name, "readOnly": true})
	}
	labels := map[string]any{"app": name}
	podSpec := map[string]any{
		"automountServiceAccountToken": false,
		"enableServiceLinks":           false,
		"affinity": map[string]any{"nodeAffinity": map[string]any{"requiredDuringSchedulingIgnoredDuringExecution": map[string]any{
			// Only nodes of the DaemonSet's architecture: an image without a variant for the node would hang in ErrImagePull.
			"nodeSelectorTerms": []any{map[string]any{"matchExpressions": []any{
				map[string]any{"key": "dhbw.cloud/node-type", "operator": "In", "values": []any{"cpu", "gpu"}},
				map[string]any{"key": "kubernetes.io/arch", "operator": "In", "values": []any{arch}},
			}}},
		}}},
		"tolerations": []any{map[string]any{"key": "dhbw.cloud/gpu-lab", "operator": "Equal", "value": "true", "effect": "NoSchedule"}},
		"containers": []any{map[string]any{"name": "hold", "image": hold, "imagePullPolicy": "IfNotPresent",
			"command": []any{"sleep", "infinity"}, "securityContext": security, "resources": resources, "volumeMounts": mounts}},
	}
	if len(pullSecrets) > 0 {
		var refs []any
		for _, n := range pullSecrets {
			refs = append(refs, map[string]any{"name": n})
		}
		podSpec["imagePullSecrets"] = refs
	}
	if len(volumes) > 0 {
		podSpec["volumes"] = volumes
	} else {
		delete(podSpec["containers"].([]any)[0].(map[string]any), "volumeMounts")
	}
	return map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "DaemonSet",
		"metadata": map[string]any{
			"name": name, "namespace": ns, "labels": labels,
			"annotations": map[string]any{"dhbw.cloud/images": hex.EncodeToString(sum[:8])},
		},
		"spec": map[string]any{
			"selector":       map[string]any{"matchLabels": labels},
			"updateStrategy": map[string]any{"type": "RollingUpdate", "rollingUpdate": map[string]any{"maxUnavailable": "100%"}},
			"template":       map[string]any{"metadata": map[string]any{"labels": labels}, "spec": podSpec},
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

// EnsurePrepull keeps one DaemonSet per architecture (kernel-image-prepull for amd64, kernel-image-prepull-<arch> for the others) that holds the images of that architecture on its CPU and GPU nodes: the first base of it keeps the pod running (one of ours, with sleep and user 1000), the others are image volumes. An architecture without a base gets none.
func (s *Service) EnsurePrepull(ctx context.Context, images []workspace.ArchImage) error {
	ds, ok := s.pods.(daemonSets)
	if !ok {
		return nil
	}
	ns := s.cfg.WorkspaceNamespace
	if ns == "" {
		ns = "jupyterhub"
	}
	var errs []error
	for _, arch := range prepullArchs {
		hold := ""
		for _, img := range images {
			if img.Base && slices.Contains(img.Archs, arch) {
				hold = strings.TrimSpace(img.Image)
				break
			}
		}
		var list, secrets []string
		for _, img := range images {
			name := strings.TrimSpace(img.Image)
			if name != "" && name != hold && slices.Contains(img.Archs, arch) && !slices.Contains(list, name) {
				list = append(list, name)
				// A private image is pulled with its project's secret; the kubelet still checks every other pod's own credentials before it uses the image (KubeletEnsureSecretPulledImages).
				if img.PullSecret != "" && !slices.Contains(secrets, img.PullSecret) {
					secrets = append(secrets, img.PullSecret)
				}
			}
		}
		if hold == "" {
			s.prepulled.Store(arch, 0)
			continue
		}
		slices.Sort(list)
		slices.Sort(secrets)
		if err := s.putPrepull(ctx, ds, ns, prepullName(arch), prepullDaemonSet(ns, prepullName(arch), arch, hold, list, secrets)); err != nil {
			errs = append(errs, err)
			continue
		}
		s.prepulled.Store(arch, 1+len(list))
	}
	return errors.Join(errs...)
}

// prepullArchs are the architectures of the cluster's nodes.
var prepullArchs = []string{"amd64", "arm64"}

// prepullName is the DaemonSet of an architecture; amd64 keeps the name from before there were others.
func prepullName(arch string) string {
	if arch == "amd64" {
		return PrepullName
	}
	return PrepullName + "-" + arch
}

func (s *Service) putPrepull(ctx context.Context, ds daemonSets, ns, name string, want map[string]any) error {
	cur, err := ds.getDaemonSet(ctx, ns, name)
	if errors.Is(err, ErrNotFound) {
		return ds.putDaemonSet(ctx, ns, name, want, true)
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
	return ds.putDaemonSet(ctx, ns, name, want, false)
}
