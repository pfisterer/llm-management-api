package gpu

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/pfisterer/cloud-self-service-golib/httpx"
	"github.com/pfisterer/cloud-self-service-golib/msg"
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
	h := http.Header{"Authorization": {"Bearer " + k.token}}
	if method == http.MethodPatch {
		h.Set("Content-Type", "application/merge-patch+json")
	}
	var limit int64
	if _, ok := out.(*string); ok {
		limit = 1 << 20 // a log
	}
	status, err := httpx.Do(ctx, k.http, httpx.Request{Method: method, URL: k.base + path, Header: h, Body: body, MaxBody: limit}, out)
	var se *httpx.StatusError
	switch {
	case errors.As(err, &se) && se.Status == http.StatusNotFound:
		return ErrNotFound
	case se != nil && se.Status == http.StatusConflict:
		return errAlreadyExists
	case se != nil:
		return fmt.Errorf("kubernetes %s %s: %d %s", method, path, se.Status, se.Body)
	case err != nil && status == 0:
		return fmt.Errorf("kubernetes %s %s: %w", method, path, err)
	}
	return err
}

var errAlreadyExists = errors.New("already exists")

// jobStatus is the part of a Job's status the service reads.
type jobStatus struct {
	Active         int        `json:"active"`
	Succeeded      int        `json:"succeeded"`
	Failed         int        `json:"failed"`
	CompletionTime *time.Time `json:"completionTime"`
	Conditions     []struct {
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
		Name                       string            `json:"name"`
		UID                        string            `json:"uid"`
		Labels                     map[string]string `json:"labels"`
		Annotations                map[string]string `json:"annotations"`
		CreationTimestamp          time.Time         `json:"creationTimestamp"`
		DeletionTimestamp          *time.Time        `json:"deletionTimestamp"`
		DeletionGracePeriodSeconds int64             `json:"deletionGracePeriodSeconds"`
		OwnerReferences            []struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
			UID  string `json:"uid"`
		} `json:"ownerReferences"`
	} `json:"metadata"`
	Spec struct {
		ActiveDeadlineSeconds int64             `json:"activeDeadlineSeconds"`
		Priority              int32             `json:"priority"`
		PriorityClassName     string            `json:"priorityClassName"`
		NodeName              string            `json:"nodeName"`
		NodeSelector          map[string]string `json:"nodeSelector"`
		SchedulingGates       []struct {
			Name string `json:"name"`
		} `json:"schedulingGates"`
		Containers []struct {
			Name string   `json:"name"`
			Args []string `json:"args"`
			Env  []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"env"`
			Resources struct {
				Requests map[string]string `json:"requests"`
				Limits   map[string]string `json:"limits"`
			} `json:"resources"`
		} `json:"containers"`
	} `json:"spec"`
	Status struct {
		Phase             string     `json:"phase"`
		Reason            string     `json:"reason"`
		StartTime         *time.Time `json:"startTime"`
		ContainerStatuses []struct {
			RestartCount int `json:"restartCount"`
			State        struct {
				Waiting *struct {
					Reason string `json:"reason"`
				} `json:"waiting"`
				Running *struct {
					StartedAt time.Time `json:"startedAt"`
				} `json:"running"`
				Terminated *struct {
					Reason     string    `json:"reason"`
					StartedAt  time.Time `json:"startedAt"`
					FinishedAt time.Time `json:"finishedAt"`
				} `json:"terminated"`
			} `json:"state"`
			LastState struct {
				Terminated *struct {
					Reason string `json:"reason"`
				} `json:"terminated"`
			} `json:"lastState"`
		} `json:"containerStatuses"`
		Conditions []struct {
			Type               string    `json:"type"`
			Status             string    `json:"status"`
			Reason             string    `json:"reason"`
			LastTransitionTime time.Time `json:"lastTransitionTime"`
		} `json:"conditions"`
		InitContainerStatuses []struct {
			Name  string `json:"name"`
			State struct {
				Terminated *struct {
					Message string `json:"message"`
				} `json:"terminated"`
			} `json:"state"`
		} `json:"initContainerStatuses"`
	} `json:"status"`
}

func (k *kube) listPods(ctx context.Context, ns, selector string) ([]podInfo, error) {
	var pods struct {
		Items []podInfo `json:"items"`
	}
	err := k.do(ctx, http.MethodGet, "/api/v1/namespaces/"+ns+"/pods?labelSelector="+url.QueryEscape(selector), nil, &pods)
	return pods.Items, err
}

// podMetrics is what metrics-server reports for a pod.
type podMetrics struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Containers []struct {
		Usage map[string]string `json:"usage"`
	} `json:"containers"`
}

func (k *kube) podMetrics(ctx context.Context, ns string) ([]podMetrics, error) {
	var m struct {
		Items []podMetrics `json:"items"`
	}
	err := k.do(ctx, http.MethodGet, "/apis/metrics.k8s.io/v1beta1/namespaces/"+ns+"/pods", nil, &m)
	return m.Items, err
}

// jobLog returns the last lines of the job's (newest) pod.
// buildPod is what jobState reads of a build's pod.
type buildPod struct {
	Metadata struct {
		CreationTimestamp time.Time `json:"creationTimestamp"`
	} `json:"metadata"`
	Status struct {
		Phase      string     `json:"phase"`
		StartTime  *time.Time `json:"startTime"`
		Conditions []struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"conditions"`
		ContainerStatuses []containerStatus `json:"containerStatuses"`
		InitStatuses      []containerStatus `json:"initContainerStatuses"`
	} `json:"status"`
}

type containerStatus struct {
	Name  string `json:"name"`
	State struct {
		Waiting *struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"waiting"`
		Running *struct {
			StartedAt time.Time `json:"startedAt"`
		} `json:"running"`
		Terminated *struct {
			ExitCode int    `json:"exitCode"`
			Reason   string `json:"reason"`
		} `json:"terminated"`
	} `json:"state"`
}

// jobState says in a sentence what the newest pod of a build job is doing, for the people waiting for the image (German and English, with Kubernetes' own words as they are), and since when it runs.
func (k *kube) jobState(ctx context.Context, ns, job string) (msg.Text, *time.Time, error) {
	var pods struct {
		Items []buildPod `json:"items"`
	}
	if err := k.do(ctx, http.MethodGet, "/api/v1/namespaces/"+ns+"/pods?labelSelector="+url.QueryEscape("job-name="+job), nil, &pods); err != nil {
		return msg.Text{}, nil, err
	}
	if len(pods.Items) == 0 {
		return msg.T("Der Bau ist angelegt, sein Pod noch nicht.", "The build is created, its pod not yet."), nil, nil
	}
	p := pods.Items[0]
	for _, q := range pods.Items[1:] {
		if q.Metadata.CreationTimestamp.After(p.Metadata.CreationTimestamp) {
			p = q
		}
	}
	return buildPodState(p), p.Status.StartTime, nil
}

// buildPodState puts a build pod's status into words: waiting for a node, preparing or pulling, running, ended.
func buildPodState(p buildPod) msg.Text {
	for _, c := range p.Status.Conditions {
		if c.Type == "PodScheduled" && c.Status == "False" {
			return msg.F("Wartet auf einen Build-Knoten: %s", "Waiting for a build node: %s", c.Message)
		}
	}
	for _, c := range append(append([]containerStatus{}, p.Status.InitStatuses...), p.Status.ContainerStatuses...) {
		if w := c.State.Waiting; w != nil && w.Reason != "" && w.Reason != "PodInitializing" {
			switch w.Reason {
			case "ContainerCreating":
				return msg.T("Der Build-Container wird vorbereitet (Image des Builders wird geladen).", "The build container is being prepared (the builder's image is being pulled).")
			case "ErrImagePull", "ImagePullBackOff":
				return msg.F("Das Image des Builders lässt sich nicht laden: %s", "The builder's image cannot be pulled: %s", w.Message)
			}
			return msg.F("Wartet (%s): %s", "Waiting (%s): %s", w.Reason, w.Message)
		}
	}
	switch p.Status.Phase {
	case "Pending":
		return msg.T("Der Build-Pod startet.", "The build pod is starting.")
	case "Running":
		return msg.T("Der Bau läuft.", "The build is running.")
	case "Succeeded":
		return msg.T("Der Bau ist fertig.", "The build is done.")
	case "Failed":
		for _, c := range p.Status.ContainerStatuses {
			if t := c.State.Terminated; t != nil {
				return msg.F("Der Bau ist gescheitert (Exit-Code %d, %s).", "The build failed (exit code %d, %s).", t.ExitCode, t.Reason)
			}
		}
		return msg.T("Der Bau ist gescheitert.", "The build failed.")
	}
	return msg.Raw(p.Status.Phase)
}

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
