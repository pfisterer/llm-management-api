package workspace

import (
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The checks of a project image (docs/todo.md image_hardening): what its registry says about it decides little — only an image for neither linux/amd64 nor linux/arm64 or one above the size limit is refused, and only when it is saved (imageRefusal). Everything else is a warning shown with the project (imageWarnings), because any image may be the project's and many run fine although they were not built for JupyterHub.

// Size limits of project images; admins may save larger ones.
const (
	DefaultImageMaxBytes  int64 = 15_000_000_000
	DefaultImageHintBytes int64 = 5_000_000_000
)

// NotebookUID is the user every workspace runs as (jovyan of the Docker stacks, runAsUser of the hub).
const NotebookUID = 1000

// NotebookHome is the home of that user, the only place that survives the end of a server (the project's volume).
const NotebookHome = "/home/jovyan"

// ImageScan is Harbor's vulnerability scan of an image: Status as Harbor names it (Success, Running, Pending, Error, …), the number of vulnerabilities with a fix by severity (the scanner ignores unfixed ones), when it ended and the artifact's page in Harbor.
type ImageScan struct {
	Status    string    `json:"status" example:"Success"`
	Critical  int       `json:"critical"`
	High      int       `json:"high"`
	ScannedAt time.Time `json:"scanned_at,omitzero"`
	URL       string    `json:"url,omitempty" example:"https://registry.gpu.services.dhbw.cloud/harbor/projects/3/repositories/kurs/artifacts-tab/artifacts/sha256:4f2a…"`
}

// SetImageLimits sets the size above which a project image is refused (maxBytes; admins may save larger ones) and from which it gets a hint (hintBytes); 0 keeps the default.
func (s *Service) SetImageLimits(maxBytes, hintBytes int64) {
	s.imageMax, s.imageHint = maxBytes, hintBytes
}

func (s *Service) imageLimits() (maxBytes, hintBytes int64) {
	maxBytes, hintBytes = s.imageMax, s.imageHint
	if maxBytes <= 0 {
		maxBytes = DefaultImageMaxBytes
	}
	if hintBytes <= 0 {
		hintBytes = DefaultImageHintBytes
	}
	return maxBytes, hintBytes
}

// FormatGB writes bytes as German gigabytes with one decimal ("2,0 GB").
func FormatGB(b int64) string {
	return strings.Replace(strconv.FormatFloat(float64(b)/1e9, 'f', 1, 64), ".", ",", 1) + " GB"
}

// imageRefusal is why an image cannot be saved as the project's: no variant the nodes run, or larger than the limit unless an admin saves it.
func imageRefusal(image string, img RegistryImage, maxBytes int64, asAdmin bool) error {
	if len(img.Archs) > 0 && !slices.Contains(img.Archs, "amd64") && !slices.Contains(img.Archs, "arm64") {
		return refuse("Image %s gibt es nur für %s; die Knoten hier laufen mit linux/amd64 oder linux/arm64.", image, strings.Join(img.Archs, ", "))
	}
	if img.Size > maxBytes && !asAdmin {
		return refuse("Image %s ist %s groß (komprimiert), erlaubt sind bis zu %s. Ein größeres Image kann ein Admin eintragen.", image, FormatGB(img.Size), FormatGB(maxBytes))
	}
	return nil
}

// imageWarnings are what an image's config says against running it here: the user, the home, the entrypoint, whether it brings jupyterhub-singleuser, its size.
func imageWarnings(img RegistryImage, maxBytes, hintBytes int64) []string {
	var out []string
	user, _, _ := strings.Cut(strings.TrimSpace(img.User), ":")
	switch {
	case user == "" || user == "0" || user == "root":
		out = append(out, fmt.Sprintf("Das Image ist für root gebaut, läuft hier aber als UID %d (jovyan); Dateien, die root gehören, sind nicht beschreibbar.", NotebookUID))
	case user == "jovyan":
	default:
		if uid, err := strconv.Atoi(user); err != nil || uid != NotebookUID {
			out = append(out, fmt.Sprintf("Das Image ist für den Benutzer %s gebaut, läuft hier aber als UID %d (jovyan); dessen Dateien sind womöglich nicht beschreibbar.", user, NotebookUID))
		}
	}
	switch home := img.EnvValue("HOME"); {
	case home == "":
		out = append(out, fmt.Sprintf("HOME ist im Image nicht gesetzt; erhalten bleibt beim Beenden des Servers nur, was unter %s liegt.", NotebookHome))
	case strings.TrimSuffix(home, "/") != NotebookHome:
		out = append(out, fmt.Sprintf("HOME zeigt im Image auf %s statt auf %s; Dateien außerhalb von %s gehen beim Beenden des Servers verloren.", home, NotebookHome, NotebookHome))
	}
	if len(img.Entrypoint) > 0 && !initEntrypoint(img.Entrypoint) {
		out = append(out, fmt.Sprintf("Das Image hat einen eigenen Start-Befehl (ENTRYPOINT %s); der Start-Befehl des Servers wird ihm nur als Argument übergeben.", strings.Join(img.Entrypoint, " ")))
	}
	if !hasSingleuser(img) {
		out = append(out, "Ob das Image jupyterhub-singleuser enthält, ist nicht feststellbar (kein Image der Docker-Stacks und keine Basis von hier); ohne es startet der Server nicht.")
	}
	switch {
	case img.Size > maxBytes:
		out = append(out, fmt.Sprintf("Das Image ist %s groß (komprimiert) und damit über der Grenze von %s; der erste Start auf einem Knoten ohne das Image dauert lange.", FormatGB(img.Size), FormatGB(maxBytes)))
	case img.Size >= hintBytes:
		out = append(out, fmt.Sprintf("Das Image ist %s groß (komprimiert); der erste Start auf einem Knoten ohne das Image dauert entsprechend länger.", FormatGB(img.Size)))
	}
	return out
}

// initEntrypoint tells whether an entrypoint only runs the server's start command: an init process (tini, docker-init, dumb-init) with its options, followed by nothing or by start.sh of the Docker stacks (and of the bases here), which runs its arguments.
func initEntrypoint(ep []string) bool {
	switch path.Base(ep[0]) {
	case "tini", "tini-static", "docker-init", "dumb-init":
	default:
		return false
	}
	rest := ep[1:]
	for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
		rest = rest[1:]
	}
	return len(rest) == 0 || (len(rest) == 1 && path.Base(rest[0]) == "start.sh")
}

// hasSingleuser tells whether an image evidently brings jupyterhub-singleuser: a base of here (label BaseLabel) or derived from one, or an image of the Jupyter Docker stacks (their start scripts as CMD, their maintainer label).
func hasSingleuser(img RegistryImage) bool {
	if img.Labels[BaseLabel] != "" || strings.Contains(img.Labels["maintainer"], "Jupyter Project") {
		return true
	}
	for _, c := range img.Cmd {
		switch path.Base(c) {
		case "start-notebook.py", "start-notebook.sh", "start-singleuser.py", "start-singleuser.sh", "jupyterhub-singleuser":
			return true
		}
	}
	return false
}

// EnvValue is a variable of the image's ENV ("" when not set).
func (i RegistryImage) EnvValue(name string) string {
	for _, e := range i.Env {
		if k, v, ok := strings.Cut(e, "="); ok && k == name {
			return v
		}
	}
	return ""
}

// takeImageFacts copies what the checks found about an image to the project.
func (s *Service) takeImageFacts(p *Project, img RegistryImage) {
	maxBytes, hintBytes := s.imageLimits()
	p.ImageSize, p.ImageUser, p.ImageWarnings = img.Size, img.User, imageWarnings(img, maxBytes, hintBytes)
}

// sameImageFacts tells whether the project already has what the checks found about an image.
func (s *Service) sameImageFacts(p Project, img RegistryImage) bool {
	q := p
	s.takeImageFacts(&q, img)
	return q.ImageSize == p.ImageSize && q.ImageUser == p.ImageUser && slices.Equal(q.ImageWarnings, p.ImageWarnings)
}
