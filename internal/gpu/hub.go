package gpu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// hub talks to JupyterHub's REST API with the token of the hub service
// gpu-management-api (scopes admin:servers, admin:users, read:users).
type hub struct {
	base  string // https://jupyter.gpu.services.dhbw.cloud
	token string
	http  *http.Client
}

func newHub(base, token string) *hub {
	return &hub{base: base, token: token, http: withDNSRetry(&http.Client{Timeout: 30 * time.Second})}
}

func (h *hub) do(ctx context.Context, method, path string, body, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.base+"/hub/api"+path, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "token "+h.token)
	resp, err := h.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("jupyterhub: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		var e struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(msg, &e) == nil && e.Message != "" {
			return resp.StatusCode, &HubError{Status: resp.StatusCode, Message: e.Message}
		}
		return resp.StatusCode, &HubError{Status: resp.StatusCode, Message: string(msg)}
	}
	if out != nil {
		return resp.StatusCode, json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode, nil
}

// HubError carries JupyterHub's message, e.g. a refused start (GPU quota).
type HubError struct {
	Status  int
	Message string
}

func (e *HubError) Error() string { return fmt.Sprintf("jupyterhub %d: %s", e.Status, e.Message) }

// HubServer is one (named) server of a person.
type HubServer struct {
	Name        string         `json:"name"`
	Ready       bool           `json:"ready"`
	Pending     string         `json:"pending"`
	URL         string         `json:"url"`
	Started     *time.Time     `json:"started"`
	LastActive  *time.Time     `json:"last_activity"`
	UserOptions map[string]any `json:"user_options"`
}

type hubUser struct {
	Name    string               `json:"name"`
	Servers map[string]HubServer `json:"servers"`
}

func userPath(name string) string { return "/users/" + url.PathEscape(name) }

func (h *hub) user(ctx context.Context, name string) (*hubUser, error) {
	var u hubUser
	status, err := h.do(ctx, http.MethodGet, userPath(name), nil, &u)
	if status == http.StatusNotFound {
		return nil, ErrNotFound
	}
	return &u, err
}

// ensureUser creates the person in JupyterHub if they never logged in there.
func (h *hub) ensureUser(ctx context.Context, name string) error {
	_, err := h.user(ctx, name)
	if errors.Is(err, ErrNotFound) {
		_, err = h.do(ctx, http.MethodPost, userPath(name), nil, nil)
	}
	return err
}

func (h *hub) startServer(ctx context.Context, user, server string, options map[string]any) error {
	_, err := h.do(ctx, http.MethodPost, userPath(user)+"/servers/"+url.PathEscape(server), options, nil)
	return err
}

func (h *hub) stopServer(ctx context.Context, user, server string, remove bool) error {
	status, err := h.do(ctx, http.MethodDelete, userPath(user)+"/servers/"+url.PathEscape(server), map[string]bool{"remove": remove}, nil)
	if status == http.StatusNotFound {
		return ErrNotFound
	}
	return err
}
