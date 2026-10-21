package gpu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// registry reads Harbor's API anonymously: the projects platform and envs are
// public. Pushing is done by the build jobs with their own robot account.
type registry struct {
	host string
	http *http.Client

	mu          sync.Mutex
	builder     string
	builderTime time.Time
}

func newRegistry(host string) *registry {
	return &registry{host: host, http: withDNSRetry(&http.Client{Timeout: 15 * time.Second})}
}

func (r *registry) artifactURL(project, repo, ref string) string {
	return fmt.Sprintf("https://%s/api/v2.0/projects/%s/repositories/%s/artifacts/%s", r.host, project, url.PathEscape(url.PathEscape(repo)), url.PathEscape(ref))
}

// exists reports whether project/repo:tag is in the registry.
func (r *registry) exists(ctx context.Context, project, repo, tag string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.artifactURL(project, repo, tag), nil)
	if err != nil {
		return false, err
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("harbor: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("harbor: %s %d", r.artifactURL(project, repo, tag), resp.StatusCode)
	}
}

// newestBuilder returns the builder at the most recently pushed tag of platform/repo2docker-buildkit (cached for a few minutes). A builder for several architectures (images.yaml platforms) has its variants as <tag>-<arch> beside the index <tag>: those are skipped, so every build job gets the index and its node pulls its own variant.
func (r *registry) newestBuilder(ctx context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.builder != "" && time.Since(r.builderTime) < 5*time.Minute {
		return r.builder, nil
	}
	u := fmt.Sprintf("https://%s/api/v2.0/projects/platform/repositories/repo2docker-buildkit/artifacts?page_size=20&sort=-push_time&with_tag=true", r.host)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("harbor: %w", err)
	}
	defer resp.Body.Close()
	var arts []struct {
		Tags []struct {
			Name string `json:"name"`
		} `json:"tags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&arts); err != nil {
		return "", fmt.Errorf("harbor: %w", err)
	}
	tag := ""
	for _, a := range arts {
		for _, t := range a.Tags {
			if tag == "" && !archTagged(t.Name) {
				tag = t.Name
			}
		}
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
