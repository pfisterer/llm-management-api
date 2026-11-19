package fleet

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// StrataPrefix marks a reserved model name: not an Ollama model, but Strata (github.com/Niko1221/Strata, Metal engine) with one of its models, which the fleet script runs on the Mac in place of Ollama (same LaunchDaemon label, so the gate and restarts work unchanged). Set like any model, per machine in the portal or as an inventory override.
const StrataPrefix = "strata:"

// StrataModel is what a reserved strata name needs on the machine. The script maps the name to Strata's family and model (setup.py --family/--model); the service only checks that a machine can take it.
type StrataModel struct {
	// MinRAMGb: the memory of the smallest Mac class it runs on (measured 19.11.2026 on wimac32, M4 Pro 48 GB; Strata and Ollama exclude each other there). A machine may report up to 3 GB less (hw.memsize of some models).
	MinRAMGb int
}

// strataModels are the reserved names the fleet script knows (dhbw-llm-enroll.sh, strata_spec). A new one needs both sides.
var strataModels = map[string]StrataModel{
	// Qwen3.8-Flash-Next in agentionai's Gyro-S quantisation: 58.5 GB download, experts streamed from the SSD.
	"strata:gyro-s": {MinRAMGb: 48},
}

// StrataModels lists the reserved names, sorted, for the portal's suggestions.
func StrataModels() []string { return slices.Sorted(maps.Keys(strataModels)) }

// IsStrata tells a reserved strata name (known or not) from an Ollama model.
func IsStrata(model string) bool { return strings.HasPrefix(model, StrataPrefix) }

// ErrUnknownStrata is a name with the strata prefix that the fleet script does not know.
var ErrUnknownStrata = errors.New("unknown strata model")

// StrataMisfitError names the machines that cannot run a strata model: too little memory, or no Mac whose inference service the fleet script controls through the tunnel.
type StrataMisfitError struct {
	Model    string
	MinRAMGb int
	Machines []string // "wimac05 (32 GB)", "gpu-box (linux)", "site (direct)"
}

func (e *StrataMisfitError) Error() string {
	return fmt.Sprintf("%s needs a Mac of the fleet with at least %d GB: %s", e.Model, e.MinRAMGb, strings.Join(e.Machines, ", "))
}

// StrataConfig is the `strata` part of the fleet block: how a machine running a strata model is offered.
type StrataConfig struct {
	// Alias the machine's model is registered under in LiteLLM, and only that one (default chat-large). Its own name passes through as for every model.
	Alias string `json:"alias"`
	// Weight in that alias, unless the inventory's override for the serial sets one (default 0.5: a Mac answers at ~15 tokens/s, the GB10 and the RTX much faster).
	Weight float64 `json:"weight"`
	// Context the script sets Strata up with (default 32768: the window Strata's Mac rules were tested to, mac_fit.TESTED_CONTEXT, and the l class's Ollama context; 16384 is what was measured on wimac32). A change makes the machines set Strata up again (the download stays). The profile's and the override's context are Ollama's and do not apply.
	Context int `json:"context"`
}

func (s StrataConfig) alias() string {
	if s.Alias == "" {
		return "chat-large"
	}
	return s.Alias
}

func (s StrataConfig) weight() float64 {
	if s.Weight <= 0 {
		return 0.5
	}
	return s.Weight
}

func (s StrataConfig) context() int {
	if s.Context <= 0 {
		return 32768
	}
	return s.Context
}

// strataKey is the API key of one machine's Strata server: derived from the fleet's API key and the serial, so the key the fleet uses for its other sites never leaves the cluster, and a Mac learns only its own. Empty when the fleet has no API key (the script then does not start Strata).
func (c Config) strataKey(serial string) string {
	if c.APIKey == "" {
		return ""
	}
	m := hmac.New(sha256.New, []byte(c.APIKey))
	m.Write([]byte("strata/" + serial))
	return "strata-" + hex.EncodeToString(m.Sum(nil))
}

// strataMisfit names a machine that cannot run a strata model with the reason in brackets (its memory, its OS, or "direct"/"extern" for an inference service the fleet script does not control), "" when it can.
func strataMisfit(p Peer, m StrataModel) string {
	switch {
	case p.Transport == "direct":
		return p.Name + " (direct)"
	case p.OS != "darwin" || !p.Controls:
		kind := p.OS
		if kind == "" || kind == "darwin" {
			kind = "extern"
		}
		return p.Name + " (" + kind + ")"
	}
	if p.RAMMb+3072 < m.MinRAMGb*1024 {
		return fmt.Sprintf("%s (%d GB)", p.Name, p.RAMMb/1024)
	}
	return ""
}
