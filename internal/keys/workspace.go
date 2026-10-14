package keys

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
)

// WorkspaceKey is the LiteLLM key a person's workspace servers in one project use (VS Code's chat, OPENAI_API_KEY in notebooks). Derived like the chat key (HMAC of user id and project under the master key), so the hub gets the same key on every start and nothing is stored; a key deleted in the key list comes back on the next start.
func WorkspaceKey(masterKey, userID, project string) string {
	mac := hmac.New(sha256.New, []byte(masterKey))
	mac.Write([]byte("workspace:" + userID + ":" + project))
	return "sk-ws-" + hex.EncodeToString(mac.Sum(nil))[:48]
}

// workspaceAlias is the name the workspace key of a project has in the person's key list.
func workspaceAlias(project string) string { return "workspace-" + project }

// EnsureWorkspaceKey makes sure the person exists in LiteLLM with their quota and has the workspace key of the project in a pool, with the pool's key budget, and returns it. The pool is the project's own; a project without LLM tokens (a workspace project, the LLM in a project of its own, docs/bauplan.md 8.14) draws from the person's default project. ErrNotEligible when the person has no pool at all.
func (s *Service) EnsureWorkspaceKey(ctx context.Context, p Person, g Grant, masterKey, project string) (string, error) {
	if len(g.Pools) == 0 {
		return "", ErrNotEligible
	}
	uid := p.UserID()
	key := WorkspaceKey(masterKey, uid, project)
	info, _, err := s.reconcile(ctx, p, g)
	if err != nil {
		return "", err
	}
	pool, ok := g.pool(project)
	if !ok {
		if pool, ok = g.defaultPool(info); !ok {
			return "", ErrNotEligible
		}
	}
	cur, found, err := s.lite.KeyInfo(ctx, key)
	if err != nil {
		return "", err
	}
	if found {
		if cur.Team() != pool.Slug {
			if err := s.moveKey(ctx, uid, key, pool); err != nil {
				return "", fmt.Errorf("move workspace key: %w", err)
			}
		}
		cur.Token = key
		if _, err := s.applyKeyBudget(ctx, cur, pool); err != nil {
			return "", fmt.Errorf("workspace key budget: %w", err)
		}
		return key, nil
	}
	if _, err := s.ensureTeam(ctx, pool); err != nil {
		return "", err
	}
	meta := map[string]any{"created_by": "llm-management-api", "purpose": "workspace", "project": project}
	if err := s.lite.GenerateKeyWithValue(ctx, uid, pool.Slug, ownAlias(uid, workspaceAlias(project)), key, s.keyBudget(pool), meta); err != nil {
		return "", err
	}
	return key, nil
}

// ModelsOf lists the models a key may use, as the gateway's /v1/models answers it, without embedding models (no use in a chat), sorted.
func ModelsOf(ctx context.Context, gateway, key string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(gateway, "/")+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gateway /v1/models: %d", resp.StatusCode)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
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
