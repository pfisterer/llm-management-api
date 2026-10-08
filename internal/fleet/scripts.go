package fleet

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// The scripts a machine keeps up to date. Only the macOS ones go into the
// manifest; the Linux agent updates itself during enrolment.
var (
	fleetScripts    = []string{"dhbw-llm-enroll.sh", "dhbw-llm-tunnel.sh", "dhbw-llm-gate.sh", "dhbw-llm-agent.sh"}
	manifestScripts = []string{"dhbw-llm-enroll.sh", "dhbw-llm-tunnel.sh", "dhbw-llm-gate.sh"}
)

var ErrScriptNotFound = errors.New("script not found")

// Scripts serves the fleet scripts from a directory (a ConfigMap mount).
//
// The price of self-update, stated plainly: whoever controls this service can
// run code as root on the machines. fleet.selfUpdate switches it off.
type Scripts struct{ dir string }

func NewScripts(dir string) *Scripts { return &Scripts{dir: dir} }

// Current returns name -> sha256 of the delivered macOS scripts.
func (s *Scripts) Current() map[string]string {
	out := map[string]string{}
	if s == nil || s.dir == "" {
		return out
	}
	for _, name := range manifestScripts {
		b, err := os.ReadFile(filepath.Join(s.dir, name))
		if err != nil {
			continue
		}
		h := sha256.Sum256(b)
		out[name] = hex.EncodeToString(h[:])
	}
	return out
}

// UpdateMode: "aus", "testgeraete" (only canary serials) or "alle".
func UpdateMode(cfg Config) string {
	switch {
	case !cfg.SelfUpdate:
		return "aus"
	case len(cfg.SelfUpdateCanary) > 0:
		return "testgeraete"
	default:
		return "alle"
	}
}

func receivesUpdates(cfg Config, serial string) bool {
	switch UpdateMode(cfg) {
	case "alle":
		return true
	case "testgeraete":
		return slices.Contains(cfg.SelfUpdateCanary, serial)
	}
	return false
}

// Manifest is "name:sha256 name:sha256 …", empty when this machine gets no
// update. One line on purpose: the bash 3.2 agent reads it with plutil.
func (s *Scripts) Manifest(cfg Config, serial string) string {
	if !receivesUpdates(cfg, serial) {
		return ""
	}
	cur := s.Current()
	parts := make([]string, 0, len(cur))
	for _, name := range manifestScripts {
		if h, ok := cur[name]; ok {
			parts = append(parts, name+":"+h)
		}
	}
	return strings.Join(parts, " ")
}

// State compares what a machine reports with what is delivered.
func (s *Scripts) State(cfg Config, p Peer) string {
	if p.OS != "darwin" {
		return ""
	}
	have := map[string]string{}
	for _, e := range strings.Fields(p.Scripts) {
		if name, h, ok := strings.Cut(e, ":"); ok {
			have[name] = h
		}
	}
	if len(have) == 0 {
		return "unbekannt"
	}
	for name, h := range s.Current() {
		if have[name] != h {
			if receivesUpdates(cfg, p.Serial) {
				return "veraltet"
			}
			return "zurückgehalten"
		}
	}
	return "aktuell"
}

// Read returns a script for download. The allowlist is the path-traversal
// defence: nothing the caller sends is joined into a path.
func (s *Scripts) Read(cfg Config, name string) ([]byte, error) {
	if !cfg.SelfUpdate || !slices.Contains(fleetScripts, name) || s == nil || s.dir == "" {
		return nil, ErrScriptNotFound
	}
	b, err := os.ReadFile(filepath.Join(s.dir, name))
	if err != nil {
		return nil, ErrScriptNotFound
	}
	// The Linux agent gets its enrolment URL filled in, so `--url` is not needed
	// at installation. Only for this file: the manifest scripts must be served
	// verbatim, or their checksum differs and the Macs reload them every run.
	if name == "dhbw-llm-agent.sh" {
		b = []byte(strings.ReplaceAll(string(b), "@@ENROLL_URL@@", "https://"+cfg.EnrollHostname))
	}
	return b, nil
}
