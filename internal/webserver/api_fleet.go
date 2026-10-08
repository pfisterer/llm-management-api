package webserver

import (
	"context"
	"errors"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/llm-management-api/internal/fleet"
	"github.com/pfisterer/llm-management-api/internal/litellm"
)

var apiBaseAddr = regexp.MustCompile(`^https?://(\d+\.\d+\.\d+\.\d+):`)

// LiteLLMHealth turns LiteLLM's health answer into health per tunnel address.
// nil on failure, so the UI shows "unbekannt" instead of guessing.
func LiteLLMHealth(lite *litellm.Client) fleet.HealthSource {
	return func(ctx context.Context) map[string]fleet.Health {
		healthy, unhealthy, err := lite.Health(ctx)
		if err != nil {
			return nil
		}
		out := map[string]fleet.Health{}
		collect := func(list []litellm.HealthEndpoint, ok bool) {
			for _, e := range list {
				m := apiBaseAddr.FindStringSubmatch(e.APIBase)
				if m == nil {
					continue
				}
				h := out[m[1]]
				if ok {
					h.OK++
				} else {
					h.Bad++
					if h.Error == "" {
						// First line only: LiteLLM appends a whole traceback.
						h.Error = strings.SplitN(e.Error, "\n", 2)[0]
						if len(h.Error) > 160 {
							h.Error = h.Error[:160]
						}
					}
				}
				if name := strings.TrimPrefix(e.Model, "openai/"); name != "" && !slices.Contains(h.Models, name) {
					h.Models = append(h.Models, name)
				}
				out[m[1]] = h
			}
		}
		collect(healthy, true)
		collect(unhealthy, false)
		for k, h := range out {
			slices.Sort(h.Models)
			out[k] = h
		}
		return out
	}
}

// getFleet godoc
//
//	@ID			getFleet
//	@Summary		The Mac fleet
//	@Description	Machines with enrolment state, LiteLLM health, script state and the JAMF onboarding data.
//	@Tags			fleet
//	@Produce		json
//	@Security		BearerAuth
//	@Success		200	{object}	fleet.FleetView
//	@Failure		403	{object}	ErrorResponse
//	@Router			/fleet [get]
func (s *Server) getFleet(c *gin.Context) {
	v, err := s.fleet.View(c.Request.Context(), s.health, s.material)
	if err != nil {
		s.internalError(c, "fleet view", err)
		return
	}
	c.JSON(http.StatusOK, v)
}

// getFleetCSV godoc
//
//	@ID			getFleetInventoryCsv
//	@Summary	Fleet inventory as CSV
//	@Tags		fleet
//	@Produce	text/csv
//	@Security	BearerAuth
//	@Success	200	{string}	string
//	@Router		/fleet/inventory.csv [get]
func (s *Server) getFleetCSV(c *gin.Context) {
	b, err := s.fleet.CSV(c.Request.Context())
	if err != nil {
		s.internalError(c, "fleet csv", err)
		return
	}
	c.Header("Content-Disposition", `attachment; filename="llm-flotte.csv"`)
	c.Data(http.StatusOK, "text/csv; charset=utf-8", b)
}

// getFleetProfile godoc
//
//	@ID			getFleetProfile
//	@Summary		JAMF configuration profile
//	@Description	Optional location, operator and contact apply to every device in the profile's scope (one profile per device group).
//	@Tags			fleet
//	@Produce		application/x-apple-aspen-config
//	@Security		BearerAuth
//	@Param			location	query		string	false	"Standort"
//	@Param			operator	query		string	false	"Betreiber"
//	@Param			contact		query		string	false	"Kontakt"
//	@Success		200			{string}	string
//	@Router			/fleet/profile [get]
func (s *Server) getFleetProfile(c *gin.Context) {
	clean := func(k string, n int) string {
		v := strings.TrimSpace(strings.Map(func(r rune) rune {
			if r < 0x20 {
				return -1
			}
			return r
		}, c.Query(k)))
		if r := []rune(v); len(r) > n {
			v = string(r[:n])
		}
		return v
	}
	b, err := s.material.Mobileconfig(s.fleet.Config(), fleet.ProfileOptions{
		Location: clean("location", 64), Operator: clean("operator", 64), Contact: clean("contact", 96)})
	if err != nil {
		abort(c, http.StatusNotFound, "not_found", "Profilvorlage fehlt.")
		return
	}
	c.Header("Content-Disposition", `attachment; filename="de.dhbw.llm.mobileconfig"`)
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "application/x-apple-aspen-config; charset=utf-8", b)
}

// getFleetPackage godoc
//
//	@ID			getFleetPackage
//	@Summary	The newest JAMF package
//	@Tags		fleet
//	@Produce	application/octet-stream
//	@Security	BearerAuth
//	@Success	200	{file}		binary
//	@Failure	404	{object}	ErrorResponse
//	@Router		/fleet/package [get]
func (s *Server) getFleetPackage(c *gin.Context) {
	path, name, err := s.material.PackagePath()
	if err != nil {
		abort(c, http.StatusNotFound, "not_found", "Kein Paket hinterlegt (build-bundle.sh --upload).")
		return
	}
	c.Header("Cache-Control", "no-store")
	c.FileAttachment(path, name)
}

// deleteFleetPackage godoc
//
//	@ID			deleteFleetPackage
//	@Summary		Withdraw the uploaded JAMF package
//	@Description	Admins only. The machines are unaffected; only the download disappears.
//	@Tags			fleet
//	@Security		BearerAuth
//	@Success		204
//	@Router			/fleet/package [delete]
func (s *Server) deleteFleetPackage(c *gin.Context) {
	if err := s.material.DeletePackages(); err != nil {
		s.internalError(c, "delete package", err)
		return
	}
	c.Status(http.StatusNoContent)
}

// getFleetReadme godoc
//
//	@ID			getFleetReadme
//	@Summary	Fleet onboarding guide (Markdown)
//	@Tags		fleet
//	@Produce	text/markdown
//	@Security	BearerAuth
//	@Success	200	{string}	string
//	@Router		/fleet/readme [get]
func (s *Server) getFleetReadme(c *gin.Context) {
	b, err := os.ReadFile(s.material.ReadmePath)
	if err != nil {
		abort(c, http.StatusNotFound, "not_found", "Anleitung fehlt.")
		return
	}
	c.Header("Content-Disposition", `attachment; filename="LIESMICH-Flotte.md"`)
	c.Data(http.StatusOK, "text/markdown; charset=utf-8", b)
}

// blockMachine godoc
//
//	@ID			blockMachine
//	@Summary	Block a machine (it is refused at the next enrolment)
//	@Tags		fleet
//	@Security	BearerAuth
//	@Param		serial	path	string	true	"Serial number"
//	@Success	204
//	@Failure	404	{object}	ErrorResponse
//	@Router		/fleet/{serial}/block [post]
func (s *Server) blockMachine(c *gin.Context) { s.setBlocked(c, true) }

// unblockMachine godoc
//
//	@ID			unblockMachine
//	@Summary	Unblock a machine
//	@Tags		fleet
//	@Security	BearerAuth
//	@Param		serial	path	string	true	"Serial number"
//	@Success	204
//	@Failure	404	{object}	ErrorResponse
//	@Router		/fleet/{serial}/unblock [post]
func (s *Server) unblockMachine(c *gin.Context) { s.setBlocked(c, false) }

func (s *Server) setBlocked(c *gin.Context, blocked bool) {
	err := s.fleet.SetBlocked(c.Request.Context(), c.Param("serial"), blocked)
	if errors.Is(err, fleet.ErrPeerNotFound) {
		abort(c, http.StatusNotFound, "not_found", "Maschine nicht gefunden.")
		return
	}
	if err != nil {
		s.internalError(c, "block machine", err)
		return
	}
	c.Status(http.StatusNoContent)
}

// forgetMachine godoc
//
//	@ID			forgetMachine
//	@Summary		Remove a machine from the registry
//	@Description	For decommissioned devices. A machine that boots again re-enrols with a NEW address; to keep it out, block it instead.
//	@Tags			fleet
//	@Security		BearerAuth
//	@Param			serial	path	string	true	"Serial number"
//	@Success		204
//	@Failure		404	{object}	ErrorResponse
//	@Router			/fleet/{serial} [delete]
func (s *Server) forgetMachine(c *gin.Context) {
	err := s.fleet.Forget(c.Request.Context(), c.Param("serial"))
	if errors.Is(err, fleet.ErrPeerNotFound) {
		abort(c, http.StatusNotFound, "not_found", "Maschine nicht gefunden.")
		return
	}
	if err != nil {
		s.internalError(c, "forget machine", err)
		return
	}
	c.Status(http.StatusNoContent)
}
