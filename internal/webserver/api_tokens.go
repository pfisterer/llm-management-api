package webserver

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/cloud-self-service-golib/token"
	"github.com/pfisterer/llm-management-api/internal/access"
)

// APITokenPrefix tells this service's API tokens apart from an OIDC bearer token and from the other services' tokens (dynz_token_, os_mgt_), and lets a secret scanner find a leaked one. Not "sk-": that is what the LLM keys (LiteLLM) look like, and the two must not be confused — an API token manages things here, an LLM key calls models on the OpenAI-compatible API.
const APITokenPrefix = "aiaas_"

// TokenOptions is what the API tokens need: the service from cloud-self-service-golib, the lifetime policy and the cap per person.
type TokenOptions struct {
	// Service issues and checks the tokens; nil = no API tokens (no routes, and a bearer with the prefix is just an invalid OIDC token).
	Service *token.Service
	// TTL is the lifetime policy: the default when a request names none, the maximum and whether "no expiry" may be asked for. The table is the library's, the values the deployment's.
	TTL token.TTLPolicy
	// MaxPerUser is how many valid tokens one person may hold.
	MaxPerUser int
}

// apiTokens is the server's part of the API tokens.
type apiTokens struct {
	TokenOptions
	// issue serialises counting and issuing, so two parallel requests cannot both pass the cap with one token left. One replica runs this service; with more, the cap could be overshot by the number of replicas, which is harmless.
	issue sync.Mutex
}

// APIToken is the API shape of an API token, the same as dynamic-zones and openstack-management-api answer. Not the stored row: the columns are the shared library's to change, these field names are published.
type APIToken struct {
	ID     uint   `json:"id" example:"1"`
	User   string `json:"user" example:"alice@dhbw.de"`
	Prefix string `json:"token_prefix" example:"aiaas_ab12cd34"`
	// TokenString is set only in the response that creates a token. It is not stored and cannot be recovered afterwards.
	TokenString string `json:"token_string,omitempty" example:"aiaas_ab12cd34…"`
	// ExpiresAt is null for a token that does not expire (a pointer, so that "no expiry" is null and not the year 1).
	ExpiresAt *time.Time `json:"expires_at" example:"2027-10-19T12:00:00Z"`
	ReadOnly  bool       `json:"read_only" example:"false"`
	CreatedAt time.Time  `json:"created_at" example:"2026-10-19T12:00:00Z"`
	// Description is what the owner wrote down about this token. A memory aid, never a permission.
	Description string `json:"description" example:"nightly GPU report"`
	// LastUsedAt is when the token last authenticated a request, to the nearest minute, or null if it never has — which is what makes revoking a forgotten token safe.
	LastUsedAt *time.Time `json:"last_used_at" example:"2026-10-20T08:30:00Z"`
}

// APITokensResponse is the list returned by GET /v1/tokens.
type APITokensResponse struct {
	Tokens []APIToken `json:"tokens"`
}

// CreateAPITokenRequest is the body of POST /v1/tokens.
type CreateAPITokenRequest struct {
	// ReadOnly limits the token to GET requests.
	ReadOnly bool `json:"read_only" example:"false"`
	// Description is the owner's note about the token, at most 100 characters.
	Description string `json:"description" example:"nightly GPU report"`
	// TTLHours is how long the token should live. Omitted or 0 means the configured default; -1 means it never expires: allowed for read-only tokens and for admins, or for everyone where the deployment allows it.
	TTLHours int `json:"ttl_hours" example:"720"`
}

// nilIfZero maps Go's zero time to a JSON null: no expiry, never used.
func nilIfZero(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func toAPIToken(rec token.Record) APIToken {
	return APIToken{
		ID:          rec.ID,
		User:        rec.Subject,
		Prefix:      rec.Prefix,
		ExpiresAt:   nilIfZero(rec.ExpiresAt),
		ReadOnly:    rec.ReadOnly,
		CreatedAt:   rec.CreatedAt,
		Description: rec.Description,
		LastUsedAt:  nilIfZero(rec.LastUsedAt),
	}
}

// tokenSubject is the identity a token belongs to: the person the request acts as, the e-mail address everything else here is keyed on (the same string as authn.Claims.Identity()). An admin acting on somebody's behalf (X-Act-As) lists and revokes THAT person's tokens — cleaning them up for a ticket is one of the reasons to act on someone's behalf. Issuing one is not: see createAPIToken.
func tokenSubject(c *gin.Context) string { return caller(c).Email }

// listAPITokens godoc
//
//	@ID			listApiTokens
//	@Summary		The caller's API tokens
//	@Description	API tokens authenticate scripts, CI jobs and notebooks against this management API (/v1/…) as the person who created them. They are not LLM keys: those (/v1/keys) call models on the OpenAI-compatible API. Expired tokens are not listed.
//	@Tags			tokens
//	@Produce		json
//	@Security		BearerAuth
//	@Success		200	{object}	APITokensResponse
//	@Failure		401	{object}	ErrorResponse
//	@Router			/v1/tokens [get]
func (s *Server) listAPITokens(c *gin.Context) {
	subject := tokenSubject(c)
	records, err := s.tokens.Service.List(c.Request.Context(), subject)
	if err != nil {
		s.internalError(c, "list api tokens", err)
		return
	}
	out := make([]APIToken, 0, len(records))
	for _, rec := range records {
		out = append(out, toAPIToken(rec))
	}
	c.JSON(http.StatusOK, APITokensResponse{Tokens: out})
}

// createAPIToken godoc
//
//	@ID			createApiToken
//	@Summary		Create an API token
//	@Description	Issues an API token for the signed-in caller, sent as "Authorization: Bearer aiaas_…". It acts as its owner with the owner's rights at the time of each request (an admin's token acts as admin), a read-only token only for GET. The secret is returned exactly once; only its hash is stored. Not possible with another API token or while acting on behalf of someone else. The lifetime comes from ttl_hours, or the configured default when it is omitted; -1 asks for a token that never expires: allowed for read-only tokens and for admins (or for everyone where the deployment allows it).
//	@Tags			tokens
//	@Accept			json
//	@Produce		json
//	@Security		BearerAuth
//	@Param			token	body		CreateAPITokenRequest	false	"Token options"
//	@Success		201		{object}	APIToken
//	@Failure		400		{object}	ErrorResponse
//	@Failure		403		{object}	ErrorResponse	"Acting on behalf of someone else, or authenticated with an API token."
//	@Failure		409		{object}	ErrorResponse	"The caller already holds the maximum number of tokens."
//	@Router			/v1/tokens [post]
func (s *Server) createAPIToken(c *gin.Context) {
	cl := caller(c)
	// A token issued while acting on behalf would hand the admin a credential that IS the person, outliving the session and the act-as rule that allowed it; and the person would find a token they never made.
	if cl.Actor != "" {
		abort(c, http.StatusForbidden, "token_on_behalf", "API-Tokens lassen sich nicht im Auftrag anderer Personen erstellen; die Person muss ihr Token selbst erstellen.")
		return
	}
	// A token that mints tokens would let a leaked one renew itself forever, past every expiry.
	if cl.ViaAPIToken {
		abort(c, http.StatusForbidden, "token_via_token", "API-Tokens lassen sich nur nach einer Anmeldung erstellen, nicht mit einem anderen API-Token.")
		return
	}
	var req CreateAPITokenRequest
	// An empty body is the defaults.
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			abort(c, http.StatusBadRequest, "invalid_request", "Ungültige Anfrage.")
			return
		}
	}
	// No expiry for a read-only token (it can change nothing) and for an admin's (for long-running admin automation), besides where the deployment allows it for everyone (API_TOKEN_ALLOW_NEVER_EXPIRES). Everyone else's tokens always expire: a token in a public repository must not grant access for good.
	policy := s.tokens.TTL
	policy.AllowNever = policy.AllowNever || req.ReadOnly || cl.Role.AtLeast(access.RoleAdmin)
	ttl, err := policy.Resolve(req.TTLHours)
	if err != nil {
		abort(c, http.StatusBadRequest, "invalid_ttl", ttlMessage(err, policy))
		return
	}

	subject := tokenSubject(c)
	s.tokens.issue.Lock()
	defer s.tokens.issue.Unlock()
	existing, err := s.tokens.Service.List(c.Request.Context(), subject)
	if err != nil {
		s.internalError(c, "list api tokens", err)
		return
	}
	if len(existing) >= s.tokens.MaxPerUser {
		abort(c, http.StatusConflict, "token_limit", fmt.Sprintf("Höchstens %d API-Tokens pro Person; zuerst ein altes löschen.", s.tokens.MaxPerUser))
		return
	}
	issued, err := s.tokens.Service.Issue(c.Request.Context(), subject, token.IssueOptions{TTL: ttl, ReadOnly: req.ReadOnly, Description: req.Description})
	// The description is the caller's text: too long is a 400, and it is never echoed into the log.
	if errors.Is(err, token.ErrDescriptionTooLong) {
		abort(c, http.StatusBadRequest, "invalid_description", fmt.Sprintf("Die Beschreibung darf höchstens %d Zeichen lang sein.", token.MaxDescriptionLen))
		return
	}
	if err != nil {
		s.internalError(c, "issue api token", err)
		return
	}
	s.log.Infow("api token created", "subject", subject, "id", issued.ID, "read_only", issued.ReadOnly, "expires_at", issued.ExpiresAt)
	out := toAPIToken(issued.Record)
	out.TokenString = issued.Secret
	c.JSON(http.StatusCreated, out)
}

// ttlMessage says which lifetime limit a request broke, in the language of the other error texts.
func ttlMessage(err error, p token.TTLPolicy) string {
	switch {
	case errors.Is(err, token.ErrNeverExpiresNotAllowed):
		return "API-Tokens ohne Ablaufdatum gibt es nur lesend oder für Admins."
	case errors.Is(err, token.ErrTTLTooLong):
		return fmt.Sprintf("Die Laufzeit darf höchstens %d Stunden betragen.", int(p.Max/time.Hour))
	default:
		return "Ungültige Laufzeit: ttl_hours muss positiv sein, 0 für die Voreinstellung."
	}
}

// deleteAPIToken godoc
//
//	@ID			deleteApiToken
//	@Summary		Revoke an API token
//	@Description	Deletes one of the caller's API tokens; every client still using it is refused from then on.
//	@Tags			tokens
//	@Produce		json
//	@Security		BearerAuth
//	@Param			id	path	int	true	"ID of the token to revoke"
//	@Success		204
//	@Failure		400	{object}	ErrorResponse
//	@Failure		404	{object}	ErrorResponse
//	@Router			/v1/tokens/{id} [delete]
func (s *Server) deleteAPIToken(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		abort(c, http.StatusBadRequest, "invalid_request", "Ungültige Token-ID.")
		return
	}
	subject := tokenSubject(c)
	err = s.tokens.Service.Revoke(c.Request.Context(), subject, uint(id))
	switch {
	case errors.Is(err, token.ErrNotFound):
		// Not found and not yours give the same answer, so the response cannot be used to discover which IDs exist.
		abort(c, http.StatusNotFound, "not_found", "Token nicht gefunden.")
	case err != nil:
		s.internalError(c, "revoke api token", err)
	default:
		s.log.Infow("api token revoked", "subject", subject, "id", id, "actor", caller(c).Actor)
		c.Status(http.StatusNoContent)
	}
}
