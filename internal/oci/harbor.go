package oci

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Scan is Harbor's vulnerability scan of an artifact (Trivy): its status as Harbor names it (Success, Running, Pending, Error, Stopped, …), the vulnerabilities by severity, when it ended, and the artifact's page in Harbor's UI.
type Scan struct {
	Status    string
	Critical  int
	High      int
	ScannedAt time.Time
	URL       string
}

const vulnReportTypes = "application/vnd.security.vulnerability.report; version=1.1, application/vnd.scanner.adapter.vuln.report.harbor+json; version=1.0"

type harborArtifact struct {
	ProjectID    int                       `json:"project_id"`
	Digest       string                    `json:"digest"`
	ScanOverview map[string]harborOverview `json:"scan_overview"`
	References   []struct {
		ChildDigest string `json:"child_digest"`
		Platform    struct {
			OS           string `json:"os"`
			Architecture string `json:"architecture"`
		} `json:"platform"`
	} `json:"references"`
}

type harborOverview struct {
	ScanStatus string    `json:"scan_status"`
	EndTime    time.Time `json:"end_time"`
	Summary    struct {
		Summary map[string]int `json:"summary"`
	} `json:"summary"`
}

// HarborScan reads the scan overview of an image in a Harbor registry (its first path segment is the Harbor project): GET /api/v2.0/projects/{p}/repositories/{r}/artifacts/{ref}?with_scan_overview=true, anonymously for a public project or with auth (a robot account that may read). For an image index without a scan of its own, the scan of its linux/amd64 (else first) variant counts. nil without an error = not scanned (yet).
func (c *Client) HarborScan(ctx context.Context, image string, auth *Auth) (*Scan, error) {
	r, err := ParseRef(image)
	if err != nil {
		return nil, err
	}
	project, repo, ok := strings.Cut(r.Repo, "/")
	if !ok {
		return nil, fmt.Errorf("image %s: no Harbor project in its path", image)
	}
	a, err := c.harborArtifact(ctx, r.Host, project, repo, r.Ref, auth)
	if err != nil {
		return nil, err
	}
	ov, found := overviewOf(a)
	if !found && len(a.References) > 0 {
		child := a.References[0].ChildDigest
		for _, ref := range a.References {
			if ref.Platform.OS == "linux" && ref.Platform.Architecture == "amd64" {
				child = ref.ChildDigest
			}
		}
		if ca, err := c.harborArtifact(ctx, r.Host, project, repo, child, auth); err == nil {
			ov, found = overviewOf(ca)
		}
	}
	if !found {
		return nil, nil
	}
	sum := ov.Summary.Summary
	return &Scan{
		Status:    ov.ScanStatus,
		Critical:  sum["Critical"],
		High:      sum["High"],
		ScannedAt: ov.EndTime,
		URL:       fmt.Sprintf("https://%s/harbor/projects/%d/repositories/%s/artifacts-tab/artifacts/%s", r.Host, a.ProjectID, url.PathEscape(repo), a.Digest),
	}, nil
}

// overviewOf is an artifact's scan overview of the vulnerability report (Harbor keys it by the report's media type).
func overviewOf(a harborArtifact) (harborOverview, bool) {
	for typ, ov := range a.ScanOverview {
		if strings.Contains(typ, "vuln") && ov.ScanStatus != "" {
			return ov, true
		}
	}
	return harborOverview{}, false
}

func (c *Client) harborArtifact(ctx context.Context, host, project, repo, ref string, auth *Auth) (harborArtifact, error) {
	scheme := c.Scheme
	if scheme == "" {
		scheme = "https"
	}
	// Harbor wants a repository with slashes escaped twice in the path.
	u := fmt.Sprintf("%s://%s/api/v2.0/projects/%s/repositories/%s/artifacts/%s?with_scan_overview=true", scheme, host, url.PathEscape(project), url.PathEscape(url.PathEscape(repo)), url.PathEscape(ref))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return harborArtifact{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Accept-Vulnerabilities", vulnReportTypes)
	if auth != nil {
		req.SetBasicAuth(auth.Username, auth.Password)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return harborArtifact{}, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return harborArtifact{}, ErrNotFound
	default:
		return harborArtifact{}, fmt.Errorf("harbor %s: artifact %d", host, resp.StatusCode)
	}
	var a harborArtifact
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&a); err != nil {
		return harborArtifact{}, err
	}
	return a, nil
}
