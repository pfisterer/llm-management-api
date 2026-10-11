package webserver

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/llm-management-api/internal/usage"
)

// maxUsageSpan is the longest period a report may cover (three years, like the "lifetime" view of the cloud projects' report).
const maxUsageSpan = 1098 * 24 * time.Hour

// usagePeriod reads from and to (YYYY-MM-DD, both included); default: the last 30 finished days.
func usagePeriod(c *gin.Context) (time.Time, time.Time, bool) {
	today := usage.Day(time.Now())
	to, from := today.AddDate(0, 0, -1), today.AddDate(0, 0, -30)
	parse := func(name string, dst *time.Time) bool {
		v := c.Query(name)
		if v == "" {
			return true
		}
		t, err := time.Parse("2006-01-02", v)
		if err != nil {
			abort(c, http.StatusBadRequest, "invalid", name+" ist kein Datum (JJJJ-MM-TT).")
			return false
		}
		*dst = t
		return true
	}
	if !parse("from", &from) || !parse("to", &to) {
		return time.Time{}, time.Time{}, false
	}
	if to.Before(from) || to.Sub(from) > maxUsageSpan {
		abort(c, http.StatusBadRequest, "invalid", "Zeitraum ungültig (from nach to oder länger als drei Jahre).")
		return time.Time{}, time.Time{}, false
	}
	return from, to, true
}

// getWorkspaceUsage godoc
//
//	@ID				getWorkspaceUsage
//	@Summary		Usage of all workspaces
//	@Description	What workspaces (Jupyter servers, GPU kernels) reserved per project, person, profile and GPU class: vCPU-h, RAM-GB-h, GPU-h, home GB-days, plus the measured use. Default period: the last 30 finished days. Admins only.
//	@Tags			workspaces
//	@Produce		json
//	@Security		BearerAuth
//	@Param			from	query		string	false	"first day (YYYY-MM-DD)"
//	@Param			to		query		string	false	"last day (YYYY-MM-DD), included"
//	@Success		200		{object}	usage.Report
//	@Failure		400		{object}	ErrorResponse
//	@Failure		403		{object}	ErrorResponse
//	@Router			/v1/workspaces/usage [get]
func (s *Server) getWorkspaceUsage(c *gin.Context) {
	s.workspaceUsage(c, nil)
}

// getMyWorkspaceUsage godoc
//
//	@ID				getMyWorkspaceUsage
//	@Summary		Own workspace usage
//	@Description	The caller's own rows of the workspace usage, in every project. Default period: the last 30 finished days.
//	@Tags			workspaces
//	@Produce		json
//	@Security		BearerAuth
//	@Param			from	query		string	false	"first day (YYYY-MM-DD)"
//	@Param			to		query		string	false	"last day (YYYY-MM-DD), included"
//	@Success		200		{object}	usage.Report
//	@Failure		400		{object}	ErrorResponse
//	@Router			/v1/workspaces/usage/me [get]
func (s *Server) getMyWorkspaceUsage(c *gin.Context) {
	me := strings.ToLower(caller(c).Identity.Email)
	s.workspaceUsage(c, func(r usage.Row) bool { return strings.EqualFold(r.Person, me) })
}

func (s *Server) workspaceUsage(c *gin.Context, keep usage.Filter) {
	from, to, ok := usagePeriod(c)
	if !ok {
		return
	}
	rep, err := usage.BuildReport(c.Request.Context(), s.usage, from, to, keep)
	if err != nil {
		s.internalError(c, "workspace usage", err)
		return
	}
	c.JSON(http.StatusOK, rep)
}
