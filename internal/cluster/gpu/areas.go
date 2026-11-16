package gpu

// The areas below ~/dhbw (workspace.Area, docs/todo.md areas_project, areas_groups) in the pods this service builds: batch jobs and the files pods. The hub mounts the same for the servers (jupyterhub-values.yaml.j2, dhbw_storage_pod), the GPU kernel pods take the server's mounts.

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/pfisterer/llm-management-api/internal/cluster/workspace"
)

// areaDirsScript runs in the init container dhbw-dirs with the volume at /ws, as the person: it makes the home ($1), each area's folder in the volume and its mount point in the home (the remaining arguments, home-relative mount and volume dir in turns), so that none appears as root's (Kubernetes would create a missing subPath or mount point as root), writes the README of ~/dhbw and replaces the old mount point ~/project-shared, which is empty in the volume, by a link to dhbw/project (a folder someone filled stays).
const areaDirsScript = `set -e
h="/ws/$1"; shift
mkdir -p "$h"
while [ $# -ge 2 ]; do mkdir -p "$h/$1" "/ws/$2"; shift 2; done
if [ -d "$h/dhbw" ]; then
  [ -f "$h/dhbw/README.txt" ] || printf '%s\n' "$DHBW_AREAS_README" > "$h/dhbw/README.txt" || true
  if [ -d "$h/dhbw/project" ] && [ ! -L "$h/project-shared" ]; then
    rmdir "$h/project-shared" 2>/dev/null || true
    [ -e "$h/project-shared" ] || ln -s dhbw/project "$h/project-shared" || true
  fi
fi
`

// AreasReadme is what ~/dhbw/README.txt says.
const AreasReadme = `Hier haengt die DHBW Cloud gemeinsame Ordner ein:
  project/          der gemeinsame Ordner des Projekts (frueher ~/project-shared)
  groups/<gruppe>/  die Ordner der eigenen Gruppen
Was hier liegt, sehen auch andere. Eigene Dateien gehoeren direkt ins Home.

The DHBW Cloud mounts shared folders here:
  project/          the project's shared folder (formerly ~/project-shared)
  groups/<group>/   the folders of your groups
Others see what is here. Keep your own files in your home.`

// areaDirsInit is the init container dhbw-dirs of a pod with the person's home folder home (homes/<slug>) and the areas.
func areaDirsInit(image, home string, areas []workspace.Area, sec map[string]any) map[string]any {
	cmd := []any{"sh", "-c", areaDirsScript, "dhbw-dirs", home}
	for _, a := range areas {
		cmd = append(cmd, a.Mount, a.Dir)
	}
	return map[string]any{
		"name": "dhbw-dirs", "image": image,
		"command":         cmd,
		"env":             []any{envVar("DHBW_AREAS_README", AreasReadme)},
		"securityContext": sec,
		"resources":       map[string]any{"requests": map[string]any{"cpu": "10m", "memory": "32Mi"}, "limits": map[string]any{"memory": "128Mi"}},
		"volumeMounts":    []any{map[string]any{"name": "home", "mountPath": "/ws"}},
	}
}

// areaMounts are the mounts of the areas below the folder at (the home, or /data in a files pod); readOnly forces all of them read-only.
func areaMounts(at string, areas []workspace.Area, readOnly bool) []any {
	out := []any{}
	for _, a := range areas {
		m := map[string]any{"name": "home", "mountPath": at + "/" + a.Mount, "subPath": a.Dir}
		if readOnly || a.ReadOnly {
			m["readOnly"] = true
		}
		out = append(out, m)
	}
	return out
}

// areasKey tells area sets apart in the key of a files pod: a person whose groups changed gets a new pod rather than the old mounts.
func areasKey(areas []workspace.Area) string {
	h := sha256.New()
	for _, a := range areas {
		ro := "w"
		if a.ReadOnly {
			ro = "r"
		}
		h.Write([]byte(a.Mount + "\x00" + a.Dir + "\x00" + ro + "\x00"))
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// AreaRel maps a home-relative path through the old place of the shared folder (SharedDir, ~/project-shared until 16.11.2026) to its area (dhbw/project), so paths of before keep working in the API and the CLI.
func AreaRel(rel string) string {
	switch {
	case rel == SharedDir:
		return workspace.ProjectArea
	case strings.HasPrefix(rel, SharedDir+"/"):
		return workspace.ProjectArea + strings.TrimPrefix(rel, SharedDir)
	}
	return rel
}

// areaOf is the area rel lies in or is (false: none); rel is home-relative and cleaned.
func areaOf(rel string, areas []workspace.Area) (workspace.Area, bool) {
	for _, a := range areas {
		if rel == a.Mount || strings.HasPrefix(rel, a.Mount+"/") {
			return a, true
		}
	}
	return workspace.Area{}, false
}

// areaNames lists the writable areas for a message ("keine" when there are none).
func areaNames(areas []workspace.Area) string {
	names := []string{}
	for _, a := range areas {
		if !a.ReadOnly {
			names = append(names, "~/"+a.Mount)
		}
	}
	if len(names) == 0 {
		return "–"
	}
	return strings.Join(names, ", ")
}
