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
	Token       string `json:"token"`
	DisplayName string `json:"display_name,omitempty"`
}

// Provider is what the rest of the service needs from the role-provider.
type Provider interface {
	// UserTokens never fails: on any error it degrades to "user:<email>", so
	// rules written for the person still apply and group rules do not — fewer
	// rights, never more.
	UserTokens(ctx context.Context, email string) []string
	SearchGroups(ctx context.Context, query string, limit int) ([]Group, error)
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
		if g.DisplayName != nil {
			hit.DisplayName = *g.DisplayName
		}
		out = append(out, hit)
	}
	return out, nil
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
}

func (m *Mock) UserTokens(_ context.Context, email string) []string {
	email = strings.ToLower(strings.TrimSpace(email))
	return withUserToken(append([]string{}, m.Memberships[email]...), email)
}

func (m *Mock) SearchGroups(_ context.Context, query string, limit int) ([]Group, error) {
	var out []Group
	for _, g := range m.Groups {
		if strings.Contains(g.Token, strings.ToLower(query)) || strings.Contains(strings.ToLower(g.DisplayName), strings.ToLower(query)) {
			out = append(out, g)
		}
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}
