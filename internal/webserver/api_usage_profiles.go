package webserver

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/llm-management-api/internal/llm/usageprofiles"
)

// ProfileReport is the last computed report of the usage profiles; false before the first.
type ProfileReport interface {
	Report() (usageprofiles.Report, bool)
}

// getUsageProfiles godoc
//
//	@ID				getUsageProfiles
//	@Summary		Usage profiles of the LLM gateway
//	@Description	How the LLM gateway is used, per model alias (code, agent, chat-default, …), from LiteLLM's spend logs of the last days, computed nightly: input and output tokens per request, tokens and requests per active key-hour, the busiest minute and the requests at once of an active key, tokens of a key in a key-budget window, each as median and P90 with the number of values and of distinct keys. An alias or a figure with fewer distinct keys than min_keys is left out; nothing per person, key, team or project. user_agents (shares of the requests by kind of client) only where the spend logs name the user agent. For the resource estimator of resource-management-api, with the tree's token. 503 before the first computation.
//	@Tags			tree
//	@Produce		json
//	@Security		BearerAuth
//	@Success		200	{object}	usageprofiles.Report
//	@Failure		401	{object}	ErrorResponse
//	@Failure		503	{object}	ErrorResponse
//	@Router			/v1/usage-profiles [get]
func (s *Server) getUsageProfiles(c *gin.Context) {
	if s.profiles == nil {
		abortMachine(c, http.StatusServiceUnavailable, "disabled", "Die Nutzungsprofile sind abgeschaltet.")
		return
	}
	rep, ok := s.profiles.Report()
	if !ok {
		abortMachine(c, http.StatusServiceUnavailable, "not_ready", "Die Nutzungsprofile sind noch nicht berechnet.")
		return
	}
	c.JSON(http.StatusOK, rep)
}
