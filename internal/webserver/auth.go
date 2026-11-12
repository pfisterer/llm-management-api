package webserver

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/cloud-self-service-golib/authn"
	"github.com/pfisterer/cloud-self-service-golib/credential"
	"github.com/pfisterer/cloud-self-service-golib/ginweb"
	"github.com/pfisterer/cloud-self-service-golib/msg"
	"github.com/pfisterer/cloud-self-service-golib/token"
	"github.com/pfisterer/llm-management-api/internal/access"
	"github.com/pfisterer/llm-management-api/internal/llm/keys"
)

// Identity is who is calling, after authentication.
type Identity struct {
	Email string
	// Subject is the Keycloak sub; empty for an API token, which knows only the e-mail address (keyPerson looks the sub up where keys need it).
	Subject string
	// ViaAPIToken: the caller authenticated with an API token (aiaas_…) rather than by signing in, which this API holds against acting on behalf of somebody and issuing further tokens. TokenID names the token, for the request log.
	ViaAPIToken bool
	TokenID     uint
	// ViaWorkspace: the caller is a workspace with its hub token on the hub listener (workspaceCaller), with the person's project rights and no platform role.
	ViaWorkspace bool
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
// Which case a credential is (none, an API token, an ID token, rejected, the provider away) is decided by golib's credential.Resolve, the same in every service; the answers here are this service's.
//
// Every successful path records whether the credential is read-only (ginweb.SetReadOnly): RejectWritesForReadOnlyTokens refuses a write on a request where nobody did.
func (s *Server) authenticate(c *gin.Context) (Identity, bool) {
	header := c.GetHeader("Authorization")
	if s.devMode && header == "" {
		ginweb.SetReadOnly(c, false)
		return Identity{Email: s.devUser, Subject: "dev-" + s.devUser}, true
	}
	var tokens credential.Tokens
	if s.tokens != nil {
		tokens = s.tokens.Service
	}
	p, err := credential.Resolve(c.Request.Context(), header, tokens, s.verifier)
	switch {
	case err == nil:
	case errors.Is(err, credential.ErrMissing), errors.Is(err, credential.ErrNoVerifier):
		abort(c, http.StatusUnauthorized, "unauthenticated", msg.T("Anmeldung erforderlich.", "Sign-in required."))
		return Identity{}, false
	case errors.Is(err, credential.ErrKeysUnavailable):
		abort(c, http.StatusServiceUnavailable, "auth_unavailable", msg.T("Anmeldedienst gerade nicht erreichbar.", "The sign-in service is not reachable right now."))
		return Identity{}, false
	case errors.Is(err, token.ErrNotFound):
		// Expired, revoked and unknown tokens are one answer (the library makes an expired token absent), so the response does not tell which.
		s.log.Infow("api token rejected")
		abort(c, http.StatusUnauthorized, "unauthenticated", msg.T("API-Token ungültig, abgelaufen oder gelöscht.", "API token invalid, expired or deleted."))
		return Identity{}, false
	case errors.Is(err, credential.ErrRejected):
		s.log.Infow("token rejected", "error", err)
		abort(c, http.StatusUnauthorized, "unauthenticated", msg.T("Anmeldung ungültig oder abgelaufen.", "Sign-in invalid or expired."))
		return Identity{}, false
	default:
		s.log.Errorw("api token lookup failed", "error", err)
		abort(c, http.StatusInternalServerError, "internal", msg.T("Interner Fehler.", "Internal error."))
		return Identity{}, false
	}
	if p.ViaAPIToken() {
		ginweb.SetReadOnly(c, p.ReadOnly)
		return Identity{Email: strings.ToLower(strings.TrimSpace(p.Token.Subject)), ViaAPIToken: true, TokenID: p.Token.ID}, true
	}
	email := strings.ToLower(strings.TrimSpace(p.Claims.Email))
	if email == "" || p.Claims.Subject == "" {
		abort(c, http.StatusUnauthorized, "unauthenticated", msg.T("Das Token enthält keine E-Mail-Adresse.", "The token contains no e-mail address."))
		return Identity{}, false
	}
	ginweb.SetReadOnly(c, false)
	return Identity{Email: email, Subject: p.Claims.Subject}, true
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
		abort(c, http.StatusForbidden, "act_as_forbidden", msg.T("Handeln im Auftrag anderer Personen ist nur nach einer Anmeldung möglich, nicht mit einem API-Token.", "Acting on behalf of other people is only possible after signing in, not with an API token."))
		return
	}
	may := self.Role.AtLeast(access.RoleAdmin) && !id.ViaAPIToken
	target, err := authn.ActAs(c.GetHeader(authn.ActAsHeader), id.Email, may)
	switch {
	case errors.Is(err, authn.ErrActAsInvalid):
		abort(c, http.StatusBadRequest, "act_as_invalid", msg.T("X-Act-As muss eine E-Mail-Adresse sein.", "X-Act-As must be an e-mail address."))
		return
	case err != nil:
		abort(c, http.StatusForbidden, "act_as_forbidden", msg.T("Handeln im Auftrag anderer Personen ist nur für Administratoren möglich.", "Only administrators can act on behalf of other people."))
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
	// ViaAPIToken carries over (it is false here, withCaller refuses X-Act-As with a token above): what a request may do depends on how the person at the keyboard signed in, not only on whom they act for.
	c.Set(callerKey, Caller{Identity: Identity{Email: target, ViaAPIToken: id.ViaAPIToken}, Person: s.access.Who(c.Request.Context(), target), Actor: id.Email, MayActAs: may})
	c.Next()
}

// keyPerson is whom the caller's keys belong to. Acting on somebody's behalf or with an API token (which knows only the e-mail address), their Keycloak sub comes from LiteLLM; a person who has never used the LLM themself has none yet, and keys cannot be bound to them (development falls back to the dev sub, so the seed's people can be tried out).
func (s *Server) keyPerson(c *gin.Context) (keys.Person, bool) {
	p, fail := s.keyPersonFor(c.Request.Context(), caller(c))
	if fail != nil {
		s.fail(c, gatewayErrors, "subject lookup", fail)
		return keys.Person{}, false
	}
	return p, true
}

// keyPersonFor is keyPerson without gin, for the REST handlers and the MCP tools alike: the person, or the refusal (as keyPerson answers it: 502 with LiteLLM's error, 409 for a person the LLM service does not know yet).
func (s *Server) keyPersonFor(ctx context.Context, cl Caller) (keys.Person, *keyFailure) {
	if cl.Actor == "" && cl.Subject != "" {
		return keys.Person{Subject: cl.Subject, Email: cl.Email}, nil
	}
	sub, found, err := s.keys.SubjectOf(ctx, cl.Email)
	if err != nil {
		return keys.Person{}, &keyFailure{http.StatusBadGateway, "litellm", msg.T("Das Gateway ist gerade nicht erreichbar.", "The gateway is not reachable right now."), err}
	}
	if !found {
		if !s.devMode {
			text := msg.T("Diese Person hat den KI-Dienst noch nie selbst genutzt. In ihrem Auftrag sind Keys erst möglich, nachdem sie sich einmal angemeldet und die LLM-Seite geöffnet hat.", "This person has never used the AI service themselves. Keys on their behalf are only possible after they have signed in once and opened the LLM page.")
			if cl.Actor == "" {
				text = msg.T("Mit einem API-Token sind LLM-Keys erst möglich, nachdem sich die Person einmal angemeldet und die LLM-Seite geöffnet hat.", "With an API token, LLM keys are only possible after the person has signed in once and opened the LLM page.")
			}
			return keys.Person{}, &keyFailure{http.StatusConflict, "person_unknown", text, nil}
		}
		sub = "dev-" + cl.Email
	}
	return keys.Person{Subject: sub, Email: cl.Email}, nil
}

// require aborts unless the caller holds at least the given role. The UI hides
// what a role may not use; this is what actually enforces it.
func require(min access.Role) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !caller(c).Role.AtLeast(min) {
			abort(c, http.StatusForbidden, "forbidden", msg.T("Keine Berechtigung für diese Funktion.", "No permission for this function."))
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

// abort answers an error to people: its text in the request's language (ginweb.Lang: Accept-Language, German by default), the stable code, and aborts the chain.
func abort(c *gin.Context, status int, code string, text msg.Text) {
	c.AbortWithStatusJSON(status, ErrorResponse{Error: text.In(ginweb.Lang(c)), Code: code})
}

// abortMachine answers an error to another system, not to people (JupyterHub on its own endpoints of the hub listener, the budget tree on /v1/tree): one text, in German, as these callers have always read it.
func abortMachine(c *gin.Context, status int, code, text string) {
	c.AbortWithStatusJSON(status, ErrorResponse{Error: text, Code: code})
}
