package webserver

import (
	"context"
	"net/http"
	"slices"

	"github.com/pfisterer/llm-management-api/internal/keys"
	"github.com/pfisterer/llm-management-api/internal/workspace"
)

// LLMQuota decides what a person may use of the LLM, entirely from the budget tree: every project with LLM tokens is one pool shared by its members — no project no access —, with the project's requests and tokens per minute and models for all its members together.
type LLMQuota struct {
	// DefaultRPM and DefaultTPM apply to a project for which the tree sets none (0 = no limit).
	DefaultRPM int
	DefaultTPM int
	// BaseModels are the model groups every project may use; the tree adds more per project.
	BaseModels []string
	Projects   *workspace.Service
	// SelfServiceURL is where a person without LLM tokens gets a project.
	SelfServiceURL string
}

// GrantFor is the person's pools; a status and message when they may not use the LLM (projects not readable, no project with tokens).
func (q LLMQuota) GrantFor(ctx context.Context, tokens []string) (keys.Grant, int, string) {
	pools, status, msg := q.PoolsFor(ctx, tokens)
	if status != 0 {
		return keys.Grant{}, status, msg
	}
	if len(pools) == 0 {
		msg := "Kein Projekt mit LLM-Tokens."
		if q.SelfServiceURL != "" {
			msg += " Ein Projekt gibt es unter " + q.SelfServiceURL
		}
		return keys.Grant{}, http.StatusForbidden, msg
	}
	return keys.Grant{Pools: pools}, 0, ""
}

// PoolsFor lists the pools the person may draw from (see workspace.LLMPoolsFor) with their effective limits; a status and message when the projects cannot be read.
func (q LLMQuota) PoolsFor(ctx context.Context, tokens []string) ([]keys.Pool, int, string) {
	if q.Projects == nil {
		return nil, http.StatusServiceUnavailable, "Projekte sind hier nicht verfügbar."
	}
	list, err := q.Projects.LLMPoolsFor(ctx, tokens)
	if err != nil {
		return nil, http.StatusServiceUnavailable, "Projekte konnten gerade nicht gelesen werden."
	}
	return q.toPools(list), 0, ""
}

// toPools gives every project its effective limits: its own rate limits or the defaults, and the base models plus its own, sorted and without duplicates.
func (q LLMQuota) toPools(list []workspace.LLMPool) []keys.Pool {
	out := make([]keys.Pool, 0, len(list))
	for _, p := range list {
		pool := keys.Pool{Slug: p.Slug, Name: p.Name, MTokens: p.MTokens, RPM: p.RPM, TPM: p.TPM}
		if pool.RPM <= 0 {
			pool.RPM = q.DefaultRPM
		}
		if pool.TPM <= 0 {
			pool.TPM = q.DefaultTPM
		}
		models := append(slices.Clone(q.BaseModels), p.Models...)
		slices.Sort(models)
		pool.Models = slices.Compact(models)
		if pool.Models == nil {
			pool.Models = []string{}
		}
		out = append(out, pool)
	}
	return out
}
