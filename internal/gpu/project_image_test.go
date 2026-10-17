package gpu

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// runClone runs the build script's clone step with a fake git that fails the first `failures` clones, in a temporary directory instead of /tmp. It returns the script's output, its exit code and how often git was called.
func runClone(t *testing.T, failures int, reason string) (string, int, int) {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	counter := filepath.Join(dir, "calls")
	git := `#!/bin/sh
n=$(cat "` + counter + `" 2>/dev/null || echo 0); n=$((n+1)); echo "$n" > "` + counter + `"
if [ "$n" -le ` + strconv.Itoa(failures) + ` ]; then echo "fatal: ` + reason + `" >&2; exit 128; fi
mkdir -p "$5"
`
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(git), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "set -euo pipefail\n" + strings.ReplaceAll(projectImageClone, "/tmp/", dir+"/") + "echo cloned\n"
	cmd := exec.Command(bash, "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "GIT_URL=https://example.org/r.git", "CLONE_RETRY_PAUSE=0")
	out, _ := cmd.CombinedOutput()
	calls := 0
	if b, err := os.ReadFile(counter); err == nil {
		calls, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	return string(out), cmd.ProcessState.ExitCode(), calls
}

// A clone that breaks once or twice is tried again and the build goes on, without a "dhbw: " line that would become the build's message.
func TestProjectImageCloneRetries(t *testing.T) {
	for _, failures := range []int{0, 1, 2} {
		out, code, calls := runClone(t, failures, "unable to access")
		if code != 0 || calls != failures+1 || !strings.Contains(out, "cloned") || strings.Contains(out, "dhbw: ") {
			t.Errorf("%d failures: exit %d, %d calls, output %q", failures, code, calls, out)
		}
	}
}

// Three failed clones end the build with one "dhbw: " line naming the reason; a repository that needs a login reads as not found or not public.
func TestProjectImageCloneGivesUp(t *testing.T) {
	for reason, want := range map[string]string{
		"unable to access 'https://example.org/r.git/': Recv failure: Connection reset by peer": "unable to access 'https://example.org/r.git/': Recv failure: Connection reset by peer",
		"could not read Username for 'https://example.org': terminal prompts disabled":          "Repository nicht gefunden oder nicht öffentlich",
	} {
		out, code, calls := runClone(t, 3, reason)
		if code != 1 || calls != 3 || strings.Contains(out, "cloned") {
			t.Fatalf("exit %d, %d calls, output %q", code, calls, out)
		}
		if strings.Count(out, "dhbw: ") != 1 || !strings.Contains(out, "dhbw: Das Repository https://example.org/r.git konnte nach 3 Versuchen nicht geklont werden: "+want+"\n") {
			t.Errorf("output %q", out)
		}
	}
}

// The job runs the clone step first, before the checkout.
func TestProjectImageScriptStartsWithClone(t *testing.T) {
	if !strings.HasPrefix(projectImageScript, "set -euo pipefail\n"+projectImageClone+"git -C /tmp/repo checkout") {
		t.Errorf("script starts with %q", projectImageScript[:200])
	}
}
