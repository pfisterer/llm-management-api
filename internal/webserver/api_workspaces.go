package webserver

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/llm-management-api/internal/access"
	"github.com/pfisterer/llm-management-api/internal/usage"
	"github.com/pfisterer/llm-management-api/internal/workspace"
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
//	@Description	What workspaces (Jupyter servers, GPU kernels) reserved per project, person, profile and GPU class: vCPU-h, RAM-GB-h, GPU-h, home GB-days, plus the measured use. Default period: the last 30 finished days. Admins see everything or one project; a project's managers see their project.
//	@Tags			workspaces
//	@Produce		json
//	@Security		BearerAuth
//	@Param			from	query		string	false	"first day (YYYY-MM-DD)"
//	@Param			to		query		string	false	"last day (YYYY-MM-DD), included"
//	@Param			project	query		string	false	"only this project (required for managers)"
//	@Success		200		{object}	usage.Report
//	@Failure		400		{object}	ErrorResponse
//	@Failure		403		{object}	ErrorResponse
//	@Router			/v1/workspaces/usage [get]
func (s *Server) getWorkspaceUsage(c *gin.Context) {
	cl := caller(c)
	project := c.Query("project")
	if cl.Decision.Role.AtLeast(access.RoleAdmin) {
		if project == "" {
			s.workspaceUsage(c, nil)
			return
		}
	} else {
		p, err := s.ws.Get(c.Request.Context(), project)
		if project == "" || err != nil || p.RoleOf(cl.Tokens) != workspace.RoleManager {
			abort(c, http.StatusForbidden, "forbidden", "Nur für Admins und die Verwaltung des Projekts.")
			return
		}
	}
	s.workspaceUsage(c, func(r usage.Row) bool { return r.Project == project })
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

// myProjects are the active projects of the caller (cached in the request).
func (s *Server) myProjects(c *gin.Context) []workspace.Mine {
	if v, ok := c.Get("workspace-projects"); ok {
		return v.([]workspace.Mine)
	}
	if s.ws == nil {
		return nil
	}
	mine, err := s.ws.ForPerson(c.Request.Context(), caller(c).Tokens, caller(c).Email, false)
	if err != nil {
		s.log.Warnw("workspace projects", "error", err)
		mine = nil
	}
	c.Set("workspace-projects", mine)
	return mine
}

// getWorkspaceCatalog godoc
//
//	@ID				getWorkspaceCatalog
//	@Summary		Profiles and GPU classes
//	@Description	What a project can allow: the JupyterHub profiles (with the GPU class they use) and the GPU classes.
//	@Tags			workspaces
//	@Produce		json
//	@Security		BearerAuth
//	@Success		200	{object}	workspace.Config
//	@Failure		403	{object}	ErrorResponse
//	@Router			/v1/workspaces/catalog [get]
func (s *Server) getWorkspaceCatalog(c *gin.Context) { c.JSON(http.StatusOK, s.ws.Catalog()) }

// listWorkspaceProjects godoc
//
//	@ID				listWorkspaceProjects
//	@Summary		Workspace projects
//	@Description	The caller's active projects with their role. Admins with all=true: every project, also ended ones.
//	@Tags			workspaces
//	@Produce		json
//	@Security		BearerAuth
//	@Param			all	query		bool	false	"every project (admins)"
//	@Success		200	{array}		workspace.Mine
//	@Router			/v1/workspaces/projects [get]
func (s *Server) listWorkspaceProjects(c *gin.Context) {
	cl := caller(c)
	if c.Query("all") == "true" && cl.Decision.Role.AtLeast(access.RoleAdmin) {
		list, err := s.ws.ForPerson(c.Request.Context(), cl.Tokens, cl.Email, true)
		if err != nil {
			s.internalError(c, "workspace projects", err)
			return
		}
		c.JSON(http.StatusOK, list)
		return
	}
	mine := s.myProjects(c)
	if mine == nil {
		mine = []workspace.Mine{}
	}
	c.JSON(http.StatusOK, mine)
}

// updateWorkspaceProject godoc
//
//	@ID				updateWorkspaceProject
//	@Summary		Change a workspace project
//	@Description	Admins change everything; the project's managers only the description, the members and the images.
//	@Tags			workspaces
//	@Accept			json
//	@Produce		json
//	@Security		BearerAuth
//	@Param			slug	path		string				true	"short name"
//	@Param			project	body		workspace.Project	true	"the project"
//	@Success		200		{object}	workspace.Project
//	@Failure		400		{object}	ErrorResponse
//	@Failure		403		{object}	ErrorResponse
//	@Failure		404		{object}	ErrorResponse
//	@Router			/v1/workspaces/projects/{slug} [put]
func (s *Server) updateWorkspaceProject(c *gin.Context) {
	cl := caller(c)
	var p workspace.Project
	if err := c.ShouldBindJSON(&p); err != nil {
		abort(c, http.StatusBadRequest, "invalid", "Ungültiger Inhalt.")
		return
	}
	p.Slug = c.Param("slug")
	admin := cl.Decision.Role.AtLeast(access.RoleAdmin)
	if !admin {
		cur, err := s.ws.Get(c.Request.Context(), p.Slug)
		if s.workspaceError(c, err) {
			return
		}
		if cur.RoleOf(cl.Tokens) != workspace.RoleManager {
			abort(c, http.StatusForbidden, "forbidden", "Nur für Admins und die Verwaltung des Projekts.")
			return
		}
	}
	out, err := s.ws.Update(c.Request.Context(), p, cl.Email, admin)
	if s.workspaceError(c, err) {
		return
	}
	c.JSON(http.StatusOK, out)
}

// deleteWorkspaceProject godoc
//
//	@ID				deleteWorkspaceProject
//	@Summary		Delete a workspace project
//	@Description	Running servers keep running; usage stays booked to the project.
//	@Tags			workspaces
//	@Security		BearerAuth
//	@Param			slug	path	string	true	"short name"
//	@Success		204
//	@Failure		403	{object}	ErrorResponse
//	@Failure		404	{object}	ErrorResponse
//	@Router			/v1/workspaces/projects/{slug} [delete]
func (s *Server) deleteWorkspaceProject(c *gin.Context) {
	if s.workspaceError(c, s.ws.Delete(c.Request.Context(), c.Param("slug"))) {
		return
	}
	c.Status(http.StatusNoContent)
}

func (s *Server) workspaceError(c *gin.Context, err error) bool {
	var r *workspace.Refusal
	switch {
	case err == nil:
		return false
	case errors.As(err, &r):
		abort(c, http.StatusBadRequest, "invalid", r.Reason)
	case errors.Is(err, workspace.ErrNotFound):
		abort(c, http.StatusNotFound, "not_found", "Projekt nicht gefunden.")
	default:
		s.internalError(c, "workspace project", err)
	}
	return true
}
