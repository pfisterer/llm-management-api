// Package oci reads a container image's digest and labels from its registry (OCI distribution API), anonymously as a node would pull it: public images only.
package oci

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// ErrNotFound is an image the registry does not have or does not give out without credentials.
var ErrNotFound = errors.New("image not found or not public")

// Ref is an image reference split into registry host, repository and tag or digest.
type Ref struct {
	Host, Repo, Ref string
}

// ParseRef splits "ghcr.io/org/img:tag", "org/img" (Docker Hub, tag latest), "host/img@sha256:…" or "host/img:tag@sha256:…".
func ParseRef(s string) (Ref, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s, " \t") {
		return Ref{}, fmt.Errorf("invalid image %q", s)
	}
	host, rest := "docker.io", s
	if first, after, ok := strings.Cut(s, "/"); ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		host, rest = first, after
	}
	repo, ref := rest, "latest"
	if r, d, ok := strings.Cut(rest, "@"); ok {
		// "repo:tag@sha256:…": the digest wins, the tag is only a name for people.
		if i := strings.LastIndex(r, ":"); i > strings.LastIndex(r, "/") {
			r = r[:i]
		}
		repo, ref = r, d
	} else if i := strings.LastIndex(rest, ":"); i > strings.LastIndex(rest, "/") {
		repo, ref = rest[:i], rest[i+1:]
	}
	if host == "docker.io" {
		host = "registry-1.docker.io"
		if !strings.Contains(repo, "/") {
			repo = "library/" + repo
		}
	}
	if repo == "" || ref == "" {
		return Ref{}, fmt.Errorf("invalid image %q", s)
	}
	return Ref{Host: host, Repo: repo, Ref: ref}, nil
}

// Client reads image labels; Scheme is "https" except in tests.
type Client struct {
	HTTP   *http.Client
	Scheme string
}

const manifestTypes = "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json"

// Image is what the registry says about an image reference: the digest the reference points at now (for an image built for several platforms, that of the index), the labels of its linux/amd64 variant (else of its first one) and the architectures it has for linux (amd64, arm64).
type Image struct {
	Digest string
	Labels map[string]string
	Archs  []string
}

// Pinned is the reference fixed to the digest ("repo:tag@sha256:…"): every node runs exactly this image, whatever the tag points at later.
func (i Image) Pinned(image string) string {
	if at := strings.LastIndex(image, "@"); at >= 0 {
		image = image[:at]
	}
	return image + "@" + i.Digest
}

// Inspect resolves the reference to its digest and reads the labels of that digest.
func (c *Client) Inspect(ctx context.Context, image string) (Image, error) {
	r, err := ParseRef(image)
	if err != nil {
		return Image{}, err
	}
	s := &session{c: c, r: r}
	var m struct {
		Manifests []struct {
			Digest   string `json:"digest"`
			Platform struct {
				OS           string `json:"os"`
				Architecture string `json:"architecture"`
			} `json:"platform"`
		} `json:"manifests"`
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
	}
	body, header, err := s.get(ctx, "manifests/"+r.Ref, manifestTypes)
	if err != nil {
		return Image{}, err
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return Image{}, err
	}
	// The registry names the digest; if not, it is the hash of the manifest as served.
	digest := header.Get("Docker-Content-Digest")
	if strings.HasPrefix(r.Ref, "sha256:") {
		digest = r.Ref
	}
	if digest == "" {
		sum := sha256.Sum256(body)
		digest = "sha256:" + hex.EncodeToString(sum[:])
	}
	var archs []string
	if m.Config.Digest == "" {
		// An index: its linux platforms (attestations have the platform unknown), labels from the amd64 variant if there is one.
		variant := ""
		for _, x := range m.Manifests {
			if x.Platform.OS != "linux" || x.Platform.Architecture == "" || x.Platform.Architecture == "unknown" || slices.Contains(archs, x.Platform.Architecture) {
				continue
			}
			archs = append(archs, x.Platform.Architecture)
			if variant == "" || x.Platform.Architecture == "amd64" {
				variant = x.Digest
			}
		}
		if variant == "" {
			return Image{}, fmt.Errorf("image %s has no linux variant", image)
		}
		m.Manifests = nil
		if err := s.getJSON(ctx, "manifests/"+variant, manifestTypes, &m); err != nil {
			return Image{}, err
		}
	}
	if m.Config.Digest == "" {
		return Image{}, fmt.Errorf("image %s: manifest without config", image)
	}
	var cfg struct {
		Architecture string `json:"architecture"`
		Config       struct {
			Labels map[string]string `json:"Labels"`
		} `json:"config"`
	}
	if err := s.getJSON(ctx, "blobs/"+m.Config.Digest, "*/*", &cfg); err != nil {
		return Image{}, err
	}
	if cfg.Config.Labels == nil {
		cfg.Config.Labels = map[string]string{}
	}
	if archs == nil && cfg.Architecture != "" {
		archs = []string{cfg.Architecture}
	}
	slices.Sort(archs)
	return Image{Digest: digest, Labels: cfg.Config.Labels, Archs: archs}, nil
}

// session is one image's requests, with the anonymous token the registry hands out on the first 401.
type session struct {
	c     *Client
	r     Ref
	token string
}

func (s *session) getJSON(ctx context.Context, path, accept string, v any) error {
	body, _, err := s.get(ctx, path, accept)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

// get fetches one path of the repository, with an anonymous token after the first 401.
func (s *session) get(ctx context.Context, path, accept string) ([]byte, http.Header, error) {
	scheme := s.c.Scheme
	if scheme == "" {
		scheme = "https"
	}
	u := fmt.Sprintf("%s://%s/v2/%s/%s", scheme, s.r.Host, s.r.Repo, path)
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, nil, err
		}
		req.Header.Set("Accept", accept)
		if s.token != "" {
			req.Header.Set("Authorization", "Bearer "+s.token)
		}
		resp, err := s.c.HTTP.Do(req)
		if err != nil {
			return nil, nil, err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if err != nil {
			return nil, nil, err
		}
		switch {
		case resp.StatusCode == http.StatusOK:
			return body, resp.Header, nil
		case resp.StatusCode == http.StatusUnauthorized && attempt == 0:
			if err := s.fetchToken(ctx, resp.Header.Get("WWW-Authenticate")); err != nil {
				return nil, nil, err
			}
		case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden, resp.StatusCode == http.StatusNotFound:
			return nil, nil, ErrNotFound
		default:
			return nil, nil, fmt.Errorf("registry %s: %s %d", s.r.Host, path, resp.StatusCode)
		}
	}
}

// fetchToken gets an anonymous pull token from the realm a 401 names (Bearer realm="…",service="…",scope="…").
func (s *session) fetchToken(ctx context.Context, challenge string) error {
	scheme, params, _ := strings.Cut(challenge, " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return ErrNotFound
	}
	p := map[string]string{}
	for _, part := range splitParams(params) {
		k, v, _ := strings.Cut(part, "=")
		p[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	if p["realm"] == "" {
		return ErrNotFound
	}
	q := url.Values{}
	if p["service"] != "" {
		q.Set("service", p["service"])
	}
	scope := p["scope"]
	if scope == "" {
		scope = "repository:" + s.r.Repo + ":pull"
	}
	q.Set("scope", scope)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p["realm"]+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := s.c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ErrNotFound
	}
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&t); err != nil {
		return err
	}
	s.token = t.Token
	if s.token == "" {
		s.token = t.AccessToken
	}
	if s.token == "" {
		return ErrNotFound
	}
	return nil
}

// splitParams splits a challenge's parameters at commas outside quotes (scope may hold commas: "repository:x:pull,push").
func splitParams(s string) []string {
	var out []string
	quoted, start := false, 0
	for i, ch := range s {
		switch {
		case ch == '"':
			quoted = !quoted
		case ch == ',' && !quoted:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}
