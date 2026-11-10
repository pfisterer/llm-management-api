package gpu

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/pfisterer/llm-management-api/internal/httpx"
	"github.com/pfisterer/llm-management-api/internal/oci"
)

// registry reads Harbor's API anonymously (oci.Harbor): the projects platform and envs are public. Pushing is done by the build jobs with their own robot account.
type registry struct {
	host   string
	harbor *oci.Harbor

	mu          sync.Mutex
	builder     string
	builderTime time.Time
}

func newRegistry(host string) *registry {
	return &registry{host: host, harbor: &oci.Harbor{Client: &oci.Client{HTTP: httpx.RetryingClient(15 * time.Second)}, Host: host}}
}

// exists reports whether project/repo:tag is in the registry.
func (r *registry) exists(ctx context.Context, project, repo, tag string) (bool, error) {
	return r.harbor.ArtifactExists(ctx, project, repo, tag)
}

// newestBuilder returns the builder at the most recently pushed tag of platform/repo2docker-buildkit (cached for a few minutes). A builder for several architectures (images.yaml platforms) has its variants as <tag>-<arch> beside the index <tag>: those are skipped, so every build job gets the index and its node pulls its own variant.
func (r *registry) newestBuilder(ctx context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.builder != "" && time.Since(r.builderTime) < 5*time.Minute {
		return r.builder, nil
	}
	tag, err := r.harbor.NewestTag(ctx, "platform", "repo2docker-buildkit", archTagged)
	if err != nil {
		return "", err
	}
	if tag == "" {
		return "", errors.New("harbor: no builder image platform/repo2docker-buildkit")
	}
	r.builder = fmt.Sprintf("%s/platform/repo2docker-buildkit:%s", r.host, tag)
	r.builderTime = time.Now()
	return r.builder, nil
}

// archSuffixes are the tag suffixes of the variants of an image built for several architectures (tasks/images-stage.yaml, multi_arch.go).
var archSuffixes = []string{"-amd64", "-arm64"}

// archTagged tells whether a tag is that of one architecture's variant (<tag>-<arch>).
func archTagged(tag string) bool {
	for _, suf := range archSuffixes {
		if strings.HasSuffix(tag, suf) {
			return true
		}
	}
	return false
}
