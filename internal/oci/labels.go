// Package oci reads the labels of a container image from its registry (OCI distribution API), anonymously as a node would pull it: public images only.
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
)

// ErrNotFound is an image the registry does not have or does not give out without credentials.
var ErrNotFound = errors.New("image not found or not public")

// Ref is an image reference split into registry host, repository and tag or digest.
type Ref struct {
	Host, Repo, Ref string
}

// ParseRef splits "ghcr.io/org/img:tag", "org/img" (Docker Hub, tag latest) or "host/img@sha256:…".
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

// Labels returns the labels of the image's linux/amd64 variant (the platform of the cluster).
func (c *Client) Labels(ctx context.Context, image string) (map[string]string, error) {
	r, err := ParseRef(image)
	if err != nil {
		return nil, err
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
	if err := s.getJSON(ctx, "manifests/"+r.Ref, manifestTypes, &m); err != nil {
		return nil, err
	}
	if m.Config.Digest == "" {
		// An index: the variant for the cluster's platform.
		digest := ""
		for _, x := range m.Manifests {
			if x.Platform.OS == "linux" && x.Platform.Architecture == "amd64" {
				digest = x.Digest
				break
			}
		}
		if digest == "" {
			return nil, fmt.Errorf("image %s has no linux/amd64 variant", image)
		}
		m.Manifests = nil
		if err := s.getJSON(ctx, "manifests/"+digest, manifestTypes, &m); err != nil {
			return nil, err
		}
	}
	if m.Config.Digest == "" {
		return nil, fmt.Errorf("image %s: manifest without config", image)
	}
	var cfg struct {
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"config"`
	}
	if err := s.getJSON(ctx, "blobs/"+m.Config.Digest, "*/*", &cfg); err != nil {
		return nil, err
	}
	if cfg.Config.Labels == nil {
		return map[string]string{}, nil
	}
	return cfg.Config.Labels, nil
}

// session is one image's requests, with the anonymous token the registry hands out on the first 401.
type session struct {
	c     *Client
	r     Ref
	token string
}

func (s *session) getJSON(ctx context.Context, path, accept string, v any) error {
	scheme := s.c.Scheme
	if scheme == "" {
		scheme = "https"
	}
	u := fmt.Sprintf("%s://%s/v2/%s/%s", scheme, s.r.Host, s.r.Repo, path)
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", accept)
		if s.token != "" {
			req.Header.Set("Authorization", "Bearer "+s.token)
		}
		resp, err := s.c.HTTP.Do(req)
		if err != nil {
			return err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if err != nil {
			return err
		}
		switch {
		case resp.StatusCode == http.StatusOK:
			return json.Unmarshal(body, v)
		case resp.StatusCode == http.StatusUnauthorized && attempt == 0:
			if err := s.fetchToken(ctx, resp.Header.Get("WWW-Authenticate")); err != nil {
				return err
			}
		case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden, resp.StatusCode == http.StatusNotFound:
			return ErrNotFound
		default:
			return fmt.Errorf("registry %s: %s %d", s.r.Host, path, resp.StatusCode)
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
