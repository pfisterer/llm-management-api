package gpu

// buildJob is the Kubernetes Job that builds one environment: repo2docker with
// the BuildKit engine (image platform/repo2docker-buildkit) on a build node,
// BuildKit as root in the pod's own user namespace (hostUsers: false, nothing
// privileged), push to Harbor with the robot account in BuilderSecret. Same as
// dhbw-ai-service/tests/env-build.sh. The namespace's NetworkPolicy limits the job to
// the internet, Harbor and DNS.
func buildJob(cfg Config, name, builder, gitURL, commit, branch, image, slug string) map[string]any {
	script := `repo2docker --engine buildkit --no-run --push --user-id 1000 --user-name jovyan --ref "$GIT_COMMIT" ` +
		`--appendix "$(cat /etc/r2d/appendix.Dockerfile)" --label "org.dhbw.git.url=$GIT_URL" --label "org.dhbw.git.branch=$GIT_BRANCH" ` +
		`--image-name "$IMAGE" "$GIT_URL"`
	return map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata": map[string]any{
			"name":   name,
			"labels": map[string]any{"dhbw.cloud/env": slug, "app.kubernetes.io/managed-by": "llm-management-api"},
		},
		"spec": map[string]any{
			"backoffLimit":            0,
			"ttlSecondsAfterFinished": 86400,
			"activeDeadlineSeconds":   3600,
			"template": map[string]any{
				"spec": map[string]any{
					"restartPolicy":                "Never",
					"hostUsers":                    false,
					"automountServiceAccountToken": false,
					"nodeSelector":                 map[string]any{"dhbw.cloud/node-type": "build"},
					"tolerations":                  []any{map[string]any{"key": "dhbw.cloud/build", "operator": "Equal", "value": "true", "effect": "NoSchedule"}},
					"containers": []any{map[string]any{
						"name":    "build",
						"image":   builder,
						"command": []any{"bash", "-c", script},
						"env": []any{
							map[string]any{"name": "GIT_URL", "value": gitURL},
							map[string]any{"name": "GIT_COMMIT", "value": commit},
							map[string]any{"name": "GIT_BRANCH", "value": branch},
							map[string]any{"name": "IMAGE", "value": image},
							map[string]any{"name": "DOCKER_CONFIG", "value": "/docker-config"},
							map[string]any{"name": "R2D_BUILDKIT_CACHE", "value": cfg.Registry + "/" + cfg.EnvsProject + "/buildcache:" + slug},
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
				},
			},
		},
	}
}
