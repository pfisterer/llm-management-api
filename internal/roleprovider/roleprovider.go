// Package roleprovider asks the role-provider-service which tokens a person
// holds ("user:a@dhbw.de", "group:wwi23seb", …) and searches its groups for the
// access-list editor.
package roleprovider

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	roleclient "github.com/pfisterer/llm-management-api/internal/roleprovider/api"
	"go.uber.org/zap"
)

// Group is one search hit for the access-list editor.
type Group struct {
	Token string `json:"token"`
	// Display name, left out when it only repeats the group ID (imported groups).
	Label       string `json:"label,omitempty"`
	Description string `json:"description,omitempty"`
}

// Provider is what the rest of the service needs from the role-provider.
type Provider interface {
	// UserTokens never fails: on any error it degrades to "user:<email>", so
	// rules written for the person still apply and group rules do not — fewer
	// rights, never more.
	UserTokens(ctx context.Context, email string) []string
	SearchGroups(ctx context.Context, query string, limit int) ([]Group, error)
	// SearchUsers matches the email address only — the role-provider stores
	// no names, so people cannot be looked up by name.
	SearchUsers(ctx context.Context, query string, limit int) ([]string, error)
}

// ---------------------------------------------------------------- http

type HTTP struct {
	client *roleclient.ClientWithResponses
	log    *zap.SugaredLogger
}

func NewHTTP(baseURL, apiToken string, timeout time.Duration, log *zap.SugaredLogger) (*HTTP, error) {
	httpClient := &http.Client{Timeout: timeout}
	auth := func(_ context.Context, req *http.Request) error {
		req.Header.Set("Authorization", "Bearer "+apiToken)
		return nil
	}
	c, err := roleclient.NewClientWithResponses(baseURL,
		roleclient.WithHTTPClient(httpClient), roleclient.WithRequestEditorFn(auth))
	if err != nil {
		return nil, fmt.Errorf("role-provider client: %w", err)
	}
	return &HTTP{client: c, log: log}, nil
}

func (h *HTTP) UserTokens(ctx context.Context, email string) []string {
	email = strings.ToLower(strings.TrimSpace(email))
	fallback := []string{"user:" + email}
	resp, err := h.client.GetUserTokensWithResponse(ctx, email)
	if err != nil {
		h.log.Warnw("role-provider unreachable, using user token only", "email", email, zap.Error(err))
		return fallback
	}
	if resp.JSON200 == nil {
		h.log.Warnw("role-provider: unexpected status, using user token only", "email", email, "status", resp.StatusCode())
		return fallback
	}
	return withUserToken(*resp.JSON200, email)
}

func (h *HTTP) SearchGroups(ctx context.Context, query string, limit int) ([]Group, error) {
	resp, err := h.client.ListGroupsWithResponse(ctx, &roleclient.ListGroupsParams{Q: &query, Limit: &limit})
	if err != nil {
		return nil, fmt.Errorf("role-provider group search: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, fmt.Errorf("role-provider group search: status %d", resp.StatusCode())
	}
	out := make([]Group, 0, len(*resp.JSON200))
	for _, g := range *resp.JSON200 {
		if g.Token == nil {
			continue
		}
		hit := Group{Token: *g.Token}
		// Imported groups get display_name = ID; dropped so the UI does not
		// print the token twice (same as openstack-management-api).
		if g.DisplayName != nil && *g.DisplayName != strings.TrimPrefix(*g.Token, "group:") {
			hit.Label = *g.DisplayName
		}
		if g.Description != nil {
			hit.Description = *g.Description
		}
		out = append(out, hit)
	}
	return out, nil
}

func (h *HTTP) SearchUsers(ctx context.Context, query string, limit int) ([]string, error) {
	resp, err := h.client.SearchUsersWithResponse(ctx, &roleclient.SearchUsersParams{Q: &query, Limit: &limit})
	if err != nil {
		return nil, fmt.Errorf("role-provider user search: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, fmt.Errorf("role-provider user search: status %d", resp.StatusCode())
	}
	return *resp.JSON200, nil
}

// withUserToken makes sure the person's own token is present, whatever the
// role-provider returned.
func withUserToken(tokens []string, email string) []string {
	own := "user:" + email
	for _, t := range tokens {
		if strings.EqualFold(t, own) {
			return tokens
		}
	}
	return append(tokens, own)
}

// ---------------------------------------------------------------- mock

// Mock serves fixed memberships for local development and tests.
type Mock struct {
	Memberships map[string][]string // email -> group tokens
	Groups      []Group
	Users       []string
}

func (m *Mock) UserTokens(_ context.Context, email string) []string {
	email = strings.ToLower(strings.TrimSpace(email))
	return withUserToken(append([]string{}, m.Memberships[email]...), email)
}

func (m *Mock) SearchGroups(_ context.Context, query string, limit int) ([]Group, error) {
	var out []Group
	for _, g := range m.Groups {
		q := strings.ToLower(query)
		if strings.Contains(g.Token, q) || strings.Contains(strings.ToLower(g.Label), q) || strings.Contains(strings.ToLower(g.Description), q) {
			out = append(out, g)
		}
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (m *Mock) SearchUsers(_ context.Context, query string, limit int) ([]string, error) {
	var out []string
	for _, u := range m.Users {
		if strings.Contains(u, strings.ToLower(query)) {
			out = append(out, u)
		}
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}
