package keys

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/pfisterer/cloud-self-service-golib/httpx"
)

// WorkspaceKey is the LiteLLM key a person's workspace servers in one project use (VS Code's chat, OPENAI_API_KEY in notebooks). Derived like the chat key (HMAC of user id and project under the master key), so the hub gets the same key on every start and nothing is stored; a key deleted in the key list comes back on the next start.
func WorkspaceKey(masterKey, userID, project string) string {
	return derivedKey(masterKey, "sk-ws-", "workspace:"+userID+":"+project)
}

// workspaceAlias is the name the workspace key of a project has in the person's key list.
func workspaceAlias(project string) string { return "workspace-" + project }

// EnsureWorkspaceKey makes sure the person exists in LiteLLM with their quota and has the workspace key of the project in the project's own pool, with the pool's key budget, and returns it. Only the project's own pool: a project without LLM tokens (or one the person is no member of) gets no key, ErrNotEligible, and a workspace key left from before (when such projects drew from the person's default project) is deleted, so a running notebook's copy stops working too.
func (s *Service) EnsureWorkspaceKey(ctx context.Context, p Person, g Grant, masterKey, project string) (string, error) {
	uid := p.UserID()
	key := WorkspaceKey(masterKey, uid, project)
	pool, ok := g.pool(project)
	if !ok {
		if _, found, err := s.lite.KeyInfo(ctx, key); err != nil {
			return "", err
		} else if found {
			if err := s.lite.DeleteKeys(ctx, key); err != nil {
				return "", fmt.Errorf("remove workspace key without pool: %w", err)
			}
		}
		return "", ErrNotEligible
	}
	if _, _, err := s.reconcile(ctx, p, g); err != nil {
		return "", err
	}
	if found, err := s.keepDerivedKey(ctx, uid, key, pool, "workspace"); err != nil {
		return "", err
	} else if found {
		return key, nil
	}
	meta := map[string]any{"created_by": "llm-management-api", "purpose": "workspace", "project": project}
	if err := s.createDerivedKey(ctx, uid, key, pool, workspaceAlias(project), meta); err != nil {
		return "", err
	}
	return key, nil
}

// modelsClient asks the gateway for a key's models; ModelsOf limits each call to 10 s as well.
var modelsClient = httpx.NewClient(15 * time.Second)

// ModelsOf lists the models a key may use, as the gateway's /v1/models answers it, without embedding models (no use in a chat), sorted.
func ModelsOf(ctx context.Context, gateway, key string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	status, err := httpx.Do(ctx, modelsClient, httpx.Request{Method: http.MethodGet, URL: strings.TrimRight(gateway, "/") + "/v1/models", Header: http.Header{"Authorization": {"Bearer " + key}}}, &body)
	if se, ok := errors.AsType[*httpx.StatusError](err); ok {
		return nil, fmt.Errorf("gateway /v1/models: %d", se.Status)
	}
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("gateway /v1/models: %d", status)
	}
	out := []string{}
	for _, m := range body.Data {
		if m.ID != "" && !strings.Contains(strings.ToLower(m.ID), "embed") {
			out = append(out, m.ID)
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}
