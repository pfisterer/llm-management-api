package fleet

import (
	"bytes"
	"cmp"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"time"
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
	// SourcePort: the port on which a Mac whose Strata is ready serves its model file read-only to the other Macs of its network (default 11480; 1024-65535, the server runs unprivileged).
	SourcePort int `json:"sourcePort"`
	// SourcePrefix: the IPv4 prefix length that makes two Macs one network for the model download (default 24, 8-32). A Mac only ever fetches from a Mac whose primary address shares this prefix with its own, never across networks.
	SourcePrefix int `json:"sourcePrefix"`
	// SourceWaitHours: how long a Mac waits for a source in its network before it downloads from Hugging Face itself (default 6).
	SourceWaitHours int `json:"sourceWaitHours"`
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

func (s StrataConfig) sourcePort() int {
	if s.SourcePort <= 0 {
		return 11480
	}
	return s.SourcePort
}

func (s StrataConfig) sourcePrefix() int {
	if s.SourcePrefix <= 0 {
		return 24
	}
	return s.SourcePrefix
}

func (s StrataConfig) sourceWaitHours() int {
	if s.SourceWaitHours <= 0 {
		return 6
	}
	return s.SourceWaitHours
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

// EnrollStrata is the `strata` part of the enrolment answer, set only while a strata model is assigned to a tunnelled machine: where the machine fetches the model file from (once per network from Hugging Face, otherwise from a Mac of the same network that has it) and how it serves its own file to the others once its Strata is ready.
type EnrollStrata struct {
	// Source is the base URL (HF_ENDPOINT) of a Mac in the same network whose Strata is ready, "http://<primary address>:<port>"; empty = Hugging Face, or wait (Wait).
	Source string `json:"source,omitempty"`
	// Wait: no Mac in the network has the model yet and another one (the leader) is downloading it; ask again at the next enrolment, after WaitHours of waiting download from Hugging Face.
	Wait      bool `json:"wait"`
	WaitHours int  `json:"waitHours"`
	// Port the machine serves its model file on once its Strata is ready, and Network the only addresses it serves to (its own prefix); Network is empty when the machine reported no IPv4 address, it then neither serves nor fetches from a neighbour.
	Port    int    `json:"port"`
	Network string `json:"network,omitempty"`
}

// strataNetwork is the network of a primary address for the model download: its IPv4 prefix of the configured length. Only IPv4 (the script reports the IPv4 address of the default route's interface); anything else has no network and takes part in nothing.
func (c Config) strataNetwork(primaryIP string) (netip.Prefix, bool) {
	a, err := netip.ParseAddr(primaryIP)
	if err != nil || !a.Unmap().Is4() {
		return netip.Prefix{}, false
	}
	p, err := a.Unmap().Prefix(c.Strata.sourcePrefix())
	return p, err == nil
}

// strataHas: the machine has its strata model, i.e. it reported the assigned name as served, which the script does only after its Strata answered with the machine's key (so the setup, including the download, is complete).
func strataHas(p Peer) bool { return IsStrata(p.Model) && slices.Contains(p.Models(), p.Model) }

// strataSource fills the `strata` part of the answer to me (already updated by this enrolment; peers contains it). Derived from the fleet's state at every enrolment, nothing kept: a peer in the same network (same prefix of the primary address) is a source when it was seen within one and a half enrolment intervals, is neither blocked nor pending, has the same strata model assigned, has it (strataHas) and is not busy (setting up, or someone works at it). Several sources are spread by rendezvous hashing over the serials, so a machine keeps its source while the set is stable. Without a source: if a Mac of the network has the model but is busy, wait for it; otherwise the first by name of the online Macs of the network that have the model assigned but not yet (the leader) downloads from Hugging Face and the others wait; when it drops out, the next one leads. A machine that has the model itself gets neither.
func (s *Service) strataSource(peers []Peer, me Peer, now time.Time) *EnrollStrata {
	if !IsStrata(me.Model) || me.Transport == "direct" {
		return nil
	}
	out := &EnrollStrata{Port: s.cfg.Strata.sourcePort(), WaitHours: s.cfg.Strata.sourceWaitHours()}
	lan, ok := s.cfg.strataNetwork(me.PrimaryIP)
	if !ok {
		return out
	}
	out.Network = lan.String()
	if strataHas(me) {
		return out
	}
	fresh := time.Duration(s.cfg.ReenrollSeconds) * time.Second * 3 / 2
	var sources []Peer
	held := false
	leader := me
	for _, p := range peers {
		if p.Serial == me.Serial || p.Blocked || p.Pending || p.Transport == "direct" || p.Model != me.Model ||
			p.LastSeen == nil || now.Sub(*p.LastSeen) > fresh {
			continue
		}
		if pn, ok := s.cfg.strataNetwork(p.PrimaryIP); !ok || pn != lan {
			continue
		}
		switch {
		case strataHas(p) && !p.Busy:
			sources = append(sources, p)
		case strataHas(p):
			held = true
		case cmp.Or(cmp.Compare(p.Name, leader.Name), cmp.Compare(p.Serial, leader.Serial)) < 0:
			leader = p
		}
	}
	if len(sources) > 0 {
		best := slices.MaxFunc(sources, func(a, b Peer) int {
			return bytes.Compare(rendezvous(me.Serial, a.Serial), rendezvous(me.Serial, b.Serial))
		})
		out.Source = fmt.Sprintf("http://%s:%d", best.PrimaryIP, out.Port)
		return out
	}
	out.Wait = held || leader.Serial != me.Serial
	return out
}

// rendezvous is the weight of source for requester (highest random weight hashing): every requester ranks the sources differently, the ranking does not change when other sources come or go.
func rendezvous(requester, source string) []byte {
	h := sha256.Sum256([]byte(requester + "\x00" + source))
	return h[:]
}
