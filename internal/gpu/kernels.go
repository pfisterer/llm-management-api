package gpu

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// GPU kernels in their own pods (docs/bauplan.md 6.2): the notebook server of "CPU + GPU-Kernel" asks here for a kernel pod instead of creating it itself, so it needs no rights in Kubernetes. Who asks is the server pod with the request's source address (checked against the person's hub token by the caller); the pod is built from that server pod — its image, its home and shared folder as it mounts them, its person and project —, the request only adds the kernel's connection parameters.

// KernelConfig is how kernel pods are built; zero values take the defaults.
type KernelConfig struct {
	Queue string // Kueue LocalQueue in the hub's namespace for kernels without GPU (NoGPU)
	NoGPU bool   // kernels without a GPU (staging, which has none)
}

// KernelOwner is the server pod a kernel belongs to.
type KernelOwner struct {
	Pod         string
	UID         string
	IP          string
	Person      string // JupyterHub user name (annotation hub.jupyter.org/username)
	UserLabel   string // its label hub.jupyter.org/username (escaped), shared by the person's servers and kernels
	Project     string
	Profile     string
	GPUClass    string // chosen at the server's start (label dhbw.cloud/gpu-class, set by the hub); the kernel goes to its queue
	Image       string
	Arch        string // the server's node selector kubernetes.io/arch (set by the hub when its image has only that architecture); the kernel keeps it
	pullSecrets []any  // the server's imagePullSecrets (its project's, for a private image); the kernel runs the same image
	container   map[string]any
	volumes     []any
}

// KernelRequest is what the notebook server's provisioner knows about a kernel to start (gateway_provisioners' launch arguments).
type KernelRequest struct {
	KernelID            string `json:"kernel_id"`
	ResponseAddress     string `json:"response_address"`
	PublicKey           string `json:"public_key"`
	PortRange           string `json:"port_range"`
	TransportEncryption string `json:"transport_encryption"`
	KernelClassName     string `json:"kernel_class_name"`
	WorkingDir          string `json:"working_dir"`
}

// KernelState is one kernel pod as the provisioner polls it.
type KernelState struct {
	Name     string    `json:"name"`
	Phase    string    `json:"phase"`
	PodIP    string    `json:"pod_ip,omitempty"`
	HostIP   string    `json:"host_ip,omitempty"`
	Created  time.Time `json:"created"`
	Admitted bool      `json:"admitted"`           // Kueue gave it a GPU
	Position int       `json:"position,omitempty"` // place in the queue while waiting, 1 = next
	// EndsAt is when the running kernel pod ends at the latest (its start plus activeDeadlineSeconds, the project's upper bound); nil while it waits or without an upper bound.
	EndsAt *time.Time `json:"ends_at,omitempty"`
}

// KernelOverview is the person's GPU kernels for the status bar in JupyterLab, with the times running GPU pods end at the latest (what a waiting kernel can expect).
type KernelOverview struct {
	Kernels []KernelOverviewEntry `json:"kernels"`
	Ends    []time.Time           `json:"ends"`
}

type KernelOverviewEntry struct {
	KernelID string `json:"kernel_id"`
	KernelState
}

// ErrNoKernelOwner is a request from an address that is no running server of "CPU + GPU-Kernel".
var ErrNoKernelOwner = errors.New("no workspace server at this address")

// KernelRefused is a start the limits or the request do not allow; Reason is shown in JupyterLab.
type KernelRefused struct{ Reason string }

func (e *KernelRefused) Error() string { return e.Reason }

type kubeDoer interface {
	do(ctx context.Context, method, path string, body, out any) error
}

// SplitProfile is the profile whose servers may start GPU kernels.
const SplitProfile = "cpu-gpukernel"

const kernelLabelOwner = "dhbw.cloud/kernel-owner"

var (
	kernelIDPattern  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	portRangePattern = regexp.MustCompile(`^[0-9]{1,5}\.\.[0-9]{1,5}$`)
	publicKeyPattern = regexp.MustCompile(`^[A-Za-z0-9+/=_-]*$`)
	classNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.]{0,200}$`)
)

// SetKernelCheck sets what decides whether a person may start one more GPU kernel now (the project's limits and budgets); a *KernelRefused tells why not.
func (s *Service) SetKernelCheck(f func(ctx context.Context, o *KernelOwner) error) {
	s.kernelCheck = f
}

// DefaultKernelMax is how long a GPU kernel runs at the latest without SetKernelMax: 4 hours, as before release times per project.
const DefaultKernelMax = 4 * time.Hour

// SetKernelMax sets how long a GPU kernel of the owner's project may run at the latest (its release times, docs/todo.md idle_release); 0 = no upper bound (the project's own GPUs). The kernel pod's activeDeadlineSeconds.
func (s *Service) SetKernelMax(f func(ctx context.Context, o *KernelOwner) time.Duration) {
	s.kernelMax = f
}

func (s *Service) kernelNamespace() string {
	if s.cfg.WorkspaceNamespace != "" {
		return s.cfg.WorkspaceNamespace
	}
	return "jupyterhub"
}

// tokenCache remembers for a few minutes whose a hub token is, so that the provisioner's polling does not ask the hub every second.
type tokenCache struct {
	mu   sync.Mutex
	hits map[string]tokenHit
}

type tokenHit struct {
	user  string
	until time.Time
}

// KernelCaller is the server asking: the server pod at the request's source address, whose person must own the request's hub token. The address says which server (pods cannot fake their source address: no NET_RAW, NetworkPolicy), the token that it is still the person's running server and not some other pod that got the address.
func (s *Service) KernelCaller(ctx context.Context, ip, token string) (*KernelOwner, error) {
	if s.whoami == nil || token == "" {
		return nil, ErrNoKernelOwner
	}
	sum := sha256.Sum256([]byte(token))
	key := hex.EncodeToString(sum[:])
	s.tokens.mu.Lock()
	hit, ok := s.tokens.hits[key]
	s.tokens.mu.Unlock()
	if !ok || time.Now().After(hit.until) {
		user, err := s.whoami(ctx, token)
		if errors.Is(err, ErrNotFound) {
			return nil, ErrNoKernelOwner
		}
		if err != nil {
			return nil, err
		}
		hit = tokenHit{user: user, until: time.Now().Add(5 * time.Minute)}
		s.tokens.mu.Lock()
		if s.tokens.hits == nil || len(s.tokens.hits) > 10000 {
			s.tokens.hits = map[string]tokenHit{}
		}
		s.tokens.hits[key] = hit
		s.tokens.mu.Unlock()
	}
	o, err := s.KernelOwnerAt(ctx, ip)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(o.Person, hit.user) {
		return nil, ErrNoKernelOwner
	}
	return o, nil
}

// KernelOwnerAt finds the running server pod of "CPU + GPU-Kernel" with this address.
func (s *Service) KernelOwnerAt(ctx context.Context, ip string) (*KernelOwner, error) {
	if s.k8s == nil || net.ParseIP(ip) == nil {
		return nil, ErrNoKernelOwner
	}
	var pods struct {
		Items []map[string]any `json:"items"`
	}
	q := url.Values{"labelSelector": {"component=singleuser-server"}, "fieldSelector": {"status.podIP=" + ip + ",status.phase=Running"}}
	if err := s.k8s.do(ctx, http.MethodGet, "/api/v1/namespaces/"+s.kernelNamespace()+"/pods?"+q.Encode(), nil, &pods); err != nil {
		return nil, err
	}
	for _, p := range pods.Items {
		meta, spec, status := obj(p, "metadata"), obj(p, "spec"), obj(p, "status")
		if str(status, "podIP") != ip || meta["deletionTimestamp"] != nil {
			continue
		}
		labels, annotations := obj(meta, "labels"), obj(meta, "annotations")
		o := &KernelOwner{Pod: str(meta, "name"), UID: str(meta, "uid"), IP: ip, Person: str(annotations, "hub.jupyter.org/username"),
			UserLabel: str(labels, "hub.jupyter.org/username"), Project: str(labels, "dhbw.cloud/project"), Profile: str(labels, "dhbw.cloud/profile"), GPUClass: str(labels, labelClass)}
		for _, c := range list(spec, "containers") {
			if c, ok := c.(map[string]any); ok && str(c, "name") == "notebook" {
				o.container, o.Image = c, str(c, "image")
			}
		}
		o.volumes = list(spec, "volumes")
		o.Arch = str(obj(spec, "nodeSelector"), "kubernetes.io/arch")
		o.pullSecrets = list(spec, "imagePullSecrets")
		if o.Profile != SplitProfile || o.Person == "" || o.container == nil {
			return nil, ErrNoKernelOwner
		}
		return o, nil
	}
	return nil, ErrNoKernelOwner
}

// StartKernel checks the request and the limits and creates the kernel pod.
func (s *Service) StartKernel(ctx context.Context, o *KernelOwner, r KernelRequest) (string, error) {
	if err := validKernelRequest(o, &r); err != nil {
		return "", err
	}
	if o.GPUClass == "" && !s.cfg.Kernels.NoGPU {
		// A server started before GPU classes, or without one chosen.
		return "", &KernelRefused{"Für diesen Workspace ist keine GPU-Klasse gewählt. Bitte den Workspace neu starten."}
	}
	if s.kernelCheck != nil {
		if err := s.kernelCheck(ctx, o); err != nil {
			return "", err
		}
	}
	deadline := DefaultKernelMax
	if s.kernelMax != nil {
		deadline = s.kernelMax(ctx, o)
	}
	pod := s.kernelPod(o, r, deadline)
	name := pod["metadata"].(map[string]any)["name"].(string)
	if err := s.k8s.do(ctx, http.MethodPost, "/api/v1/namespaces/"+s.kernelNamespace()+"/pods", pod, nil); err != nil {
		return "", err
	}
	s.log.Infow("gpu kernel started", "pod", name, "owner", o.Pod, "person", o.Person, "project", o.Project)
	return name, nil
}

func validKernelRequest(o *KernelOwner, r *KernelRequest) error {
	host, _, err := net.SplitHostPort(r.ResponseAddress)
	switch {
	case !kernelIDPattern.MatchString(r.KernelID):
		return &KernelRefused{"Ungültige Kernel-ID."}
	case err != nil || host != o.IP:
		// The kernel reports its ports to the server that asked, nowhere else.
		return &KernelRefused{"Ungültige Rückmeldeadresse."}
	case r.PortRange != "" && !portRangePattern.MatchString(r.PortRange):
		return &KernelRefused{"Ungültiger Portbereich."}
	case len(r.PublicKey) > 4096 || !publicKeyPattern.MatchString(r.PublicKey):
		return &KernelRefused{"Ungültiger Schlüssel."}
	case !classNamePattern.MatchString(r.KernelClassName):
		return &KernelRefused{"Ungültige Kernel-Klasse."}
	case r.TransportEncryption != "" && r.TransportEncryption != "auto" && r.TransportEncryption != "required":
		return &KernelRefused{"Ungültige Transportverschlüsselung."}
	}
	if r.WorkingDir != "" {
		d := path.Clean(r.WorkingDir)
		if d != "/home/jovyan" && !strings.HasPrefix(d, "/home/jovyan/") {
			return &KernelRefused{"Arbeitsverzeichnis außerhalb des Homes."}
		}
		r.WorkingDir = d
	}
	return nil
}

// kernelPod is the kernel's pod: the image and the home mounts of its server (read-only stays read-only), the owner's labels for counting and booking, the queue for the GPU, the launcher's parameters as environment, and deadline as its upper bound (0 = none). Nothing else of the server's environment goes along (no hub token), only the LLM key of the project (OPENAI_*).
func (s *Service) kernelPod(o *KernelOwner, r KernelRequest, deadline time.Duration) map[string]any {
	queue := s.kernelQueue(o)
	suffix := make([]byte, 3)
	_, _ = rand.Read(suffix)
	name := "kernel-" + r.KernelID[:8] + "-" + hex.EncodeToString(suffix)

	env := []any{
		envVar("KERNEL_ID", r.KernelID), envVar("KERNEL_LANGUAGE", "python"), envVar("KERNEL_NAME", "python3-gpu"),
		envVar("RESPONSE_ADDRESS", r.ResponseAddress), envVar("PUBLIC_KEY", r.PublicKey), envVar("PORT_RANGE", r.PortRange),
		envVar("KERNEL_SPARK_CONTEXT_INIT_MODE", "none"), envVar("KERNEL_USERNAME", o.Person),
	}
	if r.KernelClassName != "" {
		env = append(env, envVar("KERNEL_CLASS_NAME", r.KernelClassName))
	}
	if r.TransportEncryption != "" {
		env = append(env, envVar("TRANSPORT_ENCRYPTION", r.TransportEncryption))
	}
	for _, e := range list(o.container, "env") {
		if e, ok := e.(map[string]any); ok && strings.HasPrefix(str(e, "name"), "OPENAI_") {
			env = append(env, e)
		}
	}
	// The home as the server mounts it: the person's directory in the project's volume and the shared folder, each with the server's readOnly.
	var mounts []any
	for _, m := range list(o.container, "volumeMounts") {
		if m, ok := m.(map[string]any); ok && str(m, "name") == "home" {
			mount := map[string]any{"name": "home", "mountPath": str(m, "mountPath")}
			if sp := str(m, "subPath"); sp != "" {
				mount["subPath"] = sp
			}
			if ro, _ := m["readOnly"].(bool); ro {
				mount["readOnly"] = true
			}
			mounts = append(mounts, mount)
		}
	}
	volume := map[string]any{"name": "home", "emptyDir": map[string]any{}}
	for _, v := range o.volumes {
		if v, ok := v.(map[string]any); ok && str(v, "name") == "home" {
			if pvc := obj(v, "persistentVolumeClaim"); str(pvc, "claimName") != "" {
				volume = map[string]any{"name": "home", "persistentVolumeClaim": map[string]any{"claimName": str(pvc, "claimName")}}
			}
		}
	}
	if len(mounts) == 0 {
		mounts = []any{map[string]any{"name": "home", "mountPath": "/home/jovyan"}}
	}

	requests := map[string]any{"cpu": "1", "memory": "2G"}
	limits := map[string]any{"cpu": "3", "memory": "5G"}
	spec := map[string]any{
		"restartPolicy":                "Never",
		"automountServiceAccountToken": false,
		"enableServiceLinks":           false,
		"securityContext":              map[string]any{"runAsUser": 1000, "runAsGroup": 100, "fsGroup": 100},
		"volumes":                      []any{volume},
	}
	if deadline > 0 {
		spec["activeDeadlineSeconds"] = int64(deadline / time.Second)
	}
	if !s.cfg.Kernels.NoGPU {
		requests["nvidia.com/gpu"], limits["nvidia.com/gpu"] = "1", "1"
		spec["tolerations"] = []any{map[string]any{"key": "dhbw.cloud/gpu-lab", "operator": "Equal", "value": "true", "effect": "NoSchedule"}}
	}
	if o.Arch != "" {
		spec["nodeSelector"] = map[string]any{"kubernetes.io/arch": o.Arch}
	}
	if len(o.pullSecrets) > 0 {
		spec["imagePullSecrets"] = o.pullSecrets
	}
	container := map[string]any{
		"name":  "kernel",
		"image": o.Image,
		// The launcher of the base, whatever CMD a derived image sets; the entrypoint of Docker Stacks (tini, start.sh) stays.
		"args":            []any{"/usr/local/bin/bootstrap-kernel.sh"},
		"env":             env,
		"securityContext": map[string]any{"allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []any{"ALL"}}},
		"resources":       map[string]any{"requests": requests, "limits": limits},
		"volumeMounts":    mounts,
	}
	if r.WorkingDir != "" {
		container["workingDir"] = r.WorkingDir
	}
	spec["containers"] = []any{container}
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name": name,
			"labels": map[string]any{
				"kernel_id": r.KernelID, "app": "gateway-provisioners", "component": "kernel",
				"hub.jupyter.org/network-access-singleuser": "true",
				"kueue.x-k8s.io/queue-name":                 queue,
				"dhbw.cloud/kernel-user":                    o.UserLabel,
				kernelLabelOwner:                            o.Pod,
				labelClass:                                  o.GPUClass,
				"dhbw.cloud/project":                        o.Project,
			},
			// Deleted with its server.
			"ownerReferences": []any{map[string]any{"apiVersion": "v1", "kind": "Pod", "name": o.Pod, "uid": o.UID}},
		},
		"spec": spec,
	}
}

// KernelStatus is the live pod of one kernel of this server; nil when there is none (not yet created, deleted, evicted).
func (s *Service) KernelStatus(ctx context.Context, o *KernelOwner, kernelID string) (*KernelState, error) {
	pods, err := s.kernelPods(ctx, "component=kernel,kernel_id="+kernelID+","+kernelLabelOwner+"="+o.Pod)
	if err != nil || len(pods) == 0 {
		return nil, err
	}
	st := kernelState(pods[0])
	return &st, nil
}

// StopKernel deletes the pods of one kernel of this server.
func (s *Service) StopKernel(ctx context.Context, o *KernelOwner, kernelID string) error {
	if !kernelIDPattern.MatchString(kernelID) {
		return &KernelRefused{"Ungültige Kernel-ID."}
	}
	q := url.Values{"labelSelector": {"component=kernel,kernel_id=" + kernelID + "," + kernelLabelOwner + "=" + o.Pod}, "gracePeriodSeconds": {"0"}}
	err := s.k8s.do(ctx, http.MethodDelete, "/api/v1/namespaces/"+s.kernelNamespace()+"/pods?"+q.Encode(), nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// KernelOverview lists the person's GPU kernels (all their servers) with Kueue's view: admitted or the place in the queue.
func (s *Service) KernelOverview(ctx context.Context, o *KernelOwner) (KernelOverview, error) {
	out := KernelOverview{Kernels: []KernelOverviewEntry{}, Ends: []time.Time{}}
	pods, err := s.kernelPods(ctx, "component=kernel,dhbw.cloud/kernel-user="+o.UserLabel)
	if err != nil || len(pods) == 0 {
		return out, err
	}
	ns := s.kernelNamespace()
	var wls struct {
		Items []map[string]any `json:"items"`
	}
	if err := s.k8s.do(ctx, http.MethodGet, "/apis/kueue.x-k8s.io/v1beta2/namespaces/"+ns+"/workloads", nil, &wls); err != nil {
		return out, err
	}
	workloadOf := map[string]map[string]any{}
	for _, w := range wls.Items {
		for _, ref := range list(obj(w, "metadata"), "ownerReferences") {
			if ref, ok := ref.(map[string]any); ok && str(ref, "kind") == "Pod" {
				workloadOf[str(ref, "name")] = w
			}
		}
	}
	queue := s.kernelQueue(o)
	var pending struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Position int `json:"positionInClusterQueue"`
		} `json:"items"`
	}
	position := map[string]int{}
	if err := s.k8s.do(ctx, http.MethodGet, "/apis/visibility.kueue.x-k8s.io/v1beta2/namespaces/"+ns+"/localqueues/"+queue+"/pendingworkloads", nil, &pending); err == nil {
		for _, w := range pending.Items {
			position[w.Metadata.Name] = w.Position + 1
		}
	}
	for _, p := range pods {
		st := kernelState(p)
		if w := workloadOf[st.Name]; w != nil {
			st.Admitted = obj(w, "status")["admission"] != nil
			st.Position = position[str(obj(w, "metadata"), "name")]
		}
		out.Kernels = append(out.Kernels, KernelOverviewEntry{KernelID: str(obj(obj(p, "metadata"), "labels"), "kernel_id"), KernelState: st})
	}
	// When GPUs free up at the latest: running GPU pods anywhere end at their deadline; pods with negative priority (the inference gap fillers) do not count, Kueue evicts them for a waiting kernel.
	var running struct {
		Items []podInfo `json:"items"`
	}
	if err := s.k8s.do(ctx, http.MethodGet, "/api/v1/pods?fieldSelector="+url.QueryEscape("status.phase=Running"), nil, &running); err == nil {
		for _, p := range running.Items {
			if p.Status.StartTime == nil || p.Spec.ActiveDeadlineSeconds == 0 || p.Spec.Priority < 0 || !holdsGPU(p) {
				continue
			}
			out.Ends = append(out.Ends, p.Status.StartTime.Add(time.Duration(p.Spec.ActiveDeadlineSeconds)*time.Second))
		}
		sort.Slice(out.Ends, func(i, j int) bool { return out.Ends[i].Before(out.Ends[j]) })
	}
	return out, nil
}

// kernelQueue is the LocalQueue of the server's GPU class, or the configured one without GPUs (staging).
func (s *Service) kernelQueue(o *KernelOwner) string {
	if s.cfg.Kernels.NoGPU {
		return s.cfg.Kernels.Queue
	}
	return "gpu-" + o.GPUClass
}

func holdsGPU(p podInfo) bool {
	for _, c := range p.Spec.Containers {
		if c.Resources.Requests["nvidia.com/gpu"] != "" || c.Resources.Limits["nvidia.com/gpu"] != "" {
			return true
		}
	}
	return false
}

// kernelPods are the live kernel pods matching the selector, oldest first; terminating ones are left out (a restart creates the next before the last is gone).
func (s *Service) kernelPods(ctx context.Context, selector string) ([]map[string]any, error) {
	if s.k8s == nil {
		return nil, nil
	}
	var pods struct {
		Items []map[string]any `json:"items"`
	}
	if err := s.k8s.do(ctx, http.MethodGet, "/api/v1/namespaces/"+s.kernelNamespace()+"/pods?labelSelector="+url.QueryEscape(selector), nil, &pods); err != nil {
		return nil, err
	}
	var out []map[string]any
	for _, p := range pods.Items {
		phase := str(obj(p, "status"), "phase")
		if obj(p, "metadata")["deletionTimestamp"] == nil && (phase == "Pending" || phase == "Running") {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return str(obj(out[i], "metadata"), "creationTimestamp") < str(obj(out[j], "metadata"), "creationTimestamp")
	})
	return out, nil
}

func kernelState(p map[string]any) KernelState {
	meta, status := obj(p, "metadata"), obj(p, "status")
	created, _ := time.Parse(time.RFC3339, str(meta, "creationTimestamp"))
	st := KernelState{Name: str(meta, "name"), Phase: strings.ToLower(str(status, "phase")), PodIP: str(status, "podIP"), HostIP: str(status, "hostIP"), Created: created}
	// The deadline counts from the pod's start on its node (startTime), not from its creation: the wait in the queue is not part of it.
	if started, err := time.Parse(time.RFC3339, str(status, "startTime")); err == nil {
		if d, ok := obj(p, "spec")["activeDeadlineSeconds"].(float64); ok && d > 0 {
			end := started.Add(time.Duration(d) * time.Second)
			st.EndsAt = &end
		}
	}
	return st
}

func envVar(name, value string) map[string]any { return map[string]any{"name": name, "value": value} }

func obj(m map[string]any, key string) map[string]any {
	v, _ := m[key].(map[string]any)
	if v == nil {
		return map[string]any{}
	}
	return v
}

func list(m map[string]any, key string) []any {
	v, _ := m[key].([]any)
	return v
}

func str(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}
