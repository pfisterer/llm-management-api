package gpu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/pfisterer/llm-management-api/internal/httpx"
)

// hub talks to JupyterHub's REST API with the token of the hub service
// gpu-management-api (scopes admin:servers, admin:users, read:users).
type hub struct {
	base  string // https://jupyter.gpu.services.dhbw.cloud
	token string
	http  *http.Client
}

func newHub(base, token string) *hub {
	return &hub{base: base, token: token, http: httpx.WithRetry(&http.Client{Timeout: 30 * time.Second})}
}

func (h *hub) do(ctx context.Context, method, path string, body, out any) (int, error) {
	status, err := httpx.Do(ctx, h.http, httpx.Request{Method: method, URL: h.base + "/hub/api" + path, Header: http.Header{"Authorization": {"token " + h.token}}, Body: body}, out)
	var se *httpx.StatusError
	switch {
	case errors.As(err, &se):
		var e struct {
			Message string `json:"message"`
		}
		if json.Unmarshal([]byte(se.Body), &e) == nil && e.Message != "" {
			return status, &HubError{Status: status, Message: e.Message}
		}
		return status, &HubError{Status: status, Message: se.Body}
	case err != nil && status == 0:
		return 0, fmt.Errorf("jupyterhub: %w", err)
	}
	return status, err
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

// whoami is the JupyterHub user a token belongs to (any token may identify itself at /hub/api/user), e.g. the API token of a person's server.
func (h *hub) whoami(ctx context.Context, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.base+"/hub/api/user", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "token "+token)
	resp, err := h.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("jupyterhub: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", ErrNotFound
	}
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("jupyterhub: /user %d", resp.StatusCode)
	}
	var u struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil || u.Name == "" {
		return "", ErrNotFound
	}
	return u.Name, nil
}
