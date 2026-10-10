package gpu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
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

// newestTag returns the most recently pushed tag of project/repo (cached for a few minutes).
func (r *registry) newestBuilder(ctx context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.builder != "" && time.Since(r.builderTime) < 5*time.Minute {
		return r.builder, nil
	}
	u := fmt.Sprintf("https://%s/api/v2.0/projects/platform/repositories/repo2docker-buildkit/artifacts?page_size=1&sort=-push_time", r.host)
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
	if len(arts) == 0 || len(arts[0].Tags) == 0 {
		return "", errors.New("harbor: no builder image platform/repo2docker-buildkit")
	}
	r.builder = fmt.Sprintf("%s/platform/repo2docker-buildkit:%s", r.host, arts[0].Tags[0].Name)
	r.builderTime = time.Now()
	return r.builder, nil
}
