package gpu

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// ErrNotFound is returned by the clients for a missing object.
var ErrNotFound = errors.New("not found")

// kube is a minimal client for the GPU cluster's Kubernetes API: only what the
// build jobs need, with the ServiceAccount token of this service.
type kube struct {
	base  string
	token string
	http  *http.Client
}

func newKube(base, token, caPEM string) (*kube, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(caPEM)) {
		return nil, errors.New("GPU_KUBE_CA contains no certificate")
	}
	return &kube{base: base, token: token, http: &http.Client{Timeout: 20 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}}, nil
}

func (k *kube) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, k.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+k.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := k.http.Do(req)
	if err != nil {
		return fmt.Errorf("kubernetes %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode == http.StatusConflict {
		return errAlreadyExists
	}
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("kubernetes %s %s: %d %s", method, path, resp.StatusCode, msg)
	}
	if out == nil {
		return nil
	}
	if s, ok := out.(*string); ok {
		b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		*s = string(b)
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

var errAlreadyExists = errors.New("already exists")

// jobStatus is the part of a Job's status the service reads.
type jobStatus struct {
	Active     int `json:"active"`
	Succeeded  int `json:"succeeded"`
	Failed     int `json:"failed"`
	Conditions []struct {
		Type    string `json:"type"`
		Status  string `json:"status"`
		Reason  string `json:"reason"`
		Message string `json:"message"`
	} `json:"conditions"`
}

func (k *kube) createJob(ctx context.Context, ns string, job map[string]any) error {
	return k.do(ctx, http.MethodPost, "/apis/batch/v1/namespaces/"+ns+"/jobs", job, nil)
}

func (k *kube) jobStatus(ctx context.Context, ns, name string) (jobStatus, error) {
	var j struct {
		Status jobStatus `json:"status"`
	}
	err := k.do(ctx, http.MethodGet, "/apis/batch/v1/namespaces/"+ns+"/jobs/"+name, nil, &j)
	return j.Status, err
}

func (k *kube) deleteJob(ctx context.Context, ns, name string) error {
	err := k.do(ctx, http.MethodDelete, "/apis/batch/v1/namespaces/"+ns+"/jobs/"+name+"?propagationPolicy=Background", nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// podInfo is the part of a pod the inference listing reads.
type podInfo struct {
	Metadata struct {
		Name              string            `json:"name"`
		Labels            map[string]string `json:"labels"`
		CreationTimestamp time.Time         `json:"creationTimestamp"`
		DeletionTimestamp *time.Time        `json:"deletionTimestamp"`
	} `json:"metadata"`
	Spec struct {
		NodeName        string `json:"nodeName"`
		SchedulingGates []struct {
			Name string `json:"name"`
		} `json:"schedulingGates"`
		Containers []struct {
			Args []string `json:"args"`
		} `json:"containers"`
	} `json:"spec"`
	Status struct {
		StartTime  *time.Time `json:"startTime"`
		Conditions []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
	} `json:"status"`
}

func (k *kube) listPods(ctx context.Context, ns, selector string) ([]podInfo, error) {
	var pods struct {
		Items []podInfo `json:"items"`
	}
	err := k.do(ctx, http.MethodGet, "/api/v1/namespaces/"+ns+"/pods?labelSelector="+url.QueryEscape(selector), nil, &pods)
	return pods.Items, err
}

// jobLog returns the last lines of the job's (newest) pod.
func (k *kube) jobLog(ctx context.Context, ns, job string, tail int) (string, error) {
	var pods struct {
		Items []struct {
			Metadata struct {
				Name              string    `json:"name"`
				CreationTimestamp time.Time `json:"creationTimestamp"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := k.do(ctx, http.MethodGet, "/api/v1/namespaces/"+ns+"/pods?labelSelector="+url.QueryEscape("job-name="+job), nil, &pods); err != nil {
		return "", err
	}
	if len(pods.Items) == 0 {
		return "", ErrNotFound
	}
	newest := pods.Items[0]
	for _, p := range pods.Items[1:] {
		if p.Metadata.CreationTimestamp.After(newest.Metadata.CreationTimestamp) {
			newest = p
		}
	}
	var log string
	err := k.do(ctx, http.MethodGet, fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/log?tailLines=%d", ns, newest.Metadata.Name, tail), nil, &log)
	return log, err
}
