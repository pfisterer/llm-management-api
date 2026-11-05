package gpu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A workspace of any profile is a caller for the main API's routes on the hub listener (ServerCaller); GPU kernels stay for "CPU + GPU-Kernel" only.
func TestServerCaller(t *testing.T) {
	ctx := context.Background()
	cpu, _ := kernelTestService(strings.Replace(testServerPod, `"cpu-gpukernel"`, `"cpu"`, 1))
	o, err := cpu.ServerCaller(ctx, "10.42.1.5", "t-a")
	if err != nil || o.Person != "a@dhbw.de" || o.Project != "ml" {
		t.Fatalf("cpu server: %+v %v", o, err)
	}
	if _, err := cpu.KernelCaller(ctx, "10.42.1.5", "t-a"); !errors.Is(err, ErrNoKernelOwner) {
		t.Errorf("kernel from a cpu server: %v", err)
	}
	for _, c := range []struct{ ip, token string }{{"10.42.1.6", "t-a"}, {"10.42.1.5", "t-b"}, {"10.42.1.5", ""}, {"not-an-ip", "t-a"}} {
		if _, err := cpu.ServerCaller(ctx, c.ip, c.token); !errors.Is(err, ErrNoKernelOwner) {
			t.Errorf("%s with %q: %v", c.ip, c.token, err)
		}
	}
	// The token of somebody else at this server's address.
	cpu.whoami = func(context.Context, string) (string, error) { return "b@dhbw.de", nil }
	cpu.tokens.hits = nil
	if _, err := cpu.ServerCaller(ctx, "10.42.1.5", "t-x"); !errors.Is(err, ErrNoKernelOwner) {
		t.Errorf("token of another person: %v", err)
	}
	// A server pod without a person is nobody's.
	anon, _ := kernelTestService(strings.Replace(testServerPod, `"annotations":{"hub.jupyter.org/username":"a@dhbw.de"}`, `"annotations":{}`, 1))
	if _, err := anon.ServerCaller(ctx, "10.42.1.5", "t-a"); !errors.Is(err, ErrNoKernelOwner) {
		t.Errorf("server without person: %v", err)
	}
}

func TestJobFilePath(t *testing.T) {
	for _, c := range []struct {
		p, root, want string
		ok            bool
	}{
		{"jobs/a/out.pt", "", "jobs/a/out.pt", true},
		{"/home/jovyan/jobs/a", "jobs/a", "jobs/a", true},
		{"jobs/a/x/../y", "jobs/a", "jobs/a/y", true},
		{"", "", "", true},
		{"jobs/a/../b/secret", "jobs/a", "", false},
		{"jobs/ab", "jobs/a", "", false},
		{"../other", "", "", false},
		{"/etc/passwd", "", "", false},
		{"/home/jovyan/../x", "", "", false},
		{"jobs/a/\nx", "", "", false},
		{"jobs/a/\x00", "", "", false},
		{"notes.txt", "jobs/a", "", false},
	} {
		got, ok := JobFilePath(c.p, c.root)
		if got != c.want || ok != c.ok {
			t.Errorf("JobFilePath(%q, %q) = %q, %v; want %q, %v", c.p, c.root, got, ok, c.want, c.ok)
		}
	}
}

// fakeFilesKube answers a files pod as running at 127.0.0.1 and records creations and deletions.
type fakeFilesKube struct {
	mu      sync.Mutex
	created []map[string]any
	deleted []string
}

func (f *fakeFilesKube) do(_ context.Context, method, path string, body, out any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case method == "POST":
		f.created = append(f.created, body.(map[string]any))
	case method == "DELETE":
		f.deleted = append(f.deleted, path)
	case method == "GET" && strings.Contains(path, "/pods/"+filesApp+"-"):
		return json.Unmarshal([]byte(`{"status":{"phase":"Running","podIP":"127.0.0.1"}}`), out)
	case method == "GET":
		return json.Unmarshal([]byte(`{"items":[]}`), out)
	}
	return nil
}

// startFilesServer runs files_server.py on root at a free port with the token, as the reading files pod does; skipped without python3.
func startFilesServer(t *testing.T, root, token string) string {
	t.Helper()
	return startFilesServerMode(t, root, token, "ro")
}

// startFilesServerMode is startFilesServer with the mode of the pod (ro, rw).
func startFilesServerMode(t *testing.T, root, token, mode string) string {
	t.Helper()
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	cmd := exec.Command(py, "-I", "-c", filesServer, root, fmt.Sprint(port), mode)
	cmd.Env = append(os.Environ(), "DHBW_FILES_TOKEN="+token)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	addr := fmt.Sprintf("http://127.0.0.1:%d", port)
	for i := 0; i < 100; i++ {
		if c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port)); err == nil {
			c.Close()
			return addr
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("files server did not start")
	return ""
}

// The files of a job's home through the files pod: listing, download, the refusals (outside the root, symbolic links out of the home, folders as files), and the pod as it is built.
func TestJobFiles(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	for _, d := range []string{"jobs/train", "data"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range map[string]string{"jobs/train/out.txt": "result", "data/a.csv": "1,2", "../outside.txt": "secret"} {
		if err := os.WriteFile(filepath.Join(home, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.Symlink("../../../outside.txt", filepath.Join(home, "jobs/train/escape"))
	_ = os.Symlink("../../data/a.csv", filepath.Join(home, "jobs/train/inside"))

	s, _, _, _ := testService()
	k := &fakeFilesKube{}
	s.k8s = k
	s.cfg.Jobs.InitImage = "reg/platform/base-python@sha256:abc"
	// The pod's token is random: the test server takes the one the pod object carries, so it is started on the first creation.
	var addr string
	s.files.addr = func(string) string {
		if addr == "" {
			k.mu.Lock()
			env := k.created[0]["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["env"].([]any)[0].(map[string]any)
			k.mu.Unlock()
			addr = startFilesServer(t, home, env["value"].(string))
		}
		return addr
	}
	ctx := context.Background()
	j := Job{ID: "j-0123456789", Name: "train", Person: "a@dhbw.de", Project: "t-kurs"}

	list, err := s.ListJobFiles(ctx, j, "jobs/train")
	if err != nil {
		t.Fatal(err)
	}
	types := map[string]string{}
	for _, f := range list.Files {
		types[f.Path] = f.Type
	}
	if types["jobs/train/out.txt"] != "file" || types["jobs/train/escape"] != "link" || types["jobs/train/inside"] != "link" || len(list.Files) != 3 {
		t.Errorf("listing: %+v", list)
	}
	read := func(rel string) (string, error) {
		body, _, err := s.OpenJobFile(ctx, j, rel)
		if err != nil {
			return "", err
		}
		defer body.Close()
		b, err := io.ReadAll(body)
		return string(b), err
	}
	if got, err := read("jobs/train/out.txt"); err != nil || got != "result" {
		t.Errorf("download: %q %v", got, err)
	}
	if got, err := read("jobs/train/inside"); err != nil || got != "1,2" {
		t.Errorf("link inside the home: %q %v", got, err)
	}
	for _, rel := range []string{"jobs/train/escape", "../outside.txt", "jobs/train/missing"} {
		if got, err := read(rel); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %q %v, want not found", rel, got, err)
		}
	}
	var refused *JobRefused
	if _, err := read("jobs/train"); !errors.As(err, &refused) {
		t.Errorf("folder as file: %v", err)
	}
	if _, err := s.ListJobFiles(ctx, j, "jobs/train/out.txt"); !errors.As(err, &refused) {
		t.Errorf("file as folder: %v", err)
	}

	// One pod for all of these requests, built as the admission policy wants it.
	if len(k.created) != 1 {
		t.Fatalf("pods created: %d", len(k.created))
	}
	b, _ := json.Marshal(k.created[0])
	pod := string(b)
	for _, want := range []string{`"claimName":"ws-t-kurs"`, `"mountPath":"/data","name":"home","readOnly":true,"subPath":"homes/` + HubUserSlug(j.Person) + `"`, `"automountServiceAccountToken":false`, `"allowPrivilegeEscalation":false`, `"runAsUser":1000`, `"readOnlyRootFilesystem":true`, `"app":"dhbw-files"`, `"activeDeadlineSeconds":3600`} {
		if !strings.Contains(pod, want) {
			t.Errorf("files pod lacks %s: %s", want, pod)
		}
	}
	// While a download runs, the pod stays however long it takes; unused for filesIdle after it, the pod is deleted.
	body, _, err := s.OpenJobFile(ctx, j, "jobs/train/out.txt")
	if err != nil {
		t.Fatal(err)
	}
	s.sweepFilesPods(ctx, time.Now().Add(filesIdle+time.Minute))
	if len(k.deleted) != 0 {
		t.Errorf("pod deleted during a download: %v", k.deleted)
	}
	body.Close()
	s.sweepFilesPods(ctx, time.Now().Add(filesIdle+time.Minute))
	if len(k.deleted) != 1 || !strings.HasSuffix(k.deleted[0], "/pods/"+k.created[0]["metadata"].(map[string]any)["name"].(string)) {
		t.Errorf("deleted: %v", k.deleted)
	}
}

// Uploads through a files pod that may write (component=upload, its own pod beside the reading one): the file lands in the home with its folders, replaces an older one, and paths out of the home, onto folders or through files are refused; the reading pod takes no PUT.
func TestPutJobFile(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(filepath.Join(home, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(home, "notes.txt"), []byte("x"), 0o644)
	_ = os.Symlink("..", filepath.Join(home, "up"))

	s, _, _, _ := testService()
	k := &fakeFilesKube{}
	s.k8s = k
	s.cfg.Jobs.InitImage = "reg/platform/base-python@sha256:abc"
	// Every pod gets a server of its own, as it was built: its token and its mode.
	addrs := map[string]string{}
	s.files.addr = func(string) string {
		k.mu.Lock()
		pod := k.created[len(k.created)-1]
		k.mu.Unlock()
		name := pod["metadata"].(map[string]any)["name"].(string)
		if addrs[name] == "" {
			c := pod["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
			env := c["env"].([]any)[0].(map[string]any)
			cmd := c["command"].([]any)
			addrs[name] = startFilesServerMode(t, home, env["value"].(string), cmd[len(cmd)-1].(string))
		}
		return addrs[name]
	}
	ctx := context.Background()
	put := func(rel, content string) (JobFile, error) {
		return s.PutJobFile(ctx, "t-kurs", "a@dhbw.de", rel, false, strings.NewReader(content), int64(len(content)))
	}
	if f, err := put("jobs/train/train.py", "print(1)"); err != nil || f.Size != 8 || f.Path != "jobs/train/train.py" {
		t.Fatalf("upload: %+v %v", f, err)
	}
	if b, _ := os.ReadFile(filepath.Join(home, "jobs/train/train.py")); string(b) != "print(1)" {
		t.Errorf("uploaded: %q", b)
	}
	if _, err := put("jobs/train/train.py", "print(2)"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(home, "jobs/train/train.py")); string(b) != "print(2)" {
		t.Errorf("replaced: %q", b)
	}
	if left, _ := filepath.Glob(filepath.Join(home, "jobs/train/.dhbw-upload-*")); len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}
	var refused *JobRefused
	if _, err := put("data", "x"); !errors.As(err, &refused) {
		t.Errorf("onto a folder: %v", err)
	}
	if _, err := put("notes.txt/a", "x"); !errors.As(err, &refused) {
		t.Errorf("through a file: %v", err)
	}
	if _, err := put("up/outside.txt", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("out of the home through a link: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "outside.txt")); err == nil {
		t.Error("written outside the home")
	}
	// A body shorter than announced is refused and leaves nothing behind.
	if _, err := s.PutJobFile(ctx, "t-kurs", "a@dhbw.de", "short.txt", false, strings.NewReader("ab"), 5); err == nil {
		t.Error("short body accepted")
	}
	if _, err := os.Stat(filepath.Join(home, "short.txt")); err == nil {
		t.Error("short body written")
	}
	if _, err := s.PutJobFile(ctx, "t-kurs", "a@dhbw.de", "big", false, bytes.NewReader(nil), JobFileMaxBytes+1); !errors.As(err, &refused) {
		t.Errorf("too large: %v", err)
	}

	// The upload pod may write and says so; the reading pod, started for a listing, mounts read-only and refuses PUT.
	if _, err := s.ListJobFiles(ctx, Job{Project: "t-kurs", Person: "a@dhbw.de"}, "jobs"); err != nil {
		t.Fatal(err)
	}
	if len(k.created) != 2 {
		t.Fatalf("pods created: %d, want an upload pod and a reading one", len(k.created))
	}
	up, _ := json.Marshal(k.created[0])
	ro, _ := json.Marshal(k.created[1])
	for _, want := range []string{`"component":"upload"`, `"mountPath":"/data","name":"home","readOnly":false`, `"mountPath":"/data/project-shared","name":"home","readOnly":false,"subPath":"project-shared"`, `"rw"]`, `mkdir -p \"/ws/$DHBW_HOME_DIR/project-shared\" /ws/project-shared`} {
		if !strings.Contains(string(up), want) {
			t.Errorf("upload pod lacks %s: %s", want, up)
		}
	}
	for _, want := range []string{`"component":"files"`, `"mountPath":"/data","name":"home","readOnly":true`, `"mountPath":"/data/project-shared","name":"home","readOnly":true`, `"ro"]`} {
		if !strings.Contains(string(ro), want) {
			t.Errorf("reading pod lacks %s: %s", want, ro)
		}
	}
	// A reader of the project writes the home, never the shared folder: refused before any pod, and its upload pod mounts the shared folder read-only.
	var refusedShared *JobRefused
	if _, err := s.PutJobFile(ctx, "t-kurs", "a@dhbw.de", "project-shared/x", true, strings.NewReader("x"), 1); !errors.As(err, &refusedShared) {
		t.Errorf("reader into project-shared: %v", err)
	}
	if _, err := s.PutJobFile(ctx, "t-kurs", "a@dhbw.de", "notes.md", true, strings.NewReader("x"), 1); err != nil {
		t.Fatal(err)
	}
	if len(k.created) != 3 {
		t.Fatalf("pods created: %d, want a third for the reader's access", len(k.created))
	}
	if b, _ := json.Marshal(k.created[2]); !strings.Contains(string(b), `"mountPath":"/data/project-shared","name":"home","readOnly":true`) {
		t.Errorf("reader's upload pod writes the shared folder: %s", b)
	}
	roName := k.created[1]["metadata"].(map[string]any)["name"].(string)
	token := k.created[1]["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["env"].([]any)[0].(map[string]any)["value"].(string)
	req, _ := http.NewRequest(http.MethodPut, addrs[roName]+"/file?path=x.txt", strings.NewReader("x"))
	req.Header.Set("Authorization", "Bearer "+token)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("PUT to the reading pod: %v %v", resp, err)
	}
}
