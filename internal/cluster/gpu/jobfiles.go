package gpu

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pfisterer/cloud-self-service-golib/msg"
	"github.com/pfisterer/cloud-self-service-golib/retry"
)

// Files of a job (docs/architektur.md "Batch-Jobs", todo job_files): whoever only submits jobs has no workspace to fetch results from or to put a script and data into. A short-lived pod in the workspace namespace mounts the person's home folder of the project's volume (ws-<slug>, subPath homes/<slug>) and serves it to this service alone, over the pod network with a token of its own; this service streams it on. No exec into pods and no new rights in Kubernetes: the service already creates and deletes pods there (GPU kernels). The pod runs the hub's init image (a base with python3, prepulled on every node) with the small server files_server.py; its NetworkPolicy admits this service only. Besides the home the pod mounts the project's shared folder at its place in the home (project-shared), as workspaces and jobs do. Reading and writing are separate pods: the one that reads mounts both read-only (the admission policy holds it to that), only an upload starts one that may write (component=upload), the shared folder only for who may write it in a workspace (not a reader of the project).

//go:embed files_server.py
var filesServer string

const (
	filesApp  = "dhbw-files"
	filesPort = 8080
	// filesIdle is how long an unused files pod is kept for the next request of the same person in the same project (a listing, then downloads); filesReuse how old it may be to be used again, filesDeadline when Kubernetes ends it at the latest (activeDeadlineSeconds), longer than filesReuse plus the longest download. A pod this service lost track of (a restart, another replica's) runs until then and is deleted after.
	filesIdle     = 5 * time.Minute
	filesReuse    = 20 * time.Minute
	filesDeadline = time.Hour
	// filesStart is how long a files pod may take to start (pulled image, mounted volume).
	filesStart = 90 * time.Second
	// JobFileMaxBytes is the largest file a download serves; JobFileTimeout how long one may take. Larger results belong into S3 or a workspace.
	JobFileMaxBytes = 5 << 30
	JobFileTimeout  = 30 * time.Minute
	// SharedDir is the project's shared folder in the home (subPath project-shared of the volume).
	SharedDir = "project-shared"
)

// filesAccess is what a files pod may do: read the home and the shared folder, write both, or write the home and only read the shared folder (a reader of the project).
type filesAccess int

const (
	filesRead filesAccess = iota
	filesWrite
	filesWriteHome
)

// JobFile is an entry of a folder in a job's home: Path relative to the home, Type file, dir, link (a symbolic link, not followed) or other; Size only for files.
type JobFile struct {
	Name     string    `json:"name" example:"model.pt"`
	Path     string    `json:"path" example:"jobs/training-1/model.pt"`
	Type     string    `json:"type" enums:"file,dir,link,other"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

// JobFiles is a folder of a job's home: its path relative to the home, its entries sorted by name, Truncated when it has more than the listing returns.
type JobFiles struct {
	Path      string    `json:"path" example:"jobs/training-1"`
	Files     []JobFile `json:"files"`
	Truncated bool      `json:"truncated,omitempty"`
}

// filesPod is a running files pod of a person in a project, under key in filesPods; busy counts the transfers running through it (under filesPods.mu), which keep it from being deleted.
type filesPod struct {
	key, name, ip, token string
	created, used        time.Time
	busy                 int
}

type filesPods struct {
	mu   sync.Mutex
	pods map[string]*filesPod
	// starting serialises the start of one person's pod in one project: parallel requests share it.
	starting map[string]*sync.Mutex
	http     *http.Client
	// addr is the files server of a pod's address (tests point it elsewhere).
	addr func(ip string) string
}

// JobFilePath cleans a path of a job's home: relative to the home or absolute in /home/jovyan, inside root (relative to the home, "" = the whole home); false for a path that leaves it or carries control characters.
func JobFilePath(p, root string) (string, bool) {
	if strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", false
	}
	rel, ok := homeRel(p)
	if !ok {
		return "", false
	}
	if root != "" && rel != root && !strings.HasPrefix(rel, root+"/") {
		return "", false
	}
	return rel, true
}

// ListJobFiles lists a folder of the home of the job's person in its project's volume; rel is relative to the home (JobFilePath has checked it).
func (s *Service) ListJobFiles(ctx context.Context, j Job, rel string) (JobFiles, error) {
	ctx, cancel := context.WithTimeout(ctx, filesStart+30*time.Second)
	defer cancel()
	resp, err := s.filesDo(ctx, j.Project, j.Person, filesRead, "/list", rel, nil, 0)
	if err != nil {
		return JobFiles{}, err
	}
	defer resp.Body.Close()
	var out struct {
		Entries []struct {
			Name  string `json:"name"`
			Type  string `json:"type"`
			Size  int64  `json:"size"`
			MTime int64  `json:"mtime"`
		} `json:"entries"`
		Truncated bool `json:"truncated"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&out); err != nil {
		return JobFiles{}, fmt.Errorf("files listing: %w", err)
	}
	list := JobFiles{Path: rel, Files: []JobFile{}, Truncated: out.Truncated}
	for _, e := range out.Entries {
		list.Files = append(list.Files, JobFile{Name: e.Name, Path: path.Join(rel, e.Name), Type: e.Type, Size: e.Size, Modified: time.Unix(e.MTime, 0).UTC()})
	}
	return list, nil
}

// OpenJobFile opens a file of the home of the job's person for reading, with its size; the caller closes it. A file over JobFileMaxBytes is refused before anything is read.
func (s *Service) OpenJobFile(ctx context.Context, j Job, rel string) (io.ReadCloser, int64, error) {
	resp, err := s.filesDo(ctx, j.Project, j.Person, filesRead, "/file", rel, nil, 0)
	if err != nil {
		return nil, 0, err
	}
	size, err := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	if err != nil {
		resp.Body.Close()
		return nil, 0, errors.New("files server: no Content-Length")
	}
	if size > JobFileMaxBytes {
		resp.Body.Close()
		return nil, 0, &JobRefused{msg.F("Die Datei ist größer als %d GB; große Ergebnisse besser in S3 oder einem Workspace abholen.", "The file is larger than %d GB; better fetch large results through S3 or a workspace.", JobFileMaxBytes>>30)}
	}
	return resp.Body, size, nil
}

// PutJobFile writes a file of size bytes from body into the home of the person in the project, or into the project's shared folder (rel under SharedDir) unless sharedReadOnly (a reader of the project); rel is relative to the home (JobFilePath has checked it, not the home itself). Missing folders are made; an existing file is replaced only once the new one is complete. The person's upload pod writes it, the only files pod that may.
func (s *Service) PutJobFile(ctx context.Context, project, person, rel string, sharedReadOnly bool, body io.Reader, size int64) (JobFile, error) {
	if size > JobFileMaxBytes {
		return JobFile{}, &JobRefused{msg.F("Die Datei ist größer als %d GB; große Daten besser über S3 oder einen Workspace holen.", "The file is larger than %d GB; better bring large data through S3 or a workspace.", JobFileMaxBytes>>30)}
	}
	if rel == SharedDir {
		return JobFile{}, &JobRefused{msg.T("Keine Datei (ein Ordner oder etwas anderes).", "Not a file (a folder or something else).")}
	}
	access := filesWrite
	if sharedReadOnly {
		if strings.HasPrefix(rel, SharedDir+"/") {
			return JobFile{}, &JobRefused{msg.T("Im gemeinsamen Ordner ~/project-shared darf die Rolle „Lesend“ nur lesen.", "In the shared folder ~/project-shared the role “Reader” may only read.")}
		}
		access = filesWriteHome
	}
	resp, err := s.filesDo(ctx, project, person, access, "/file", rel, body, size)
	if err != nil {
		return JobFile{}, err
	}
	defer resp.Body.Close()
	var out struct {
		Size  int64 `json:"size"`
		MTime int64 `json:"mtime"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&out); err != nil {
		return JobFile{}, fmt.Errorf("files upload: %w", err)
	}
	return JobFile{Name: path.Base(rel), Path: rel, Type: "file", Size: out.Size, Modified: time.Unix(out.MTime, 0).UTC()}, nil
}

// filesDo asks the files pod of the person and project with the access: GET from the reading one, PUT to one that may write. A pod that went away meanwhile is started anew once; a body cannot be sent twice, so before a write the pod is asked whether it is still there.
func (s *Service) filesDo(ctx context.Context, project, person string, access filesAccess, op, rel string, body io.Reader, size int64) (*http.Response, error) {
	if s.k8s == nil || s.cfg.Jobs.InitImage == "" {
		return nil, &JobRefused{msg.T("Dateien von Jobs sind in diesem Cluster nicht eingerichtet.", "Files of jobs are not set up in this cluster.")}
	}
	write := access != filesRead
	method := http.MethodGet
	if write {
		method = http.MethodPut
	}
	for attempt := 0; ; attempt++ {
		p, err := s.filesPod(ctx, project, person, access)
		if err != nil {
			return nil, err
		}
		if write && attempt == 0 && !s.filesAlive(ctx, p) {
			s.dropFilesPod(context.WithoutCancel(ctx), p)
			continue
		}
		req, err := http.NewRequestWithContext(ctx, method, s.files.addr(p.ip)+op+"?"+url.Values{"path": {rel}}.Encode(), body)
		if err != nil {
			return nil, err
		}
		if write {
			req.ContentLength = size
			req.Header.Set("Content-Type", "application/octet-stream")
		}
		req.Header.Set("Authorization", "Bearer "+p.token)
		s.files.mu.Lock()
		p.busy++
		s.files.mu.Unlock()
		resp, err := s.files.http.Do(req)
		if err == nil && (resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated) {
			resp.Body = &filesBody{ReadCloser: resp.Body, done: func() { s.doneFilesPod(context.WithoutCancel(ctx), p) }}
			return resp, nil
		}
		s.doneFilesPod(context.WithoutCancel(ctx), p)
		if err != nil {
			s.dropFilesPod(context.WithoutCancel(ctx), p)
			if attempt == 0 && !write && ctx.Err() == nil {
				continue
			}
			return nil, fmt.Errorf("files pod: %w", err)
		}
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&e)
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusNotFound:
			return nil, ErrNotFound
		case e.Error == "not_a_directory":
			return nil, &JobRefused{msg.T("Kein Ordner.", "Not a folder.")}
		case e.Error == "not_a_file":
			return nil, &JobRefused{msg.T("Keine Datei (ein Ordner oder etwas anderes).", "Not a file (a folder or something else).")}
		case e.Error == "parent_not_a_directory":
			return nil, &JobRefused{msg.T("Ein Teil des Pfads ist eine Datei, kein Ordner.", "A part of the path is a file, not a folder.")}
		case e.Error == "no_space":
			return nil, &JobRefused{msg.T("Kein Platz mehr im Home: die Speichergrenze des Projekts ist erreicht.", "No space left in the home: the project's storage limit is reached.")}
		case e.Error == "incomplete":
			return nil, &JobRefused{msg.T("Die Datei kam nicht vollständig an.", "The file did not arrive completely.")}
		case resp.StatusCode == http.StatusForbidden && write:
			return nil, &JobRefused{msg.T("Nicht schreibbar.", "Not writable.")}
		case resp.StatusCode == http.StatusForbidden:
			return nil, &JobRefused{msg.T("Nicht lesbar.", "Not readable.")}
		}
		return nil, fmt.Errorf("files pod: %d %s", resp.StatusCode, e.Error)
	}
}

// filesBody is the body of a files pod's answer; closing it ends the transfer (done, once).
type filesBody struct {
	io.ReadCloser
	once sync.Once
	done func()
}

func (b *filesBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.done)
	return err
}

// doneFilesPod ends a transfer through the pod: it counts as used now, and one replaced meanwhile is deleted after its last transfer.
func (s *Service) doneFilesPod(ctx context.Context, p *filesPod) {
	s.files.mu.Lock()
	p.busy--
	p.used = time.Now()
	gone := p.busy == 0 && s.files.pods[p.key] != p
	s.files.mu.Unlock()
	if gone {
		s.dropFilesPod(ctx, p)
	}
}

// filesAlive says whether the pod's server answers.
func (s *Service) filesAlive(ctx context.Context, p *filesPod) bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.files.addr(p.ip)+"/healthz", nil)
	resp, err := s.files.http.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func filesKey(project, person string, access filesAccess) string {
	return project + "\x00" + strings.ToLower(person) + "\x00" + strconv.Itoa(int(access))
}

// filesPod is the running files pod of the person in the project with the access, started if there is none (or the one there is too old).
func (s *Service) filesPod(ctx context.Context, project, person string, access filesAccess) (*filesPod, error) {
	key := filesKey(project, person, access)
	f := &s.files
	f.mu.Lock()
	if f.starting == nil {
		f.starting = map[string]*sync.Mutex{}
	}
	start := f.starting[key]
	if start == nil {
		start = &sync.Mutex{}
		f.starting[key] = start
	}
	f.mu.Unlock()

	start.Lock()
	defer start.Unlock()
	now := time.Now()
	f.mu.Lock()
	p := f.pods[key]
	if p != nil && now.Sub(p.created) < filesReuse {
		p.used = now
		f.mu.Unlock()
		return p, nil
	}
	busy := p != nil && p.busy > 0
	if busy {
		// Too old to be used again, but a transfer still runs through it: forgotten here, deleted when that ends (doneFilesPod).
		delete(f.pods, key)
	}
	f.mu.Unlock()
	if p != nil && !busy {
		s.dropFilesPod(ctx, p)
	}
	p, err := s.startFilesPod(ctx, project, person, access)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	if f.pods == nil {
		f.pods = map[string]*filesPod{}
	}
	f.pods[key] = p
	f.mu.Unlock()
	return p, nil
}

// dropFilesPod forgets the pod and deletes it.
func (s *Service) dropFilesPod(ctx context.Context, p *filesPod) {
	s.files.mu.Lock()
	if s.files.pods[p.key] == p {
		delete(s.files.pods, p.key)
	}
	s.files.mu.Unlock()
	if err := s.k8s.do(ctx, http.MethodDelete, "/api/v1/namespaces/"+s.workspaceNamespace()+"/pods/"+p.name, nil, nil); err != nil && !errors.Is(err, ErrNotFound) {
		s.log.Warnw("files pod not deleted", "pod", p.name, "error", err)
	}
}

// startFilesPod creates the pod and waits until its server answers.
func (s *Service) startFilesPod(ctx context.Context, project, person string, access filesAccess) (*filesPod, error) {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	token := hex.EncodeToString(b)
	_, _ = rand.Read(b[:4])
	p := &filesPod{key: filesKey(project, person, access), name: filesApp + "-" + hex.EncodeToString(b[:4]), token: token, created: time.Now(), used: time.Now()}
	ns := s.workspaceNamespace()
	if err := s.k8s.do(ctx, http.MethodPost, "/api/v1/namespaces/"+ns+"/pods", s.filesPodObject(p, project, person, access), nil); err != nil {
		return nil, fmt.Errorf("files pod: %w", err)
	}
	s.log.Infow("files pod started", "pod", p.name, "project", project, "person", person, "access", access)
	ctx, cancel := context.WithTimeout(ctx, filesStart)
	defer cancel()
	fail := func(err error) (*filesPod, error) {
		s.dropFilesPod(context.WithoutCancel(ctx), p)
		return nil, err
	}
	err := retry.Do(ctx, filesPoll, func(ctx context.Context) error {
		var pod struct {
			Status struct {
				Phase string `json:"phase"`
				PodIP string `json:"podIP"`
			} `json:"status"`
		}
		if err := s.k8s.do(ctx, http.MethodGet, "/api/v1/namespaces/"+ns+"/pods/"+p.name, nil, &pod); err != nil && !errors.Is(err, ErrNotFound) {
			return retry.Permanent(fmt.Errorf("files pod: %w", err))
		}
		switch pod.Status.Phase {
		case "Failed", "Succeeded":
			return retry.Permanent(fmt.Errorf("files pod %s ended (%s)", p.name, pod.Status.Phase))
		case "Running":
			if pod.Status.PodIP != "" {
				p.ip = pod.Status.PodIP
				if s.filesAlive(ctx, p) {
					return nil
				}
			}
		}
		return errFilesNotUp
	})
	switch {
	case err == nil:
		return p, nil
	case errors.Is(err, errFilesNotUp):
		return fail(errors.New("files pod did not start in time"))
	default:
		return fail(err)
	}
}

// errFilesNotUp is a files pod that is not answering yet; filesPoll asks again every 500 ms until filesStart is over.
var errFilesNotUp = errors.New("files pod not up yet")

var filesPoll = retry.Policy{Total: filesStart, Initial: 500 * time.Millisecond, Max: 500 * time.Millisecond}

// filesPodObject is the files pod: the person's home folder of the project's volume at /data and the project's shared folder at /data/project-shared, read-only unless the pod is for uploads (component=upload, its server then takes PUT; the shared folder stays read-only for filesWriteHome). Both folders and the mount point in the home are made first as in a job (the folder of a person whose job never started may not exist yet, Kubernetes would create a missing subPath as root, and a read-only home cannot take a new mount point); the init image, nothing of the host, no service account, on a CPU node, ended by Kubernetes at filesDeadline.
func (s *Service) filesPodObject(p *filesPod, project, person string, access filesAccess) map[string]any {
	home := "homes/" + HubUserSlug(person)
	component, mode := "files", "ro"
	if access != filesRead {
		component, mode = "upload", "rw"
	}
	sec := map[string]any{"runAsUser": 1000, "runAsGroup": 100, "runAsNonRoot": true, "allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": map[string]any{"drop": []any{"ALL"}}}
	return map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{
			"name":   p.name,
			"labels": map[string]any{"app": filesApp, "component": component, "dhbw.cloud/project": project, jobLabelUser: hubLabel(person)},
		},
		"spec": map[string]any{
			"restartPolicy":                 "Never",
			"activeDeadlineSeconds":         int64(filesDeadline / time.Second),
			"terminationGracePeriodSeconds": 1,
			"automountServiceAccountToken":  false,
			"enableServiceLinks":            false,
			"nodeSelector":                  map[string]any{"dhbw.cloud/node-type": "cpu"},
			"securityContext":               map[string]any{"runAsUser": 1000, "runAsGroup": 100, "runAsNonRoot": true},
			"volumes":                       []any{map[string]any{"name": "home", "persistentVolumeClaim": map[string]any{"claimName": "ws-" + project}}},
			"initContainers": []any{map[string]any{
				"name": "dhbw-dirs", "image": s.cfg.Jobs.InitImage,
				"command":         []any{"sh", "-c", `mkdir -p "/ws/$DHBW_HOME_DIR/project-shared" /ws/project-shared`},
				"env":             []any{envVar("DHBW_HOME_DIR", home)},
				"securityContext": sec,
				"resources":       map[string]any{"requests": map[string]any{"cpu": "10m", "memory": "32Mi"}, "limits": map[string]any{"memory": "128Mi"}},
				"volumeMounts":    []any{map[string]any{"name": "home", "mountPath": "/ws"}},
			}},
			"containers": []any{map[string]any{
				"name":            "files",
				"image":           s.cfg.Jobs.InitImage,
				"command":         []any{"python3", "-I", "-c", filesServer, "/data", strconv.Itoa(filesPort), mode},
				"env":             []any{envVar("DHBW_FILES_TOKEN", p.token)},
				"ports":           []any{map[string]any{"name": "files", "containerPort": filesPort}},
				"securityContext": sec,
				"resources":       map[string]any{"requests": map[string]any{"cpu": "10m", "memory": "48Mi"}, "limits": map[string]any{"cpu": "1", "memory": "256Mi"}},
				"volumeMounts": []any{
					map[string]any{"name": "home", "mountPath": "/data", "subPath": home, "readOnly": access == filesRead},
					map[string]any{"name": "home", "mountPath": "/data/" + SharedDir, "subPath": SharedDir, "readOnly": access != filesWrite},
				},
			}},
		},
	}
}

// SweepFilesPods deletes files pods, called every minute from the start of the service (with the batch jobs' sync, internal/app.go): its own unused for filesIdle, and every ended one (activeDeadlineSeconds); a running one it does not know may be another replica's and is left to its deadline. Started from the first request instead, it never ran after a restart without one, and an ended pod stayed for days (05.11.2026).
func (s *Service) SweepFilesPods(ctx context.Context) { s.sweepFilesPods(ctx, time.Now()) }

func (s *Service) sweepFilesPods(ctx context.Context, now time.Time) {
	s.files.mu.Lock()
	known := map[string]bool{}
	var idle []*filesPod
	for key, p := range s.files.pods {
		if p.busy == 0 && now.Sub(p.used) > filesIdle {
			idle = append(idle, p)
			delete(s.files.pods, key)
			continue
		}
		known[p.name] = true
	}
	s.files.mu.Unlock()
	ns := s.workspaceNamespace()
	for _, p := range idle {
		if err := s.k8s.do(ctx, http.MethodDelete, "/api/v1/namespaces/"+ns+"/pods/"+p.name, nil, nil); err != nil && !errors.Is(err, ErrNotFound) {
			s.log.Warnw("files pod not deleted", "pod", p.name, "error", err)
		}
	}
	var pods struct {
		Items []podInfo `json:"items"`
	}
	if err := s.k8s.do(ctx, http.MethodGet, "/api/v1/namespaces/"+ns+"/pods?"+url.Values{"labelSelector": {"app=" + filesApp}}.Encode(), nil, &pods); err != nil {
		s.log.Warnw("files pods not listed", "error", err)
		return
	}
	for _, p := range pods.Items {
		if known[p.Metadata.Name] || p.Metadata.DeletionTimestamp != nil {
			continue
		}
		if p.Status.Phase == "Running" || p.Status.Phase == "Pending" {
			if now.Sub(p.Metadata.CreationTimestamp) < filesDeadline {
				continue
			}
		}
		if err := s.k8s.do(ctx, http.MethodDelete, "/api/v1/namespaces/"+ns+"/pods/"+p.Metadata.Name, nil, nil); err != nil && !errors.Is(err, ErrNotFound) {
			s.log.Warnw("files pod not deleted", "pod", p.Metadata.Name, "error", err)
		}
	}
}

// defaultFilesAddr is the files server of a pod address.
func defaultFilesAddr(ip string) string {
	return "http://" + net.JoinHostPort(ip, strconv.Itoa(filesPort))
}

// SetFilesAddr points the files pods' server elsewhere: tests of other packages run one of their own.
func (s *Service) SetFilesAddr(f func(ip string) string) { s.files.addr = f }
