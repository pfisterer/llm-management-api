package fleet

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Metrics renders the fleet in the Prometheus text exposition format (0.0.4), for the monitoring of the self-service cluster: a machine that stops reporting, loses its model or is no longer ready raises an alarm there. Written by hand rather than with client_golang: a handful of gauges computed from one List per scrape need neither a registry nor a dependency.
//
// Cardinality stays bounded by an admin's decision: per-machine series exist only for approved machines (not pending, not blocked), labelled with name (unique, cleaned at enrolment), site (the machine's location, as reported, at most 64 characters) and kind ("fleet" through the tunnel, "fleet-direct" without). Pending and blocked machines are only counted: anybody holding the shared enrolment token can register up to maxPending new serials. No address, key or serial number appears in a label.
//
// The WireGuard handshake is not known here (the hub's wg-metrics textfile has it as wireguard_peer_latest_handshake_seconds{kind="fleet"}); a machine joins it by name.
func (s *Service) Metrics(ctx context.Context) ([]byte, error) {
	peers, err := s.store.List(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now()
	stale := float64(s.cfg.ReenrollSeconds * 3)
	sort.SliceStable(peers, func(i, j int) bool { return peers[i].Name < peers[j].Name })

	var b bytes.Buffer
	gauge := func(name, help string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
	}
	boolean := func(v bool) float64 {
		if v {
			return 1
		}
		return 0
	}

	gauge("aiaas_fleet_enabled", "1 when the fleet accepts enrolments.")
	fmt.Fprintf(&b, "aiaas_fleet_enabled %g\n", boolean(s.cfg.Enabled))
	gauge("aiaas_fleet_reenroll_interval_seconds", "How often a machine enrols again; a machine is without report after three intervals.")
	fmt.Fprintf(&b, "aiaas_fleet_reenroll_interval_seconds %d\n", s.cfg.ReenrollSeconds)

	counts := map[string]int{"approved": 0, "pending": 0, "blocked": 0}
	var approved []Peer
	for _, p := range peers {
		switch {
		case p.Pending:
			counts["pending"]++
		case p.Blocked:
			counts["blocked"]++
		default:
			counts["approved"]++
			approved = append(approved, p)
		}
	}
	gauge("aiaas_fleet_machines", "Enrolled machines by state: approved, pending (awaiting a fleet admin) or blocked.")
	for _, st := range []string{"approved", "blocked", "pending"} {
		fmt.Fprintf(&b, "aiaas_fleet_machines{state=%q} %d\n", st, counts[st])
	}

	type series struct {
		name, help string
		value      func(p Peer) float64
	}
	perMachine := []series{
		{"aiaas_fleet_machine_last_seen_timestamp_seconds", "Unix time of the machine's last enrolment (0 = never since its approval).", func(p Peer) float64 {
			if p.LastSeen == nil {
				return 0
			}
			return float64(p.LastSeen.Unix())
		}},
		{"aiaas_fleet_machine_expected", "1 when the machine is meant to serve (its assignment is switched on); 0 when an admin or the inventory switched it off.", func(p Peer) float64 { return boolean(p.Enabled) }},
		{"aiaas_fleet_machine_busy", "1 while the machine's gate has stopped inference because someone works at it (wanted, no fault).", func(p Peer) float64 { return boolean(p.Busy) }},
		{"aiaas_fleet_machine_model_error", "1 when the machine reported that it could not pull or load its model.", func(p Peer) float64 { return boolean(p.ModelError != "") }},
		{"aiaas_fleet_machine_ready", "1 when the machine is meant to serve, is not busy, reported within three enrolment intervals, and serves its assigned model without error.", func(p Peer) float64 {
			fresh := p.LastSeen != nil && now.Sub(*p.LastSeen).Seconds() <= stale
			return boolean(p.Enabled && !p.Busy && fresh && serving(p))
		}},
	}
	for _, m := range perMachine {
		gauge(m.name, m.help)
		for _, p := range approved {
			fmt.Fprintf(&b, "%s{%s} %s\n", m.name, machineLabels(p), strconv.FormatFloat(m.value(p), 'f', -1, 64))
		}
	}
	return b.Bytes(), nil
}

// serving: the machine reports models, none failed, and — where it lets us control it — the assigned one is among them (the admin view's "erfüllt"). A machine managed by someone else only has to serve something.
func serving(p Peer) bool {
	models := p.Models()
	if len(models) == 0 || p.ModelError != "" {
		return false
	}
	return !p.Controls || slices.Contains(models, p.Model)
}

func machineLabels(p Peer) string {
	kind := "fleet"
	if p.Transport == "direct" {
		kind = "fleet-direct"
	}
	return `name="` + labelValue(p.Name) + `",site="` + labelValue(p.Location) + `",kind="` + kind + `"`
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

// labelValue escapes a label value as the text format wants it (backslash, double quote, line feed).
func labelValue(v string) string { return labelEscaper.Replace(v) }
