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
	// TeamID is the team whose budget the key spends; nil for a personal key.
	TeamID   *string  `json:"team_id"`
	RPMLimit *int     `json:"rpm_limit"`
	TPMLimit *int     `json:"tpm_limit"`
	Models   []string `json:"models"`
}

// Team returns the key's team id, "" for a personal key.
func (k Key) Team() string {
	if k.TeamID == nil {
		return ""
	}
	return *k.TeamID
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

// ListUsers returns every user LiteLLM knows, page by page.
func (c *Client) ListUsers(ctx context.Context) ([]UserInfo, error) {
	var out []UserInfo
	for page := 1; ; page++ {
		var r struct {
			Users      []UserInfo `json:"users"`
			TotalPages int        `json:"total_pages"`
		}
		if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/user/list?page=%d&page_size=100", page), nil, &r); err != nil {
			return nil, err
		}
		out = append(out, r.Users...)
		if page >= r.TotalPages || len(r.Users) == 0 {
			return out, nil
		}
	}
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
	// Without auto_create_key LiteLLM makes a nameless key for every new user, whose secret nobody keeps and which counts as one of the person's keys.
	body := map[string]any{"user_id": userID, "user_email": email, "user_role": "internal_user", "metadata": metadata, "auto_create_key": false}
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

// GenerateKey creates a key in a team and returns its secret value (shown exactly once).
func (c *Client) GenerateKey(ctx context.Context, userID, teamID, alias string, q Quota, metadata map[string]any) (string, error) {
	body := map[string]any{"user_id": userID, "team_id": teamID, "key_alias": alias, "metadata": metadata}
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
func (c *Client) GenerateKeyWithValue(ctx context.Context, userID, teamID, alias, key string, q Quota, metadata map[string]any) error {
	body := map[string]any{"user_id": userID, "team_id": teamID, "key_alias": alias, "key": key, "metadata": metadata}
	mergeQuota(body, q)
	return c.do(ctx, http.MethodPost, "/key/generate", body, nil)
}

// KeyInfo returns the key LiteLLM knows by its secret value. Any answer other than success counts as "not found": the caller then creates it, and a real problem shows up there with a proper error.
func (c *Client) KeyInfo(ctx context.Context, key string) (Key, bool, error) {
	var r struct {
		Info *Key `json:"info"`
	}
	err := c.do(ctx, http.MethodGet, "/key/info?key="+url.QueryEscape(key), nil, &r)
	var le *Error
	if errors.As(err, &le) {
		return Key{}, false, nil
	}
	if err != nil {
		return Key{}, false, err
	}
	if r.Info == nil {
		return Key{}, true, nil
	}
	return *r.Info, true, nil
}

// SetKeyTeam moves a key (by token or secret) into a team, or with teamID "" out of every team: LiteLLM 1.94 writes an explicit null (UpdateKeyRequest is dumped with exclude_unset, team_id is a plain nullable column) and checks nothing for it, while a move INTO a team requires the key's user to be a member of that team (validate_key_team_change, even for the master key).
func (c *Client) SetKeyTeam(ctx context.Context, token, teamID string) error {
	body := map[string]any{"key": token, "team_id": nil}
	if teamID != "" {
		body["team_id"] = teamID
	}
	return c.do(ctx, http.MethodPost, "/key/update", body, nil)
}

// SetKeyQuota writes a key's short-term budget, rate limits and models. The budget's window is only sent when it changes: LiteLLM starts a new window with every write that carries one, which would reset what the key spent so far.
func (c *Client) SetKeyQuota(ctx context.Context, token string, q Quota, windowChanged bool) error {
	body := map[string]any{"key": token}
	mergeQuota(body, q)
	if !windowChanged {
		delete(body, "budget_duration")
	}
	return c.do(ctx, http.MethodPost, "/key/update", body, nil)
}

// DeleteKeys deletes keys by token (hash).
func (c *Client) DeleteKeys(ctx context.Context, tokens ...string) error {
	return c.do(ctx, http.MethodPost, "/key/delete", map[string]any{"keys": tokens}, nil)
}

// Team is the part of a LiteLLM team this service reads.
type Team struct {
	TeamID         string         `json:"team_id"`
	Alias          *string        `json:"team_alias"`
	Spend          float64        `json:"spend"`
	MaxBudget      *float64       `json:"max_budget"`
	BudgetDuration *string        `json:"budget_duration"`
	BudgetResetAt  *string        `json:"budget_reset_at"`
	Metadata       map[string]any `json:"metadata"`
	Members        []TeamMember   `json:"members_with_roles"`
}

// TeamMember is one entry of a team's members_with_roles.
type TeamMember struct {
	UserID string `json:"user_id"`
	Role   string `json:"role"`
}

// HasMember reports whether the user is a member of the team.
func (t Team) HasMember(userID string) bool {
	for _, m := range t.Members {
		if m.UserID == userID {
			return true
		}
	}
	return false
}

// GetTeam returns a team; found=false when LiteLLM does not know it. /team/info answers {"team_id", "team_info": {...}, "keys", "team_memberships"}.
func (c *Client) GetTeam(ctx context.Context, teamID string) (Team, bool, error) {
	var r struct {
		TeamInfo *Team `json:"team_info"`
	}
	err := c.do(ctx, http.MethodGet, "/team/info?team_id="+url.QueryEscape(teamID), nil, &r)
	if IsStatus(err, http.StatusNotFound) {
		return Team{}, false, nil
	}
	if err != nil {
		return Team{}, false, err
	}
	if r.TeamInfo == nil || r.TeamInfo.TeamID == "" {
		return Team{}, false, nil
	}
	return *r.TeamInfo, true, nil
}

// ListTeams returns every team. /team/list answers a bare array in 1.94; an object with "teams" (the paginated /v2 shape) is read as well, so a change of shape does not read as "no teams".
func (c *Client) ListTeams(ctx context.Context) ([]Team, error) {
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodGet, "/team/list", nil, &raw); err != nil {
		return nil, err
	}
	var list []Team
	if err := json.Unmarshal(raw, &list); err == nil {
		return list, nil
	}
	var wrapped struct {
		Teams *[]Team `json:"teams"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil || wrapped.Teams == nil {
		return nil, fmt.Errorf("litellm: /team/list answered neither a list nor {teams: [...]}")
	}
	return *wrapped.Teams, nil
}

// TeamSpec is what this service sets on a team. BudgetDuration "" leaves the team's window (and its reset date) alone on an update: LiteLLM restarts the window whenever budget_duration is sent.
type TeamSpec struct {
	TeamID         string
	Alias          string
	MaxBudget      float64
	BudgetDuration string
	Metadata       map[string]any
}

func (t TeamSpec) body() map[string]any {
	body := map[string]any{"team_id": t.TeamID, "team_alias": t.Alias, "max_budget": t.MaxBudget, "metadata": t.Metadata}
	if t.BudgetDuration != "" {
		body["budget_duration"] = t.BudgetDuration
	}
	return body
}

// NewTeam creates a team without members and without a model list (all models; the keys carry the models).
func (c *Client) NewTeam(ctx context.Context, t TeamSpec) error {
	return c.do(ctx, http.MethodPost, "/team/new", t.body(), nil)
}

// UpdateTeam sets alias, budget and metadata of a team (and its window when BudgetDuration is set). Spend is never touched.
func (c *Client) UpdateTeam(ctx context.Context, t TeamSpec) error {
	return c.do(ctx, http.MethodPost, "/team/update", t.body(), nil)
}

// AddTeamMember adds the user to the team as a plain member; already being one is fine.
func (c *Client) AddTeamMember(ctx context.Context, teamID, userID string) error {
	err := c.do(ctx, http.MethodPost, "/team/member_add", map[string]any{"team_id": teamID, "member": map[string]any{"role": "user", "user_id": userID}}, nil)
	var le *Error
	if errors.As(err, &le) && le.Status == http.StatusBadRequest && strings.Contains(le.Body, "already in team") {
		return nil
	}
	return err
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
