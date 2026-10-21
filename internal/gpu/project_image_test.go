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

// The whole script with a fake git and BuildKit: it builds for BUILD_ARCH, and a variant for an index (PUSH_BY_DIGEST) goes to the repository without a tag and ends with its digest.
func TestProjectImageScriptBuild(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	digest := "sha256:" + strings.Repeat("c", 64)
	for _, byDigest := range []bool{false, true} {
		dir := t.TempDir()
		bin := filepath.Join(dir, "bin")
		if err := os.Mkdir(bin, 0o755); err != nil {
			t.Fatal(err)
		}
		git := "#!/bin/sh\nif [ \"$1\" = clone ]; then mkdir -p \"$5\" && echo numpy > \"$5/requirements.txt\"; fi\n"
		buildctl := "#!/bin/sh\necho \"$@\" > " + dir + "/args\nwhile [ $# -gt 0 ]; do if [ \"$1\" = --metadata-file ]; then printf '{\"containerimage.digest\": \"" + digest + "\"}' > \"$2\"; fi; shift; done\n"
		for name, body := range map[string]string{"git": git, "buildctl-daemonless.sh": buildctl} {
			if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.Command(bash, "-c", strings.ReplaceAll(projectImageScript, "/tmp/", dir+"/"))
		env := append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "GIT_URL=https://example.org/r", "GIT_COMMIT=abc", "GIT_BRANCH=main", "BASE_IMAGE=reg/platform/base-python:v1", "IMAGE=reg/envs/r", "BUILD_ARCH=arm64", "R2D_BUILDKIT_CACHE=reg/envs/buildcache:r-arm64")
		if byDigest {
			env = append(env, "PUSH_BY_DIGEST=1")
		}
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("script: %v\n%s", err, out)
		}
		args, _ := os.ReadFile(filepath.Join(dir, "args"))
		if !strings.Contains(string(args), "--opt platform=linux/arm64") {
			t.Errorf("platform not passed: %s", args)
		}
		pushed := strings.Contains(string(args), "type=image,name=reg/envs/r,push=true,push-by-digest=true,name-canonical=true")
		named := strings.Contains(string(out), "dhbw-digest: "+digest+"\n")
		if pushed != byDigest || named != byDigest {
			t.Errorf("by digest %v: args %s, output %s", byDigest, args, out)
		}
	}
}
