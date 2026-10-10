package gpu

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// gitRefs is what a Git server advertises: ref name -> commit, plus the
// default branch (symref of HEAD).
type gitRefs struct {
	Refs          map[string]string
	DefaultBranch string
}

var ErrRefNotFound = errors.New("ref not found")

// fetchRefs reads the refs of a public repository over Git's smart HTTP
// protocol (what `git ls-remote` does), so the service needs no git binary.
func fetchRefs(ctx context.Context, client *http.Client, repoURL string) (gitRefs, error) {
	u := strings.TrimSuffix(repoURL, "/") + "/info/refs?service=git-upload-pack"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return gitRefs{}, err
	}
	req.Header.Set("User-Agent", "git/2.45.0 (llm-management-api)")
	resp, err := client.Do(req)
	if err != nil {
		return gitRefs{}, fmt.Errorf("git: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return gitRefs{}, fmt.Errorf("git: repository not found or not public (%d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return gitRefs{}, fmt.Errorf("git: %s: %d", u, resp.StatusCode)
	}
	return parseRefAdvertisement(resp.Body)
}

// parseRefAdvertisement parses pkt-lines: "# service=git-upload-pack", flush,
// then "<sha> <ref>[\0capabilities]" lines up to the next flush.
func parseRefAdvertisement(r io.Reader) (gitRefs, error) {
	br := bufio.NewReader(r)
	out := gitRefs{Refs: map[string]string{}}
	first := true
	for {
		lenHex := make([]byte, 4)
		if _, err := io.ReadFull(br, lenHex); err != nil {
			if errors.Is(err, io.EOF) && len(out.Refs) > 0 {
				return out, nil
			}
			return out, fmt.Errorf("git: truncated ref advertisement: %w", err)
		}
		n, err := strconv.ParseUint(string(lenHex), 16, 16)
		if err != nil {
			return out, fmt.Errorf("git: bad pkt-line length %q", lenHex)
		}
		if n == 0 { // flush
			if len(out.Refs) > 0 {
				return out, nil
			}
			continue
		}
		if n < 4 {
			return out, fmt.Errorf("git: bad pkt-line length %d", n)
		}
		line := make([]byte, n-4)
		if _, err := io.ReadFull(br, line); err != nil {
			return out, fmt.Errorf("git: truncated pkt-line: %w", err)
		}
		text := strings.TrimRight(string(line), "\n")
		if strings.HasPrefix(text, "# service=") {
			continue
		}
		ref, caps, _ := strings.Cut(text, "\x00")
		sha, name, ok := strings.Cut(ref, " ")
		if !ok {
			continue
		}
		if first {
			first = false
			for _, c := range strings.Fields(caps) {
				if t, ok := strings.CutPrefix(c, "symref=HEAD:refs/heads/"); ok {
					out.DefaultBranch = t
				}
			}
		}
		out.Refs[name] = sha
	}
}

var hexRef = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// resolve turns a ref (empty/HEAD, branch, tag or commit) into the commit to
// build and the branch the files are checked out from.
func (g gitRefs) resolve(ref string) (commit, branch string, err error) {
	ref = strings.TrimSpace(ref)
	switch {
	case ref == "" || ref == "HEAD":
		if g.DefaultBranch == "" {
			return "", "", errors.New("git: repository has no default branch")
		}
		return g.Refs["refs/heads/"+g.DefaultBranch], g.DefaultBranch, nil
	case g.Refs["refs/heads/"+ref] != "":
		return g.Refs["refs/heads/"+ref], ref, nil
	case g.Refs["refs/tags/"+ref+"^{}"] != "":
		return g.Refs["refs/tags/"+ref+"^{}"], g.DefaultBranch, nil
	case g.Refs["refs/tags/"+ref] != "":
		return g.Refs["refs/tags/"+ref], g.DefaultBranch, nil
	case hexRef.MatchString(strings.ToLower(ref)):
		return strings.ToLower(ref), g.DefaultBranch, nil
	}
	return "", "", fmt.Errorf("%w: %s", ErrRefNotFound, ref)
}

func newGitClient() *http.Client { return &http.Client{Timeout: 20 * time.Second} }
