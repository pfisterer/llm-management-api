package webserver

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/cloud-self-service-golib/authn"
	"github.com/pfisterer/cloud-self-service-golib/ginweb"
	"github.com/pfisterer/cloud-self-service-golib/oidcauth"
	"github.com/pfisterer/cloud-self-service-golib/token"
	"github.com/pfisterer/llm-management-api/internal/access"
	"github.com/pfisterer/llm-management-api/internal/keys"
)

// Identity is who is calling, after authentication.
type Identity struct {
	Email string
	// Subject is the Keycloak sub; empty for an API token, which knows only the e-mail address (keyPerson looks the sub up where keys need it).
	Subject string
	// ViaAPIToken: the caller authenticated with an API token (aiaas_…) rather than by signing in, which this API holds against acting on behalf of somebody and issuing further tokens. TokenID names the token, for the request log.
	ViaAPIToken bool
	TokenID     uint
}

// Caller is the identity plus their role and role-provider tokens. While an admin acts on somebody's behalf (X-Act-As), Identity and Person are that somebody's — the admin sees and does what they would — and Actor names the admin.
type Caller struct {
	Identity
	access.Person
	// Actor is the signed-in admin acting on behalf of Identity; empty when the caller acts as themself.
	Actor string
	// MayActAs: the signed-in person may act on behalf of others (an admin by ADMIN_TOKENS).
	MayActAs bool
}

const callerKey = "llm.caller"

// authenticate turns the request into an Identity or aborts with 401/503.
//
// Production: the bearer is the ID token the self-service BFF forwards
// (audience = its client id), verified against Keycloak's keys.
// API token: a bearer with the prefix aiaas_ is looked up in the token store and is its owner, by e-mail address; what the owner may do is resolved per request like for a sign-in (withCaller), so a token never holds more than its owner has now.
// Development: a request without a bearer is the dev user (DEV_AUTH_USER), nothing is verified; other people are reached with X-Act-As, as in production.
//
// Every successful path records whether the credential is read-only (ginweb.SetReadOnly): RejectWritesForReadOnlyTokens refuses a write on a request where nobody did.
func (s *Server) authenticate(c *gin.Context) (Identity, bool) {
	if s.devMode && c.GetHeader("Authorization") == "" {
		ginweb.SetReadOnly(c, false)
		return Identity{Email: s.devUser, Subject: "dev-" + s.devUser}, true
	}
	raw, ok := authn.CutBearerPrefix(c.GetHeader("Authorization"))
	if ok && s.tokens != nil && s.tokens.Service.Owns(raw) {
		return s.authenticateToken(c, raw)
	}
	if !ok || s.verifier == nil {
		abort(c, http.StatusUnauthorized, "unauthenticated", "Anmeldung erforderlich.")
		return Identity{}, false
	}
	claims, err := s.verifier.Verify(c.Request.Context(), raw)
	if err != nil {
		if errors.Is(err, oidcauth.ErrKeysUnavailable) {
			abort(c, http.StatusServiceUnavailable, "auth_unavailable", "Anmeldedienst gerade nicht erreichbar.")
			return Identity{}, false
		}
		s.log.Infow("token rejected", "error", err)
		abort(c, http.StatusUnauthorized, "unauthenticated", "Anmeldung ungültig oder abgelaufen.")
		return Identity{}, false
	}
	email := strings.ToLower(strings.TrimSpace(claims.Email))
	if email == "" || claims.Subject == "" {
		abort(c, http.StatusUnauthorized, "unauthenticated", "Das Token enthält keine E-Mail-Adresse.")
		return Identity{}, false
	}
	ginweb.SetReadOnly(c, false)
	return Identity{Email: email, Subject: claims.Subject}, true
}

// authenticateToken resolves an API token. Expired, revoked and unknown tokens are one answer (the library makes an expired token absent), so the response does not tell which.
func (s *Server) authenticateToken(c *gin.Context, raw string) (Identity, bool) {
	rec, err := s.tokens.Service.Lookup(c.Request.Context(), raw)
	if errors.Is(err, token.ErrNotFound) {
		s.log.Infow("api token rejected")
		abort(c, http.StatusUnauthorized, "unauthenticated", "API-Token ungültig, abgelaufen oder gelöscht.")
		return Identity{}, false
	}
	if err != nil {
		s.log.Errorw("api token lookup failed", "error", err)
		abort(c, http.StatusInternalServerError, "internal", "Interner Fehler.")
		return Identity{}, false
	}
	ginweb.SetReadOnly(c, rec.ReadOnly)
	return Identity{Email: strings.ToLower(strings.TrimSpace(rec.Subject)), ViaAPIToken: true, TokenID: rec.ID}, true
}

// withCaller authenticates and resolves the caller's tokens and role, and the person an admin acts for (X-Act-As). It does NOT require any role beyond being signed in: /me must answer people without projects too.
func (s *Server) withCaller(c *gin.Context) {
	id, ok := s.authenticate(c)
	if !ok {
		return
	}
	self := s.access.Who(c.Request.Context(), id.Email)
	// Acting on behalf is for people signed in: a leaked admin token must not become every person at once. Refused rather than ignored, like a forbidden X-Act-As.
	if id.ViaAPIToken && c.GetHeader(authn.ActAsHeader) != "" {
		abort(c, http.StatusForbidden, "act_as_forbidden", "Handeln im Auftrag anderer Personen ist nur nach einer Anmeldung möglich, nicht mit einem API-Token.")
		return
	}
	may := self.Role.AtLeast(access.RoleAdmin) && !id.ViaAPIToken
	target, err := authn.ActAs(c.GetHeader(authn.ActAsHeader), id.Email, may)
	switch {
	case errors.Is(err, authn.ErrActAsInvalid):
		abort(c, http.StatusBadRequest, "act_as_invalid", "X-Act-As muss eine E-Mail-Adresse sein.")
		return
	case err != nil:
		abort(c, http.StatusForbidden, "act_as_forbidden", "Handeln im Auftrag anderer Personen ist nur für Administratoren möglich.")
		return
	}
	if target == id.Email {
		c.Set(callerKey, Caller{Identity: id, Person: self, MayActAs: may})
		c.Next()
		return
	}
	// The sub is looked up only where keys need it (keyPerson): most requests do not.
	if authn.IsWrite(c.Request.Method) {
		s.log.Infow("acting on behalf", "actor", id.Email, "as", target, "method", c.Request.Method, "path", c.Request.URL.Path)
	}
	c.Set(callerKey, Caller{Identity: Identity{Email: target}, Person: s.access.Who(c.Request.Context(), target), Actor: id.Email, MayActAs: may})
	c.Next()
}

// keyPerson is whom the caller's keys belong to. Acting on somebody's behalf or with an API token (which knows only the e-mail address), their Keycloak sub comes from LiteLLM; a person who has never used the LLM themself has none yet, and keys cannot be bound to them (development falls back to the dev sub, so the seed's people can be tried out).
func (s *Server) keyPerson(c *gin.Context) (keys.Person, bool) {
	cl := caller(c)
	if cl.Actor == "" && cl.Subject != "" {
		return keys.Person{Subject: cl.Subject, Email: cl.Email}, true
	}
	sub, found, err := s.keys.SubjectOf(c.Request.Context(), cl.Email)
	if err != nil {
		s.upstreamError(c, "subject lookup", err)
		return keys.Person{}, false
	}
	if !found {
		if !s.devMode {
			msg := "Diese Person hat den KI-Dienst noch nie selbst genutzt. In ihrem Auftrag sind Keys erst möglich, nachdem sie sich einmal angemeldet und die LLM-Seite geöffnet hat."
			if cl.Actor == "" {
				msg = "Mit einem API-Token sind LLM-Keys erst möglich, nachdem sich die Person einmal angemeldet und die LLM-Seite geöffnet hat."
			}
			abort(c, http.StatusConflict, "person_unknown", msg)
			return keys.Person{}, false
		}
		sub = "dev-" + cl.Email
	}
	return keys.Person{Subject: sub, Email: cl.Email}, true
}

// require aborts unless the caller holds at least the given role. The UI hides
// what a role may not use; this is what actually enforces it.
func require(min access.Role) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !caller(c).Role.AtLeast(min) {
			abort(c, http.StatusForbidden, "forbidden", "Keine Berechtigung für diese Funktion.")
			return
		}
		c.Next()
	}
}

func caller(c *gin.Context) Caller {
	v, _ := c.Get(callerKey)
	cl, _ := v.(Caller)
	return cl
}

// ErrorResponse is the body of every error answer.
type ErrorResponse struct {
	// Readable text, in the field the self-service-ui reads (as with the other DHBW Cloud APIs).
	Error string `json:"error" example:"Keine Berechtigung für diese Funktion."`
	// Stable machine-readable reason.
	Code string `json:"code" example:"forbidden"`
}

func abort(c *gin.Context, status int, code, msg string) {
	c.AbortWithStatusJSON(status, ErrorResponse{Error: msg, Code: code})
}
