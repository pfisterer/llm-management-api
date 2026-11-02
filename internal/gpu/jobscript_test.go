package gpu

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// runJobScript runs jobScript in sh with a fresh home, as the job's container does; with term it sends TERM to the shell once the command has printed "ready". It returns the exit code and the attempt's log file.
func runJobScript(t *testing.T, home, id, command string, term bool) (int, string) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	for _, tool := range []string{"mkfifo", "tee"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("no " + tool)
		}
	}
	cmd := exec.Command("sh", "-c", jobScript)
	cmd.Env = append(os.Environ(), "HOME="+home, "DHBW_JOB_ID="+id, "DHBW_JOB_NAME=train", "DHBW_JOB_DIR="+filepath.Join(home, "jobs", "train"), "DHBW_JOB_WORKDIR=", "DHBW_JOB_COMMAND="+command)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if term {
		ready := filepath.Join(home, "ready")
		deadline := time.Now().Add(10 * time.Second)
		for {
			if _, err := os.Stat(ready); err == nil {
				break
			}
			if time.Now().After(deadline) {
				_ = cmd.Process.Kill()
				t.Fatalf("command not ready:\n%s", out.String())
			}
			time.Sleep(20 * time.Millisecond)
		}
		_ = os.Remove(ready)
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("script did not end:\n%s", out.String())
	}
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	logs, _ := filepath.Glob(filepath.Join(home, "jobs", "train", "output-*.log"))
	if len(logs) == 0 {
		t.Fatalf("no log:\n%s", out.String())
	}
	// The newest attempt has the highest number (fewer than ten in the test, so the order of the names is enough).
	b, _ := os.ReadFile(logs[len(logs)-1])
	if !strings.Contains(out.String(), "Ende, Exit-Code") {
		t.Errorf("stdout lacks the end line:\n%s", out.String())
	}
	return code, string(b)
}

// The job script passes the command's exit code through, counts the attempts per job id in the job's folder and, on TERM, passes the signal on to the command, waits for it to save and keeps its last lines in the log; a command that ends with 0 after the signal leaves with 143.
func TestJobScript(t *testing.T) {
	home := t.TempDir()
	if code, log := runJobScript(t, home, "j-0000000001", `echo "attempt $DHBW_JOB_ATTEMPT"; echo oops >&2; exit 3`, false); code != 3 || !strings.Contains(log, "attempt 1") || !strings.Contains(log, "oops") || !strings.Contains(log, "Ende, Exit-Code 3") {
		t.Errorf("failure: %d\n%s", code, log)
	}
	if code, log := runJobScript(t, home, "j-0000000001", `echo "attempt $DHBW_JOB_ATTEMPT"`, false); code != 0 || !strings.Contains(log, "attempt 2") || !strings.Contains(log, "Versuch 2") || !strings.Contains(log, "Ende, Exit-Code 0") {
		t.Errorf("second attempt: %d\n%s", code, log)
	}
	// Another job of the same name counts its own attempts.
	if code, log := runJobScript(t, home, "j-0000000002", `echo "attempt $DHBW_JOB_ATTEMPT"`, false); code != 0 || !strings.Contains(log, "attempt 1") {
		t.Errorf("other job: %d\n%s", code, log)
	}

	// A command that saves on TERM, as a script around the real program (a child of a shell, which the signal must reach through its process group where setsid exists, else directly).
	save := `trap 'echo saving; sleep 1; echo saved > "$DHBW_JOB_DIR/checkpoint"; echo saved-done; exit 0' TERM; touch "$HOME/ready"; while :; do sleep 1 & wait $!; done`
	code, log := runJobScript(t, home, "j-0000000003", save, true)
	if code != 143 || !strings.Contains(log, "saved-done") || !strings.Contains(log, "Signal erhalten") || !strings.Contains(log, "Exit-Code des Befehls 0") || !strings.Contains(log, "Ende, Exit-Code 143") {
		t.Errorf("term: %d\n%s", code, log)
	}
	if b, err := os.ReadFile(filepath.Join(home, "jobs", "train", "checkpoint")); err != nil || strings.TrimSpace(string(b)) != "saved" {
		t.Errorf("checkpoint: %q %v", b, err)
	}
	// A command that leaves with its own code after TERM keeps it.
	if code, log := runJobScript(t, home, "j-0000000004", `trap 'exit 5' TERM; touch "$HOME/ready"; while :; do sleep 1 & wait $!; done`, true); code != 5 || !strings.Contains(log, "Ende, Exit-Code 5") {
		t.Errorf("term with own code: %d\n%s", code, log)
	}
}
