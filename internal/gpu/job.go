package gpu

// buildJob is the Kubernetes Job that builds one project image (projectImageScript, BuildKit in the image platform/repo2docker-buildkit) for one architecture and pushes it as image (byDigest: to the repository image without a tag, a variant for the index, see projectImageScript): amd64 on a build node, any other natively on a node of that architecture (docs/todo.md multi_arch; today the GB10s, of which the build uses only the CPUs, like the platform images in tasks/images-stage.yaml). BuildKit runs as root in the pod's own user namespace (hostUsers: false, nothing privileged) and pushes to Harbor with the robot account in BuilderSecret. Same as aiaas-deployment/tests/env-build.sh. The namespace's NetworkPolicy limits the job to the internet, Harbor and DNS. cacheTag is the build cache's tag in envs/buildcache (the repository's slug, plus the architecture for any but amd64).
func buildJob(cfg Config, name, builder string, e Environment, slug, arch, image, cacheTag string, byDigest bool) map[string]any {
	labels := map[string]any{"dhbw.cloud/env": slug, "dhbw.cloud/arch": arch, "app.kubernetes.io/managed-by": "llm-management-api"}
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
				map[string]any{"name": "R2D_BUILDKIT_CACHE", "value": cfg.Registry + "/" + cfg.EnvsProject + "/" + buildCacheRepo + ":" + cacheTag},
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
			map[string]any{"name": "docker-config", "secret": map[string]any{"secretName": cfg.BuilderSecret,
				"items": []any{map[string]any{"key": ".dockerconfigjson", "path": "config.json"}}}},
			map[string]any{"name": "state", "emptyDir": map[string]any{}},
		},
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

// DefaultIndexImage writes the index of a project image built for several architectures (the same crane as tasks/images-stage.yaml).
const DefaultIndexImage = "gcr.io/go-containerregistry/crane:v0.20.3"

// indexJob joins the variants of a project image (repository@digest, one per architecture whose build succeeded, pushed without a tag) in an index under image, so a node pulls its own variant; in Harbor they are then the index's children, listed and deleted with it. crane only copies manifests within Harbor; nothing is pulled. It takes each variant's platform from its config, so the index names linux/amd64 and linux/arm64 (oci.Client.Inspect reads the architectures from there).
func indexJob(cfg Config, name, slug, image string, variants []string) map[string]any {
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
					"volumes": []any{map[string]any{"name": "docker-config", "secret": map[string]any{"secretName": cfg.BuilderSecret,
						"items": []any{map[string]any{"key": ".dockerconfigjson", "path": "config.json"}}}}},
				},
			},
		},
	}
}
