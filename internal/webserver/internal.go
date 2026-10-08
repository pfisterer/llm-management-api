package webserver

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/llm-management-api/internal/access"
	"go.uber.org/zap"
)

// AccessAnswer is the access decision for one person, for services inside the
// cluster that act on someone's behalf without a token of theirs.
type AccessAnswer struct {
	Role access.Role `json:"role"`
	Tier string      `json:"tier,omitempty"`
}

// InternalRouter answers in-cluster services. Today that is the chat exchange
// of the key broker: LibreChat calls it with the person's identity in headers,
// and before it issues that person's chat key it asks here whether an access
// rule lets them in, and with which tier — the same decision the person API
// makes, so "no rule, no access" holds for the chat too.
//
// A THIRD listener, separate from both others: it takes an email address at
// face value. Its only protection is the NetworkPolicy that lets nothing but
// the broker reach this port, so it must never share a listener with a route
// that has an ingress.
func InternalRouter(svc *access.Service, log *zap.SugaredLogger, devMode bool) *gin.Engine {
	if !devMode {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.New()
	r.Use(gin.Recovery())
	r.GET("/healthz", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	r.GET("/internal/v1/access", func(c *gin.Context) {
		email := strings.ToLower(strings.TrimSpace(c.Query("email")))
		if email == "" || !strings.Contains(email, "@") {
			c.JSON(http.StatusBadRequest, gin.H{"error": "email required"})
			return
		}
		d, _, err := svc.Decide(c.Request.Context(), email)
		if err != nil {
			log.Errorw("internal access decision failed", "email", email, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
			return
		}
		ans := AccessAnswer{Role: d.Role}
		if d.Role != access.RoleNone {
			ans.Tier = d.Tier
		}
		c.JSON(http.StatusOK, ans)
	})
	r.NoRoute(func(c *gin.Context) { c.JSON(http.StatusNotFound, gin.H{"error": "not found"}) })
	return r
}
