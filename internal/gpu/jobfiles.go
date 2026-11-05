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
)

// Files of a job (docs/architektur.md "Batch-Jobs", todo job_files): whoever only submits jobs has no workspace to fetch results from. A short-lived pod in the workspace namespace mounts the person's home folder of the project's volume (ws-<slug>, subPath homes/<slug>) read-only and serves it to this service alone, over the pod network with a token of its own; this service streams it on. No exec into pods and no new rights in Kubernetes: the service already creates and deletes pods there (GPU kernels). The pod runs the hub's init image (a base with python3, prepulled on every node) with the small server files_server.py; its NetworkPolicy admits this service only, its admission policy keeps the home read-only.

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
	// jobFilesMax is how many entries of a folder a listing returns (files_server.py stops there).
	jobFilesMax = 5000
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

// filesPod is a running files pod of a person in a project.
type filesPod struct {
	name, ip, token string
	created, used   time.Time
}

type filesPods struct {
	mu   sync.Mutex
	pods map[string]*filesPod
	// starting serialises the start of one person's pod in one project: parallel requests share it.
	starting map[string]*sync.Mutex
	janitor  sync.Once
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
	resp, err := s.filesGet(ctx, j, "/list", rel)
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
	resp, err := s.filesGet(ctx, j, "/file", rel)
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
		return nil, 0, &JobRefused{fmt.Sprintf("Die Datei ist größer als %d GB; große Ergebnisse besser in S3 oder einem Workspace abholen.", JobFileMaxBytes>>30)}
	}
	return resp.Body, size, nil
}

// filesGet asks the files pod of the job's person and project; a pod that went away meanwhile is started anew once.
func (s *Service) filesGet(ctx context.Context, j Job, op, rel string) (*http.Response, error) {
	if s.k8s == nil || s.cfg.Jobs.InitImage == "" {
		return nil, &JobRefused{"Dateien von Jobs sind in diesem Cluster nicht eingerichtet."}
	}
	for attempt := 0; ; attempt++ {
		p, err := s.filesPod(ctx, j.Project, j.Person)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.files.addr(p.ip)+op+"?"+url.Values{"path": {rel}}.Encode(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+p.token)
		resp, err := s.files.http.Do(req)
		if err != nil {
			s.dropFilesPod(context.WithoutCancel(ctx), j.Project, j.Person, p)
			if attempt == 0 && ctx.Err() == nil {
				continue
			}
			return nil, fmt.Errorf("files pod: %w", err)
		}
		if resp.StatusCode == http.StatusOK {
			return resp, nil
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
			return nil, &JobRefused{"Kein Ordner."}
		case e.Error == "not_a_file":
			return nil, &JobRefused{"Keine Datei (ein Ordner oder etwas anderes)."}
		case resp.StatusCode == http.StatusForbidden:
			return nil, &JobRefused{"Nicht lesbar."}
		}
		return nil, fmt.Errorf("files pod: %d %s", resp.StatusCode, e.Error)
	}
}

func filesKey(project, person string) string { return project + "\x00" + strings.ToLower(person) }

// filesPod is the running files pod of the person in the project, started if there is none (or the one there is too old).
func (s *Service) filesPod(ctx context.Context, project, person string) (*filesPod, error) {
	key := filesKey(project, person)
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
	f.janitor.Do(func() { go s.filesJanitor(context.WithoutCancel(ctx)) })

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
	f.mu.Unlock()
	if p != nil {
		s.dropFilesPod(ctx, project, person, p)
	}
	p, err := s.startFilesPod(ctx, project, person)
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
func (s *Service) dropFilesPod(ctx context.Context, project, person string, p *filesPod) {
	s.files.mu.Lock()
	if s.files.pods[filesKey(project, person)] == p {
		delete(s.files.pods, filesKey(project, person))
	}
	s.files.mu.Unlock()
	if err := s.k8s.do(ctx, http.MethodDelete, "/api/v1/namespaces/"+s.kernelNamespace()+"/pods/"+p.name, nil, nil); err != nil && !errors.Is(err, ErrNotFound) {
		s.log.Warnw("files pod not deleted", "pod", p.name, "error", err)
	}
}

// startFilesPod creates the pod and waits until its server answers.
func (s *Service) startFilesPod(ctx context.Context, project, person string) (*filesPod, error) {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	token := hex.EncodeToString(b)
	_, _ = rand.Read(b[:4])
	p := &filesPod{name: filesApp + "-" + hex.EncodeToString(b[:4]), token: token, created: time.Now(), used: time.Now()}
	ns := s.kernelNamespace()
	if err := s.k8s.do(ctx, http.MethodPost, "/api/v1/namespaces/"+ns+"/pods", s.filesPodObject(p, project, person), nil); err != nil {
		return nil, fmt.Errorf("files pod: %w", err)
	}
	s.log.Infow("files pod started", "pod", p.name, "project", project, "person", person)
	ctx, cancel := context.WithTimeout(ctx, filesStart)
	defer cancel()
	fail := func(err error) (*filesPod, error) {
		s.dropFilesPod(context.WithoutCancel(ctx), project, person, p)
		return nil, err
	}
	for {
		var pod struct {
			Status struct {
				Phase string `json:"phase"`
				PodIP string `json:"podIP"`
			} `json:"status"`
		}
		if err := s.k8s.do(ctx, http.MethodGet, "/api/v1/namespaces/"+ns+"/pods/"+p.name, nil, &pod); err != nil && !errors.Is(err, ErrNotFound) {
			return fail(fmt.Errorf("files pod: %w", err))
		}
		switch pod.Status.Phase {
		case "Failed", "Succeeded":
			return fail(fmt.Errorf("files pod %s ended (%s)", p.name, pod.Status.Phase))
		case "Running":
			if pod.Status.PodIP != "" {
				p.ip = pod.Status.PodIP
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.files.addr(p.ip)+"/healthz", nil)
				if resp, err := s.files.http.Do(req); err == nil {
					resp.Body.Close()
					if resp.StatusCode == http.StatusOK {
						return p, nil
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return fail(errors.New("files pod did not start in time"))
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// filesPodObject is the files pod: the person's home folder of the project's volume read-only at /data, made first as in a job (the folder of a person whose job never started may not exist yet, and Kubernetes would create a missing subPath as root); the init image, nothing of the host, no service account, on a CPU node, ended by Kubernetes at filesDeadline.
func (s *Service) filesPodObject(p *filesPod, project, person string) map[string]any {
	home := "homes/" + HubUserSlug(person)
	sec := map[string]any{"runAsUser": 1000, "runAsGroup": 100, "runAsNonRoot": true, "allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": map[string]any{"drop": []any{"ALL"}}}
	return map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{
			"name":   p.name,
			"labels": map[string]any{"app": filesApp, "component": "files", "dhbw.cloud/project": project, jobLabelUser: hubLabel(person)},
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
				"command":         []any{"sh", "-c", `mkdir -p "/ws/$DHBW_HOME_DIR"`},
				"env":             []any{envVar("DHBW_HOME_DIR", home)},
				"securityContext": sec,
				"resources":       map[string]any{"requests": map[string]any{"cpu": "10m", "memory": "32Mi"}, "limits": map[string]any{"memory": "128Mi"}},
				"volumeMounts":    []any{map[string]any{"name": "home", "mountPath": "/ws"}},
			}},
			"containers": []any{map[string]any{
				"name":            "files",
				"image":           s.cfg.Jobs.InitImage,
				"command":         []any{"python3", "-I", "-c", filesServer, "/data", strconv.Itoa(filesPort)},
				"env":             []any{envVar("DHBW_FILES_TOKEN", p.token)},
				"ports":           []any{map[string]any{"name": "files", "containerPort": filesPort}},
				"securityContext": sec,
				"resources":       map[string]any{"requests": map[string]any{"cpu": "10m", "memory": "48Mi"}, "limits": map[string]any{"cpu": "1", "memory": "256Mi"}},
				"volumeMounts":    []any{map[string]any{"name": "home", "mountPath": "/data", "subPath": home, "readOnly": true}},
			}},
		},
	}
}

// filesJanitor deletes files pods every minute: its own unused for filesIdle, and every ended one (activeDeadlineSeconds); a running one it does not know may be another replica's and is left to its deadline.
func (s *Service) filesJanitor(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.sweepFilesPods(ctx, time.Now())
	}
}

func (s *Service) sweepFilesPods(ctx context.Context, now time.Time) {
	s.files.mu.Lock()
	known := map[string]bool{}
	var idle []*filesPod
	for key, p := range s.files.pods {
		if now.Sub(p.used) > filesIdle {
			idle = append(idle, p)
			delete(s.files.pods, key)
			continue
		}
		known[p.name] = true
	}
	s.files.mu.Unlock()
	ns := s.kernelNamespace()
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
