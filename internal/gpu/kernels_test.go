package gpu

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeKernelKube answers the pod listing with a fixed server pod and records what is created and deleted.
type fakeKernelKube struct {
	server  string
	created []map[string]any
	deleted []string
}

func (f *fakeKernelKube) do(_ context.Context, method, path string, body, out any) error {
	switch {
	case method == "GET" && strings.Contains(path, "/pods?") && strings.Contains(path, "singleuser-server"):
		return json.Unmarshal([]byte(`{"items":[`+f.server+`]}`), out)
	case method == "GET":
		return json.Unmarshal([]byte(`{"items":[]}`), out)
	case method == "POST":
		f.created = append(f.created, body.(map[string]any))
	case method == "DELETE":
		f.deleted = append(f.deleted, path)
	}
	return nil
}

const testServerPod = `{"metadata":{"name":"jupyter-a-40dhbw-2ede","uid":"u1","labels":{"component":"singleuser-server","hub.jupyter.org/username":"a-40dhbw-2ede","dhbw.cloud/project":"ml","dhbw.cloud/profile":"cpu-gpukernel","dhbw.cloud/gpu-class":"general-small"},"annotations":{"hub.jupyter.org/username":"a@dhbw.de"}},
 "spec":{"containers":[{"name":"notebook","image":"reg/envs/ml:1","env":[{"name":"JUPYTERHUB_API_TOKEN","value":"secret"},{"name":"OPENAI_API_KEY","value":"k"}],
   "volumeMounts":[{"name":"home","mountPath":"/home/jovyan","subPath":"homes/a"},{"name":"home","mountPath":"/home/jovyan/project-shared","subPath":"project-shared","readOnly":true},{"name":"kube-api","mountPath":"/var/run/secrets"}]}],
  "volumes":[{"name":"home","persistentVolumeClaim":{"claimName":"ws-ml"}}]},
 "status":{"phase":"Running","podIP":"10.42.1.5"}}`

const testKernelID = "0123abcd-0000-4000-8000-0123456789ab"

func kernelTestService(server string) (*Service, *fakeKernelKube) {
	s, _, _, _ := testService()
	k := &fakeKernelKube{server: server}
	s.k8s = k
	s.whoami = func(_ context.Context, token string) (string, error) {
		if token == "t-a" {
			return "a@dhbw.de", nil
		}
		return "", ErrNotFound
	}
	return s, k
}

func TestKernelCaller(t *testing.T) {
	s, _ := kernelTestService(testServerPod)
	ctx := context.Background()
	o, err := s.KernelCaller(ctx, "10.42.1.5", "t-a")
	if err != nil || o.Pod != "jupyter-a-40dhbw-2ede" || o.Project != "ml" || o.Image != "reg/envs/ml:1" {
		t.Fatalf("owner = %+v, %v", o, err)
	}
	if _, err := s.KernelCaller(ctx, "10.42.1.6", "t-a"); !errors.Is(err, ErrNoKernelOwner) {
		t.Errorf("other address: %v", err)
	}
	if _, err := s.KernelCaller(ctx, "10.42.1.5", "t-b"); !errors.Is(err, ErrNoKernelOwner) {
		t.Errorf("unknown token: %v", err)
	}
	s.whoami = func(context.Context, string) (string, error) { return "b@dhbw.de", nil }
	s.tokens.hits = nil
	if _, err := s.KernelCaller(ctx, "10.42.1.5", "t-a"); !errors.Is(err, ErrNoKernelOwner) {
		t.Errorf("token of another person: %v", err)
	}
	cpu, _ := kernelTestService(strings.Replace(testServerPod, `"cpu-gpukernel"`, `"cpu"`, 1))
	if _, err := cpu.KernelCaller(ctx, "10.42.1.5", "t-a"); !errors.Is(err, ErrNoKernelOwner) {
		t.Errorf("server of another profile: %v", err)
	}
}

func TestStartKernel(t *testing.T) {
	s, k := kernelTestService(testServerPod)
	ctx := context.Background()
	o, _ := s.KernelOwnerAt(ctx, "10.42.1.5")
	checked := 0
	s.SetKernelCheck(func(context.Context, *KernelOwner) error { checked++; return nil })
	req := KernelRequest{KernelID: testKernelID, ResponseAddress: "10.42.1.5:8877", PublicKey: "abc", PortRange: "0..0", WorkingDir: "/home/jovyan/kurs/../kurs"}
	if _, err := s.StartKernel(ctx, o, req); err != nil || checked != 1 || len(k.created) != 1 {
		t.Fatalf("start: %v, checked %d, created %d", err, checked, len(k.created))
	}
	b, _ := json.Marshal(k.created[0])
	pod := string(b)
	for _, want := range []string{`"image":"reg/envs/ml:1"`, `"claimName":"ws-ml"`, `"subPath":"homes/a"`, `"mountPath":"/home/jovyan/project-shared","name":"home","readOnly":true`, `"OPENAI_API_KEY"`, `"workingDir":"/home/jovyan/kurs"`, `"kueue.x-k8s.io/queue-name":"gpu-general-small"`, `"nvidia.com/gpu":"1"`, `"dhbw.cloud/kernel-owner":"jupyter-a-40dhbw-2ede"`, `"uid":"u1"`, `"automountServiceAccountToken":false`, `"activeDeadlineSeconds":14400`} {
		if !strings.Contains(pod, want) {
			t.Errorf("kernel pod lacks %s: %s", want, pod)
		}
	}
	for _, unwanted := range []string{"JUPYTERHUB_API_TOKEN", "kube-api"} {
		if strings.Contains(pod, unwanted) {
			t.Errorf("kernel pod has %s", unwanted)
		}
	}
	// The environment is the launcher's parameters and the project's LLM key, nothing else of the server (todo kernel_rest): checked by name, so a new variable has to be added here on purpose.
	launcher := map[string]bool{"RESPONSE_ADDRESS": true, "PUBLIC_KEY": true, "PORT_RANGE": true, "TRANSPORT_ENCRYPTION": true}
	c := k.created[0]["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	if _, ok := c["envFrom"]; ok {
		t.Errorf("kernel container has envFrom: %v", c["envFrom"])
	}
	for _, e := range c["env"].([]any) {
		name := e.(map[string]any)["name"].(string)
		if !launcher[name] && !strings.HasPrefix(name, "KERNEL_") && !strings.HasPrefix(name, "OPENAI_") {
			t.Errorf("kernel pod has the variable %s", name)
		}
	}

	for name, bad := range map[string]KernelRequest{
		"response elsewhere": {KernelID: testKernelID, ResponseAddress: "10.42.9.9:8877"},
		"kernel id":          {KernelID: "x; rm", ResponseAddress: "10.42.1.5:8877"},
		"working dir":        {KernelID: testKernelID, ResponseAddress: "10.42.1.5:8877", WorkingDir: "/home/jovyan/../../etc"},
		"class name":         {KernelID: testKernelID, ResponseAddress: "10.42.1.5:8877", KernelClassName: "a b"},
	} {
		var refused *KernelRefused
		if _, err := s.StartKernel(ctx, o, bad); !errors.As(err, &refused) {
			t.Errorf("%s: %v", name, err)
		}
	}

	s.SetKernelCheck(func(context.Context, *KernelOwner) error { return &KernelRefused{"voll"} })
	if _, err := s.StartKernel(ctx, o, req); err == nil || err.Error() != "voll" || len(k.created) != 1 {
		t.Errorf("refused by the limits: %v", err)
	}

	if err := s.StopKernel(ctx, o, testKernelID, "stopped_server"); err != nil || len(k.deleted) != 1 || !strings.Contains(k.deleted[0], "dhbw.cloud%2Fkernel-owner%3Djupyter-a-40dhbw-2ede") {
		t.Errorf("stop deletes only this server's kernel: %v %v", err, k.deleted)
	}
}

func TestKernelMax(t *testing.T) {
	s, k := kernelTestService(testServerPod)
	o, _ := s.KernelOwnerAt(context.Background(), "10.42.1.5")
	req := KernelRequest{KernelID: testKernelID, ResponseAddress: "10.42.1.5:8877"}
	for _, c := range []struct {
		max  time.Duration
		want string
	}{{8 * time.Hour, `"activeDeadlineSeconds":28800`}, {0, ""}} {
		s.SetKernelMax(func(context.Context, *KernelOwner) time.Duration { return c.max })
		k.created = nil
		if _, err := s.StartKernel(context.Background(), o, req); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(k.created[0])
		if has := strings.Contains(string(b), "activeDeadlineSeconds"); has != (c.want != "") || !strings.Contains(string(b), c.want) {
			t.Errorf("max %v: %s", c.max, b)
		}
	}
	// The overview's end counts from the pod's start.
	var p map[string]any
	_ = json.Unmarshal([]byte(`{"metadata":{"name":"kernel-1","creationTimestamp":"2026-10-18T08:00:00Z"},"spec":{"activeDeadlineSeconds":3600},"status":{"phase":"Running","startTime":"2026-10-18T08:10:00Z"}}`), &p)
	if st := kernelState(p); st.EndsAt == nil || st.EndsAt.Format(time.RFC3339) != "2026-10-18T09:10:00Z" {
		t.Errorf("ends at: %v", st.EndsAt)
	}
}

func TestKernelWithoutGPU(t *testing.T) {
	s, k := kernelTestService(testServerPod)
	s.cfg.Kernels = KernelConfig{Queue: "workspaces-cpu", NoGPU: true}
	o, _ := s.KernelOwnerAt(context.Background(), "10.42.1.5")
	if _, err := s.StartKernel(context.Background(), o, KernelRequest{KernelID: testKernelID, ResponseAddress: "10.42.1.5:8877"}); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(k.created[0])
	if strings.Contains(string(b), "nvidia.com/gpu") || strings.Contains(string(b), "gpu-lab") || !strings.Contains(string(b), `"workspaces-cpu"`) {
		t.Errorf("staging kernel: %s", b)
	}
}

// A GPU kernel gets its project's local storage as ephemeral-storage limit, with a request of at most 1 GiB; without SetKernelScratch the default of 1 GB.
func TestKernelScratch(t *testing.T) {
	s, k := kernelTestService(testServerPod)
	o, _ := s.KernelOwnerAt(context.Background(), "10.42.1.5")
	req := KernelRequest{KernelID: testKernelID, ResponseAddress: "10.42.1.5:8877"}
	for _, c := range []struct {
		set            func(context.Context, *KernelOwner) int
		limit, request string
	}{
		{nil, "1Gi", "1Gi"},
		{func(context.Context, *KernelOwner) int { return 8 }, "8Gi", "1Gi"},
	} {
		if c.set != nil {
			s.SetKernelScratch(c.set)
		}
		k.created = nil
		if _, err := s.StartKernel(context.Background(), o, req); err != nil {
			t.Fatal(err)
		}
		res := k.created[0]["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["resources"].(map[string]any)
		if l := res["limits"].(map[string]any)["ephemeral-storage"]; l != c.limit {
			t.Errorf("limit = %v, want %s", l, c.limit)
		}
		if r := res["requests"].(map[string]any)["ephemeral-storage"]; r != c.request {
			t.Errorf("request = %v, want %s", r, c.request)
		}
	}
	// An evicted kernel tells why.
	var p map[string]any
	_ = json.Unmarshal([]byte(`{"metadata":{"name":"kernel-1"},"status":{"phase":"Failed","reason":"Evicted","message":"Pod ephemeral local storage usage exceeds the total limit of containers 1Gi."}}`), &p)
	if st := kernelState(p); st.Phase != "failed" || st.Reason != "Evicted" || !strings.Contains(st.Message, "ephemeral local storage") {
		t.Errorf("evicted: %+v", st)
	}
}

// The environment builds keep their own 40 GiB: the workspaces' limit is not theirs.
func TestBuildJobKeepsItsEphemeralStorage(t *testing.T) {
	b, _ := json.Marshal(buildJob(Config{}, "b", "builder", Environment{}, "ml", "amd64", "reg/envs/ml:1", "", false))
	if !strings.Contains(string(b), `"ephemeral-storage":"40Gi"`) {
		t.Errorf("build job: %s", b)
	}
}
