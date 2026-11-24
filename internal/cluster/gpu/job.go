package gpu

import (
	"net/url"

	"github.com/pfisterer/llm-management-api/internal/cluster/workspace"
)

// buildJob is the Kubernetes Job that builds one project image (projectImageScript, BuildKit in the image platform/repo2docker-buildkit) for one architecture and pushes it as image (byDigest: to the repository image without a tag, a variant for the index, see projectImageScript): amd64 on a build node, any other natively on a node of that architecture (today the GB10s, of which the build uses only the CPUs, like the platform images in tasks/images-stage.yaml). BuildKit runs as root in the pod's own user namespace (hostUsers: false, nothing privileged) and pushes to Harbor with the robot account in BuilderSecret. Same as aiaas-deployment/tests/env-build.sh. The namespace's NetworkPolicy limits the job to the internet, Harbor and DNS. cacheTag is the build cache's tag in envs/buildcache (the repository's slug, plus the architecture for any but amd64).
//
// A private repository (e.Private) is built into the private project with its builder secret, its cache there too, and cloned by the init container clone, the only one that sees the project's access (the Secret <job>-git, gitSecret): it checks the commit out into the volume src, and the build container, in which the repository's setup runs, starts from there without the token.
func buildJob(cfg Config, name, builder string, e Environment, slug, arch, image, cacheTag string, byDigest bool) map[string]any {
	labels := map[string]any{"dhbw.cloud/env": slug, "dhbw.cloud/arch": arch, "app.kubernetes.io/managed-by": "llm-management-api"}
	project, secret := cfg.EnvsProject, cfg.BuilderSecret
	if e.Private {
		project, secret = cfg.PrivateEnvsProject, cfg.PrivateBuilderSecret
	}
	nodeSelector := map[string]any{"dhbw.cloud/node-type": "build", "kubernetes.io/arch": arch}
	podSpec := map[string]any{
		"restartPolicy":                "Never",
		"hostUsers":                    false,
		"automountServiceAccountToken": false,
		"nodeSelector":                 nodeSelector,
		"tolerations":                  []any{map[string]any{"key": "dhbw.cloud/build", "operator": "Equal", "value": "true", "effect": "NoSchedule"}},
		"containers": []any{map[string]any{
			"name":    "build",
			"image":   builder,
			"command": []any{"bash", "-c", projectImageScript},
			"env": []any{
				map[string]any{"name": "GIT_URL", "value": e.GitURL},
				map[string]any{"name": "GIT_COMMIT", "value": e.Commit},
				map[string]any{"name": "GIT_BRANCH", "value": e.Branch},
				map[string]any{"name": "IMAGE", "value": image},
				map[string]any{"name": "BASE_IMAGE", "value": e.Base},
				map[string]any{"name": "BUILD_ARCH", "value": arch},
				map[string]any{"name": "DOCKER_CONFIG", "value": "/docker-config"},
				map[string]any{"name": "R2D_BUILDKIT_CACHE", "value": cfg.Registry + "/" + project + "/" + buildCacheRepo + ":" + cacheTag},
			},
			"securityContext": map[string]any{
				"capabilities":    map[string]any{"add": []any{"SYS_ADMIN"}},
				"procMount":       "Unmasked",
				"seccompProfile":  map[string]any{"type": "Unconfined"},
				"appArmorProfile": map[string]any{"type": "Unconfined"},
			},
			"resources": map[string]any{
				"requests": map[string]any{"cpu": "2", "memory": "4Gi"},
				"limits":   map[string]any{"memory": "12Gi", "ephemeral-storage": "40Gi"},
			},
			"volumeMounts": []any{
				map[string]any{"name": "docker-config", "mountPath": "/docker-config"},
				map[string]any{"name": "state", "mountPath": "/var/lib/buildkit"},
			},
		}},
		"volumes": []any{
			map[string]any{"name": "docker-config", "secret": map[string]any{"secretName": secret,
				"items": []any{map[string]any{"key": ".dockerconfigjson", "path": "config.json"}}}},
			map[string]any{"name": "state", "emptyDir": map[string]any{}},
		},
	}
	// Before the clone and the build (the repository's own code runs in it): in the builder image, as its default user, nothing added.
	withEgressGuard(podSpec, egressGuardInit(builder, cfg.EgressGuard, map[string]any{"allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []any{"ALL"}}}))
	if e.Private {
		withPrivateClone(podSpec, builder, name, e)
	}
	if byDigest {
		c := podSpec["containers"].([]any)[0].(map[string]any)
		c["env"] = append(c["env"].([]any), map[string]any{"name": "PUSH_BY_DIGEST", "value": "1"})
	}
	if arch != "amd64" {
		// There is no build node of that architecture today: any node of it (the GB10s carry the taint of the lab pool), a build node first should one come.
		podSpec["nodeSelector"] = map[string]any{"kubernetes.io/arch": arch}
		podSpec["tolerations"] = append(podSpec["tolerations"].([]any), map[string]any{"key": "dhbw.cloud/gpu-lab", "operator": "Equal", "value": "true", "effect": "NoSchedule"})
		podSpec["affinity"] = map[string]any{"nodeAffinity": map[string]any{"preferredDuringSchedulingIgnoredDuringExecution": []any{map[string]any{
			"weight":     100,
			"preference": map[string]any{"matchExpressions": []any{map[string]any{"key": "dhbw.cloud/node-type", "operator": "In", "values": []any{"build"}}}},
		}}}}
	}
	return map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata":   map[string]any{"name": name, "labels": labels},
		"spec": map[string]any{
			"backoffLimit":            0,
			"ttlSecondsAfterFinished": 86400,
			"activeDeadlineSeconds":   3600,
			"template":                map[string]any{"metadata": map[string]any{"labels": labels}, "spec": podSpec},
		},
	}
}

// srcDir is where the clone container of a private build puts the repository for the build container.
const srcDir = "/src/repo"

// withPrivateClone adds the init container clone after the egress guard: it clones with git's credential store from the Secret <job>-git (mounted only here) and checks out the commit into the volume src; the build container gets the volume and PRECLONED, and skips its own clone.
func withPrivateClone(podSpec map[string]any, builder, job string, e Environment) {
	inits, _ := podSpec["initContainers"].([]any)
	podSpec["initContainers"] = append(inits, map[string]any{
		"name":    "clone",
		"image":   builder,
		"command": []any{"bash", "-c", projectImageCloneScript},
		"env": []any{
			map[string]any{"name": "GIT_URL", "value": e.GitURL},
			map[string]any{"name": "GIT_COMMIT", "value": e.Commit},
			map[string]any{"name": "REPO_DIR", "value": srcDir},
			// git reads the helper from the environment, so no file in the repository names it.
			map[string]any{"name": "GIT_CONFIG_COUNT", "value": "1"},
			map[string]any{"name": "GIT_CONFIG_KEY_0", "value": "credential.helper"},
			map[string]any{"name": "GIT_CONFIG_VALUE_0", "value": "store --file=/git/credentials"},
		},
		"securityContext": map[string]any{"allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []any{"ALL"}}},
		"resources":       map[string]any{"requests": map[string]any{"cpu": "100m", "memory": "128Mi"}, "limits": map[string]any{"memory": "1Gi"}},
		"volumeMounts": []any{
			map[string]any{"name": "git", "mountPath": "/git", "readOnly": true},
			map[string]any{"name": "src", "mountPath": "/src"},
		},
	})
	c := podSpec["containers"].([]any)[0].(map[string]any)
	c["env"] = append(c["env"].([]any), map[string]any{"name": "PRECLONED", "value": "1"}, map[string]any{"name": "REPO_DIR", "value": srcDir})
	c["volumeMounts"] = append(c["volumeMounts"].([]any), map[string]any{"name": "src", "mountPath": "/src"})
	podSpec["volumes"] = append(podSpec["volumes"].([]any),
		map[string]any{"name": "git", "secret": map[string]any{"secretName": gitSecretName(job)}},
		map[string]any{"name": "src", "emptyDir": map[string]any{}},
	)
}

// gitSecretName is the Secret with a private build's access to its Git host.
func gitSecretName(job string) string { return job + "-git" }

// gitSecret holds a private build's access in git's credential store format (https://user:token@host), owned by its Job (uid; empty only in tests) so that Kubernetes deletes it with the Job. The admission policy build-secrets lets the service create no other Secret in the build namespace.
func gitSecret(job, jobUID, gitURL string, auth workspace.Basic) map[string]any {
	meta := map[string]any{"name": gitSecretName(job), "labels": map[string]any{"app.kubernetes.io/managed-by": "llm-management-api", "dhbw.cloud/build-git": "true"}}
	if jobUID != "" {
		meta["ownerReferences"] = []any{map[string]any{"apiVersion": "batch/v1", "kind": "Job", "name": job, "uid": jobUID}}
	}
	line := "https://" + url.QueryEscape(auth.Username) + ":" + url.QueryEscape(auth.Token) + "@" + hostOf(gitURL) + "\n"
	return map[string]any{"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "metadata": meta, "stringData": map[string]any{"credentials": line}}
}

// builderSecret is the secret a build of e pushes with.
func (s *Service) builderSecret(e Environment) string {
	if e.Private {
		return s.cfg.PrivateBuilderSecret
	}
	return s.cfg.BuilderSecret
}

// DefaultIndexImage writes the index of a project image built for several architectures (the same crane as tasks/images-stage.yaml).
const DefaultIndexImage = "gcr.io/go-containerregistry/crane:v0.20.3"

// indexJob joins the variants of a project image (repository@digest, one per architecture whose build succeeded, pushed without a tag) in an index under image, so a node pulls its own variant; in Harbor they are then the index's children, listed and deleted with it. crane only copies manifests within Harbor; nothing is pulled. It takes each variant's platform from its config, so the index names linux/amd64 and linux/arm64 (oci.Client.Inspect reads the architectures from there).
func indexJob(cfg Config, name, slug, image string, variants []string, secret string) map[string]any {
	indexImage := cfg.IndexImage
	if indexImage == "" {
		indexImage = DefaultIndexImage
	}
	args := []any{"index", "append", "-t", image}
	for _, v := range variants {
		args = append(args, "-m="+v)
	}
	labels := map[string]any{"dhbw.cloud/env": slug, "dhbw.cloud/arch": "index", "app.kubernetes.io/managed-by": "llm-management-api"}
	return map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata":   map[string]any{"name": name, "labels": labels},
		"spec": map[string]any{
			"backoffLimit":            2,
			"ttlSecondsAfterFinished": 86400,
			"activeDeadlineSeconds":   600,
			"template": map[string]any{
				"metadata": map[string]any{"labels": labels},
				"spec": map[string]any{
					"restartPolicy":                "Never",
					"automountServiceAccountToken": false,
					"nodeSelector":                 map[string]any{"dhbw.cloud/node-type": "build"},
					"tolerations":                  []any{map[string]any{"key": "dhbw.cloud/build", "operator": "Equal", "value": "true", "effect": "NoSchedule"}},
					"containers": []any{map[string]any{
						"name":  "index",
						"image": indexImage,
						"args":  args,
						"env":   []any{map[string]any{"name": "DOCKER_CONFIG", "value": "/docker-config"}},
						"securityContext": map[string]any{
							"runAsNonRoot": true, "runAsUser": 65532, "allowPrivilegeEscalation": false,
							"capabilities": map[string]any{"drop": []any{"ALL"}},
						},
						"resources":    map[string]any{"requests": map[string]any{"cpu": "50m", "memory": "64Mi"}, "limits": map[string]any{"memory": "256Mi"}},
						"volumeMounts": []any{map[string]any{"name": "docker-config", "mountPath": "/docker-config", "readOnly": true}},
					}},
					"volumes": []any{map[string]any{"name": "docker-config", "secret": map[string]any{"secretName": secret,
						"items": []any{map[string]any{"key": ".dockerconfigjson", "path": "config.json"}}}}},
				},
			},
		},
	}
}
