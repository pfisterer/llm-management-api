package oci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Harbor is Harbor's v2 API for one registry host, as far as the image cleanup needs it: list a project's repositories and their artifacts, delete artifacts and repositories. Auth is a robot account (nil = anonymously, which lists a public project but deletes nothing).
type Harbor struct {
	Client *Client
	Host   string
	Auth   *Auth
}

// HarborRepository is a repository in a Harbor project; Name is without the project ("kurs", not "envs/kurs").
type HarborRepository struct {
	Name          string
	ArtifactCount int
	UpdateTime    time.Time
}

// HarborArtifact is an artifact (an image or an index) in a repository: its digest, its tags, and when it was last pushed and pulled (zero = never pulled).
type HarborArtifact struct {
	Digest   string
	Tags     []string
	PushTime time.Time
	PullTime time.Time
}

// harborPageSize is the most Harbor returns per page.
const harborPageSize = 100

func (h *Harbor) api(path string) string {
	scheme := h.Client.Scheme
	if scheme == "" {
		scheme = "https"
	}
	return scheme + "://" + h.Host + "/api/v2.0" + path
}

// repoPath is the API path of a repository: Harbor wants slashes in its name escaped twice.
func (h *Harbor) repoPath(project, repo string) string {
	return "/projects/" + url.PathEscape(project) + "/repositories/" + url.PathEscape(url.PathEscape(repo))
}

func (h *Harbor) do(ctx context.Context, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, h.api(path), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if h.Auth != nil {
		req.SetBasicAuth(h.Auth.Username, h.Auth.Password)
	}
	resp, err := h.Client.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("harbor %s: %w", h.Host, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case resp.StatusCode/100 != 2:
		// The path only, without the query: no credentials in it, but nothing else to log either.
		return fmt.Errorf("harbor %s: %s %s: %d", h.Host, method, strings.SplitN(path, "?", 2)[0], resp.StatusCode)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out)
}

// pages reads a listing page by page until a page is not full.
func pages[T any](ctx context.Context, h *Harbor, path string) ([]T, error) {
	var all []T
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	for page := 1; ; page++ {
		var batch []T
		if err := h.do(ctx, http.MethodGet, fmt.Sprintf("%s%spage=%d&page_size=%d", path, sep, page, harborPageSize), &batch); err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if len(batch) < harborPageSize || page >= 1000 {
			return all, nil
		}
	}
}

// Repositories lists the repositories of a project.
func (h *Harbor) Repositories(ctx context.Context, project string) ([]HarborRepository, error) {
	raw, err := pages[struct {
		Name          string    `json:"name"`
		ArtifactCount int       `json:"artifact_count"`
		UpdateTime    time.Time `json:"update_time"`
	}](ctx, h, "/projects/"+url.PathEscape(project)+"/repositories")
	if err != nil {
		return nil, err
	}
	out := make([]HarborRepository, 0, len(raw))
	for _, r := range raw {
		name, ok := strings.CutPrefix(r.Name, project+"/")
		if !ok {
			continue // not of this project: never acted upon
		}
		out = append(out, HarborRepository{Name: name, ArtifactCount: r.ArtifactCount, UpdateTime: r.UpdateTime})
	}
	return out, nil
}

// Artifacts lists the artifacts of a repository with their tags.
func (h *Harbor) Artifacts(ctx context.Context, project, repo string) ([]HarborArtifact, error) {
	raw, err := pages[struct {
		Digest   string    `json:"digest"`
		PushTime time.Time `json:"push_time"`
		PullTime time.Time `json:"pull_time"`
		Tags     []struct {
			Name string `json:"name"`
		} `json:"tags"`
	}](ctx, h, h.repoPath(project, repo)+"/artifacts?with_tag=true&with_scan_overview=false&with_label=false")
	if err != nil {
		return nil, err
	}
	out := make([]HarborArtifact, 0, len(raw))
	for _, a := range raw {
		art := HarborArtifact{Digest: a.Digest, PushTime: a.PushTime, PullTime: a.PullTime}
		for _, t := range a.Tags {
			art.Tags = append(art.Tags, t.Name)
		}
		out = append(out, art)
	}
	return out, nil
}

// DeleteArtifact deletes an artifact by its digest, with all its tags; one that is gone already counts as deleted. Its blobs go at Harbor's next garbage collection.
func (h *Harbor) DeleteArtifact(ctx context.Context, project, repo, digest string) error {
	if !strings.HasPrefix(digest, "sha256:") {
		return fmt.Errorf("harbor: refusing to delete %s/%s by %q, a digest is required", project, repo, digest)
	}
	err := h.do(ctx, http.MethodDelete, h.repoPath(project, repo)+"/artifacts/"+url.PathEscape(digest), nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// DeleteRepository deletes a repository with all its artifacts; one that is gone already counts as deleted.
func (h *Harbor) DeleteRepository(ctx context.Context, project, repo string) error {
	err := h.do(ctx, http.MethodDelete, h.repoPath(project, repo), nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}
