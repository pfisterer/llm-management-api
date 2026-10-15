package gpu

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// progressWindow is how long one look at a start's progress reads JupyterHub's event stream: the hub replays every event so far at once, so a short window gets them all, and the page asks again a few seconds later.
var progressWindow = 2 * time.Second

// ProgressEvent is one step of a start as JupyterHub reports it (KubeSpawner passes the pod's Kubernetes events: waiting in the queue, pulling the image, started).
type ProgressEvent struct {
	Progress int    `json:"progress"`
	Message  string `json:"message"`
}

// Progress is how far a server's start got: the events so far, and whether it ended ready or failed (Message then says why). Starting is false when the server is not starting (anymore) and the hub has nothing to report.
type Progress struct {
	Starting bool            `json:"starting"`
	Ready    bool            `json:"ready"`
	Failed   bool            `json:"failed"`
	Message  string          `json:"message,omitempty"`
	Events   []ProgressEvent `json:"events"`
}

type hubEvent struct {
	Progress int    `json:"progress"`
	Message  string `json:"message"`
	Ready    bool   `json:"ready"`
	Failed   bool   `json:"failed"`
}

// progress reads the server's progress stream (/hub/api/users/<user>/servers/<server>/progress, server-sent events) for at most progressWindow. A server that is not starting gives an empty Progress; a start that failed gives Failed with the hub's reason, also after the end.
func (h *hub) progress(ctx context.Context, user, server string) (Progress, error) {
	out := Progress{Events: []ProgressEvent{}}
	ctx, cancel := context.WithTimeout(ctx, progressWindow)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.base+"/hub/api"+userPath(user)+"/servers/"+url.PathEscape(server)+"/progress", nil)
	if err != nil {
		return out, err
	}
	req.Header.Set("Authorization", "token "+h.token)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := h.http.Do(req)
	if err != nil {
		return out, fmt.Errorf("jupyterhub: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return out, ErrNotFound
	case resp.StatusCode == http.StatusBadRequest:
		// "… is not starting": nothing to report.
		return out, nil
	case resp.StatusCode >= 400:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return out, &HubError{Status: resp.StatusCode, Message: string(msg)}
	}
	out.Starting = true
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue
		}
		var e hubEvent
		if json.Unmarshal([]byte(strings.TrimSpace(data)), &e) != nil {
			continue
		}
		if e.Ready || e.Failed {
			out.Starting, out.Ready, out.Failed, out.Message = false, e.Ready, e.Failed, e.Message
			if e.Failed {
				return out, nil
			}
		}
		out.Events = append(out.Events, ProgressEvent{Progress: e.Progress, Message: e.Message})
		if e.Ready {
			return out, nil
		}
	}
	// The window ended while the start goes on: what came so far is the answer.
	if err := sc.Err(); err != nil && !errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		return out, fmt.Errorf("jupyterhub progress: %w", err)
	}
	return out, nil
}
