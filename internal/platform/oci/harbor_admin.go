package oci

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pfisterer/llm-management-api/internal/platform/httpx"
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

// HarborArtifact is an artifact (an image or an index) in a repository: its digest, its tags, when it was last pushed and pulled (zero = never pulled), and for an index the digests of its variants (References). Harbor lists a variant on its own only when it has a tag (e.g. <tag>-arm64 of a build for several architectures); deleting the index deletes its untagged variants with it and leaves the tagged ones, which can be deleted once the index is gone.
type HarborArtifact struct {
	Digest     string
	Tags       []string
	PushTime   time.Time
	PullTime   time.Time
	References []string
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

// HarborError is an answer of Harbor outside 2xx (but 404, which is ErrNotFound), named by the path without the query: no credentials in it, but nothing else to log either.
type HarborError struct {
	Host, Method, Path string
	Status             int
}

func (e *HarborError) Error() string {
	return fmt.Sprintf("harbor %s: %s %s: %d", e.Host, e.Method, e.Path, e.Status)
}

func (h *Harbor) do(ctx context.Context, method, path string, out any) error {
	return h.doHeader(ctx, method, path, nil, out)
}

// doHeader calls Harbor's API with the account of h (anonymously without one) and the extra header hdr: 404 is ErrNotFound, another answer outside 2xx a *HarborError.
func (h *Harbor) doHeader(ctx context.Context, method, path string, hdr http.Header, out any) error {
	header := http.Header{"Accept": {"application/json"}}
	for k, v := range hdr {
		header[k] = v
	}
	if h.Auth != nil {
		header.Set("Authorization", httpx.BasicAuth(h.Auth.Username, h.Auth.Password))
	}
	status, err := httpx.Do(ctx, h.Client.HTTP, httpx.Request{Method: method, URL: h.api(path), Header: header, MaxBody: 16 << 20}, out)
	var se *httpx.StatusError
	switch {
	case errors.As(err, &se) && se.Status == http.StatusNotFound:
		return ErrNotFound
	case se != nil:
		return &HarborError{Host: h.Host, Method: method, Path: strings.SplitN(path, "?", 2)[0], Status: se.Status}
	case err != nil && status == 0:
		return fmt.Errorf("harbor %s: %w", h.Host, err)
	}
	return err
}

// ArtifactExists tells whether project/repo has an artifact at ref (a tag or a digest).
func (h *Harbor) ArtifactExists(ctx context.Context, project, repo, ref string) (bool, error) {
	err := h.do(ctx, http.MethodGet, h.repoPath(project, repo)+"/artifacts/"+url.PathEscape(ref), nil)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// NewestTag is the first tag that skip does not leave out of the most recently pushed tagged artifacts of project/repo (the newest 20); "" when there is none.
func (h *Harbor) NewestTag(ctx context.Context, project, repo string, skip func(tag string) bool) (string, error) {
	var arts []struct {
		Tags []struct {
			Name string `json:"name"`
		} `json:"tags"`
	}
	if err := h.do(ctx, http.MethodGet, h.repoPath(project, repo)+"/artifacts?page_size=20&sort=-push_time&with_tag=true", &arts); err != nil {
		return "", err
	}
	for _, a := range arts {
		for _, t := range a.Tags {
			if skip == nil || !skip(t.Name) {
				return t.Name, nil
			}
		}
	}
	return "", nil
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
		References []struct {
			ChildDigest string `json:"child_digest"`
		} `json:"references"`
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
		for _, r := range a.References {
			if r.ChildDigest != "" {
				art.References = append(art.References, r.ChildDigest)
			}
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
