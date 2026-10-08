package webserver

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/cloud-self-service-golib/authn"
	"github.com/pfisterer/cloud-self-service-golib/oidcauth"
	"github.com/pfisterer/llm-management-api/internal/access"
)

// Identity is who is calling, after authentication.
type Identity struct {
	Email   string
	Subject string
}

// Caller is the identity plus the access decision.
type Caller struct {
	Identity
	Decision access.Decision
	Tokens   []string
}

const callerKey = "llm.caller"

// authenticate turns the request into an Identity or aborts with 401/503.
//
// Production: the bearer is the ID token the self-service BFF forwards
// (audience = its client id), verified against Keycloak's keys.
// Development: X-Dummy-Auth-User names the caller, nothing is verified.
func (s *Server) authenticate(c *gin.Context) (Identity, bool) {
	if s.devMode {
		if u := strings.TrimSpace(c.GetHeader("X-Dummy-Auth-User")); u != "" {
			return Identity{Email: strings.ToLower(u), Subject: "dev-" + strings.ToLower(u)}, true
		}
	}
	raw, ok := authn.CutBearerPrefix(c.GetHeader("Authorization"))
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
	return Identity{Email: email, Subject: claims.Subject}, true
}

// withCaller authenticates and resolves the access decision. It does NOT
// require any role: /me must answer people without access too.
func (s *Server) withCaller(c *gin.Context) {
	id, ok := s.authenticate(c)
	if !ok {
		return
	}
	decision, tokens, err := s.access.Decide(c.Request.Context(), id.Email)
	if err != nil {
		s.log.Errorw("access decision failed", "email", id.Email, "error", err)
		abort(c, http.StatusInternalServerError, "internal", "Zugriff konnte nicht geprüft werden.")
		return
	}
	c.Set(callerKey, Caller{Identity: id, Decision: decision, Tokens: tokens})
	c.Next()
}

// require aborts unless the caller holds at least the given role. The UI hides
// what a role may not use; this is what actually enforces it.
func require(min access.Role) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !caller(c).Decision.Role.AtLeast(min) {
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
	Error   string `json:"error" example:"forbidden"`
	Message string `json:"message" example:"Keine Berechtigung für diese Funktion."`
}

func abort(c *gin.Context, status int, code, msg string) {
	c.AbortWithStatusJSON(status, ErrorResponse{Error: code, Message: msg})
}
