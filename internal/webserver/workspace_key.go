package webserver

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/pfisterer/llm-management-api/internal/cluster/gpu"
	"github.com/pfisterer/llm-management-api/internal/cluster/workspace"
	"github.com/pfisterer/llm-management-api/internal/llm/keys"
)

// keySource makes the LLM key of a person's workspaces and batch jobs in a project (keys.WorkspaceKey): the keys, the person's LLM pools, and the master key the keys are derived from.
type keySource struct {
	Keys      *keys.Service
	Quota     LLMQuota
	MasterKey string
}

// keyFailure is a key that could not be found out (LiteLLM or the projects not reachable): an error status, not an answer.
type keyFailure struct {
	status    int
	code, msg string
	err       error
}

// workspaceKey is the person's key for the workspaces and batch jobs of proj, whose LLM tokens it draws from chosen by llmProjectFor: the choice (a slug, gpu.NoLLM, or empty), the project's setting default_llm_project, the project's own LLM tokens, the person's first LLM project. The caller has checked that the person is a member of proj. A response with an empty Key and a Reason means definitely no key; a keyFailure that it could not be found out. sub is the person's Keycloak sub when known; without it the person is looked up by e-mail.
func (k keySource) workspaceKey(ctx context.Context, email, sub string, tokens []string, proj workspace.Project, chosen string) (WorkspaceKeyResponse, *keyFailure) {
	chosen = strings.TrimSpace(chosen)
	// Without LLM, chosen or set for the project: definitely no key, without asking LiteLLM.
	if chosen == gpu.NoLLM || (chosen == "" && proj.DefaultLLMProject == workspace.NoLLM) {
		reason := "started without LLM"
		if chosen == "" {
			reason = "the project's workspaces run without LLM"
		}
		return WorkspaceKeyResponse{Reason: reason}, nil
	}
	if sub = strings.TrimSpace(sub); sub == "" {
		found, ok, err := k.Keys.SubjectOf(ctx, email)
		if err != nil {
			return WorkspaceKeyResponse{}, &keyFailure{http.StatusBadGateway, "upstream", "LiteLLM nicht erreichbar.", err}
		}
		if !ok {
			return WorkspaceKeyResponse{Reason: "person unknown to the LLM service"}, nil
		}
		sub = found
	}
	g, status, msg := k.Quota.GrantFor(ctx, tokens)
	if status >= 500 {
		return WorkspaceKeyResponse{}, &keyFailure{http.StatusServiceUnavailable, "upstream", msg, errors.New(msg)}
	}
	if status != 0 {
		return WorkspaceKeyResponse{Reason: msg}, nil
	}
	var llm []HubLLMProject
	for _, p := range g.Pools {
		if p.MTokens > 0 {
			llm = append(llm, HubLLMProject{Slug: p.Slug, Name: p.Name})
		}
	}
	pick, ok := llmProjectFor(llm, chosen, proj.Slug, proj.DefaultLLMProject)
	target := pick.Project
	switch {
	case !ok:
		return WorkspaceKeyResponse{Reason: "the chosen LLM project is none of the person's projects with LLM tokens"}, nil
	case pick.Unavailable:
		return WorkspaceKeyResponse{Reason: "the project's LLM project for workspaces (" + proj.DefaultLLMProject + ") is none of the person's projects with LLM tokens"}, nil
	case pick.Source == LLMPreset && target == "":
		return WorkspaceKeyResponse{Reason: "the project's workspaces run without LLM"}, nil
	case target == "":
		return WorkspaceKeyResponse{Reason: "no project with LLM tokens"}, nil
	}
	key, err := k.Keys.EnsureWorkspaceKey(ctx, keys.Person{Subject: sub, Email: email}, g, k.MasterKey, target)
	if errors.Is(err, keys.ErrNotEligible) {
		return WorkspaceKeyResponse{Reason: "project has no LLM tokens"}, nil
	}
	if err != nil {
		return WorkspaceKeyResponse{}, &keyFailure{http.StatusBadGateway, "upstream", "Key konnte nicht angelegt werden.", err}
	}
	return WorkspaceKeyResponse{Key: key, LLMProject: target}, nil
}
