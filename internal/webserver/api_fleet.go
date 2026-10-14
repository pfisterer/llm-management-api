package webserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/llm-management-api/internal/fleet"
	"github.com/pfisterer/llm-management-api/internal/gpu"
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
//	@Router			/v1/fleet [get]
func (s *Server) getFleet(c *gin.Context) {
	v, err := s.fleet.View(c.Request.Context(), s.health, s.material)
	if err != nil {
		s.internalError(c, "fleet view", err)
		return
	}
	c.JSON(http.StatusOK, v)
}

// getFleetInference godoc
//
//	@ID				getFleetInference
//	@Summary		The GPU cluster's inference pool
//	@Description	vLLM replicas (one gap filler per GPU node, the base load) with node, model and state. enabled=false without the GPU part or without an inference pool.
//	@Tags			fleet
//	@Produce		json
//	@Security		BearerAuth
//	@Success		200	{object}	gpu.InferencePool
//	@Failure		403	{object}	ErrorResponse
//	@Router			/v1/fleet/inference [get]
func (s *Server) getFleetInference(c *gin.Context) {
	if s.gpu == nil {
		c.JSON(http.StatusOK, gpu.InferencePool{Pods: []gpu.InferencePod{}})
		return
	}
	v, err := s.gpu.InferencePool(c.Request.Context())
	if err != nil {
		s.internalError(c, "inference pool", err)
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
//	@Router		/v1/fleet/inventory.csv [get]
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
//	@Router			/v1/fleet/profile [get]
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
//	@Router		/v1/fleet/package [get]
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
//	@Router			/v1/fleet/package [delete]
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
//	@Router		/v1/fleet/readme [get]
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
//	@Router		/v1/fleet/{serial}/block [post]
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
//	@Router		/v1/fleet/{serial}/unblock [post]
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

// FleetModelRequest sets the model of one or more machines.
type FleetModelRequest struct {
	Serials []string `json:"serials" binding:"required,min=1"`
	// Ollama model name; empty = back to the profile's model.
	Model string `json:"model" example:"qwen3.6:35b-a3b"`
}

// FleetModelResponse is how many machines got the model.
type FleetModelResponse struct {
	Changed int `json:"changed"`
}

// setFleetModel godoc
//
//	@ID				setFleetModel
//	@Summary		Set the model of machines
//	@Description	The model wins over the machine's profile (by memory). A machine pulls it at its next enrolment, within reenrollSeconds; context and memory limit stay the profile's. An empty model returns the machines to their profile's.
//	@Tags			fleet
//	@Accept			json
//	@Produce		json
//	@Security		BearerAuth
//	@Param			body	body		FleetModelRequest	true	"machines and model"
//	@Success		200		{object}	FleetModelResponse
//	@Failure		400		{object}	ErrorResponse
//	@Failure		404		{object}	ErrorResponse
//	@Router			/v1/fleet/model [put]
func (s *Server) setFleetModel(c *gin.Context) {
	var req FleetModelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abort(c, http.StatusBadRequest, "invalid", "Ungültige Anfrage.")
		return
	}
	n, err := s.fleet.SetModel(c.Request.Context(), req.Serials, req.Model)
	var misfit *fleet.MisfitError
	switch {
	case errors.As(err, &misfit):
		abort(c, http.StatusBadRequest, "model_too_large", fmt.Sprintf("%s ist %.1f GB groß und passt nicht in die Speichergrenze von %s. Diese Maschinen abwählen oder ein kleineres Modell nehmen.", misfit.Model, misfit.SizeGB, strings.Join(misfit.Machines, ", ")))
	case errors.Is(err, fleet.ErrUnknownModel):
		abort(c, http.StatusBadRequest, "unknown_model", "Dieses Modell gibt es im Ollama-Register nicht. Name und Tag prüfen (z. B. qwen3.5:9b).")
	case errors.Is(err, fleet.ErrBadModel):
		abort(c, http.StatusBadRequest, "invalid_model", "Kein gültiger Modellname.")
	case errors.Is(err, fleet.ErrPeerNotFound):
		abort(c, http.StatusNotFound, "not_found", "Maschine nicht gefunden.")
	case errors.Is(err, fleet.ErrRegistry):
		abort(c, http.StatusBadGateway, "registry", "Das Ollama-Register ist nicht erreichbar, die Größe des Modells lässt sich nicht prüfen. Später erneut versuchen.")
	case err != nil:
		s.internalError(c, "set fleet model", err)
	default:
		c.JSON(http.StatusOK, FleetModelResponse{Changed: n})
	}
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
//	@Router			/v1/fleet/{serial} [delete]
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
