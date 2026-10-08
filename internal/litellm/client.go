// Package litellm talks to the LiteLLM management API (the "backend" service,
// not the inference gateway) with the master key.
package litellm

import (
	"bytes"
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

// Client is a minimal JSON client for the endpoints this service needs.
type Client struct {
	base string
	key  string
	http *http.Client
}

func New(baseURL, masterKey string, timeout time.Duration) *Client {
	return &Client{base: strings.TrimRight(baseURL, "/"), key: masterKey, http: &http.Client{Timeout: timeout}}
}

// Error is a non-2xx answer from LiteLLM.
type Error struct {
	Status int
	Body   string
}

func (e *Error) Error() string { return fmt.Sprintf("litellm: HTTP %d: %s", e.Status, e.Body) }

// IsStatus reports whether err is a LiteLLM answer with the given status.
func IsStatus(err error, status int) bool {
	e, ok := err.(*Error)
	return ok && e.Status == status
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("litellm: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := bytes.TrimSpace(raw)
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return &Error{Status: resp.StatusCode, Body: string(msg)}
	}
	if out != nil && len(bytes.TrimSpace(raw)) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// UserInfo is the part of /user/info this service reads.
type UserInfo struct {
	UserID         string         `json:"user_id"`
	Email          string         `json:"user_email"`
	Spend          float64        `json:"spend"`
	MaxBudget      *float64       `json:"max_budget"`
	BudgetDuration *string        `json:"budget_duration"`
	BudgetResetAt  *string        `json:"budget_reset_at"`
	RPMLimit       *int           `json:"rpm_limit"`
	TPMLimit       *int           `json:"tpm_limit"`
	Models         []string       `json:"models"`
	Metadata       map[string]any `json:"metadata"`
}

// Key is the part of a key record this service reads.
type Key struct {
	Token          string   `json:"token"`
	Alias          *string  `json:"key_alias"`
	Spend          float64  `json:"spend"`
	MaxBudget      *float64 `json:"max_budget"`
	BudgetDuration *string  `json:"budget_duration"`
	BudgetResetAt  *string  `json:"budget_reset_at"`
}

type userInfoResponse struct {
	UserInfo *UserInfo `json:"user_info"`
	Keys     []Key     `json:"keys"`
}

// GetUser returns the user and their keys; found=false if LiteLLM does not know them.
func (c *Client) GetUser(ctx context.Context, userID string) (UserInfo, []Key, bool, error) {
	var r userInfoResponse
	err := c.do(ctx, http.MethodGet, "/user/info?user_id="+url.QueryEscape(userID), nil, &r)
	if IsStatus(err, http.StatusNotFound) {
		return UserInfo{}, nil, false, nil
	}
	if err != nil {
		return UserInfo{}, nil, false, err
	}
	if r.UserInfo == nil || r.UserInfo.UserID == "" {
		return UserInfo{}, r.Keys, false, nil
	}
	return *r.UserInfo, r.Keys, true, nil
}

// Quota is what a tier writes onto a user or key.
type Quota struct {
	MaxBudget      float64  `json:"max_budget"`
	BudgetDuration string   `json:"budget_duration,omitempty"`
	TPMLimit       int      `json:"tpm_limit,omitempty"`
	RPMLimit       int      `json:"rpm_limit,omitempty"`
	Models         []string `json:"models,omitempty"`
}

// NewUser creates a user with quota and metadata.
func (c *Client) NewUser(ctx context.Context, userID, email string, q Quota, metadata map[string]any) error {
	body := map[string]any{"user_id": userID, "user_email": email, "user_role": "internal_user", "metadata": metadata}
	mergeQuota(body, q)
	return c.do(ctx, http.MethodPost, "/user/new", body, nil)
}

// UpdateUser sets email and metadata, and the quota when q is not nil. Spend
// is never touched.
func (c *Client) UpdateUser(ctx context.Context, userID, email string, q *Quota, metadata map[string]any) error {
	body := map[string]any{"user_id": userID, "user_email": email, "metadata": metadata}
	if q != nil {
		mergeQuota(body, *q)
	}
	return c.do(ctx, http.MethodPost, "/user/update", body, nil)
}

// GenerateKey creates a key and returns its secret value (shown exactly once).
func (c *Client) GenerateKey(ctx context.Context, userID, alias string, q Quota, metadata map[string]any) (string, error) {
	body := map[string]any{"user_id": userID, "key_alias": alias, "metadata": metadata}
	mergeQuota(body, q)
	var r struct {
		Key string `json:"key"`
	}
	if err := c.do(ctx, http.MethodPost, "/key/generate", body, &r); err != nil {
		return "", err
	}
	if r.Key == "" {
		return "", fmt.Errorf("litellm: /key/generate returned no key")
	}
	return r.Key, nil
}

// GenerateKeyWithValue creates a key whose secret the caller chose. Used for the
// chat key, which is derived from the person's id and never stored.
func (c *Client) GenerateKeyWithValue(ctx context.Context, userID, alias, key string, q Quota, metadata map[string]any) error {
	body := map[string]any{"user_id": userID, "key_alias": alias, "key": key, "metadata": metadata}
	mergeQuota(body, q)
	return c.do(ctx, http.MethodPost, "/key/generate", body, nil)
}

// KeyExists reports whether LiteLLM knows the key (by its secret value). Any
// answer other than success counts as "no": the caller then creates it, and a
// real problem shows up there with a proper error.
func (c *Client) KeyExists(ctx context.Context, key string) (bool, error) {
	err := c.do(ctx, http.MethodGet, "/key/info?key="+url.QueryEscape(key), nil, nil)
	if err == nil {
		return true, nil
	}
	var le *Error
	if errors.As(err, &le) {
		return false, nil
	}
	return false, err
}

// DeleteKeys deletes keys by token (hash).
func (c *Client) DeleteKeys(ctx context.Context, tokens ...string) error {
	return c.do(ctx, http.MethodPost, "/key/delete", map[string]any{"keys": tokens}, nil)
}

func mergeQuota(body map[string]any, q Quota) {
	body["max_budget"] = q.MaxBudget
	if q.BudgetDuration != "" {
		body["budget_duration"] = q.BudgetDuration
	}
	if q.TPMLimit > 0 {
		body["tpm_limit"] = q.TPMLimit
	}
	if q.RPMLimit > 0 {
		body["rpm_limit"] = q.RPMLimit
	}
	if len(q.Models) > 0 {
		body["models"] = q.Models
	}
}

// HealthEndpoint is one entry of LiteLLM's /health answer.
type HealthEndpoint struct {
	APIBase string `json:"api_base"`
	Model   string `json:"model"`
	Error   string `json:"error"`
}

// Health returns LiteLLM's cached background health check (cheap: with
// background_health_checks the proxy answers from its cache and does NOT run
// inference — that coupling is why this may be called per page view).
func (c *Client) Health(ctx context.Context) (healthy, unhealthy []HealthEndpoint, err error) {
	var r struct {
		Healthy   []HealthEndpoint `json:"healthy_endpoints"`
		Unhealthy []HealthEndpoint `json:"unhealthy_endpoints"`
	}
	if err := c.do(ctx, http.MethodGet, "/health", nil, &r); err != nil {
		return nil, nil, err
	}
	return r.Healthy, r.Unhealthy, nil
}
