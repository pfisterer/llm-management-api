package fleet

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestMetrics(t *testing.T) {
	ctx := context.Background()
	svc, st, _ := testService(t, nil)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }
	ago := func(d time.Duration) *time.Time { t := now.Add(-d); return &t }
	peers := []Peer{
		// Ready: approved, switched on, recent, serves its model.
		{Serial: "S0001", Name: "wimac01", Location: "Raum 1.23", Transport: "wireguard", PublicKey: key1, Address: "10.90.4.1", Controls: true, Model: "big", ModelsList: "big small", Enabled: true, LastSeen: ago(30 * time.Minute)},
		// Silent for more than three enrolment intervals.
		{Serial: "S0002", Name: "wimac02", Location: `Lab "B"`, Transport: "wireguard", PublicKey: key2, Address: "10.90.4.2", Controls: true, Model: "big", ModelsList: "big", Enabled: true, LastSeen: ago(4 * time.Hour)},
		// Busy, and could not load its model.
		{Serial: "S0003", Name: "wimac03", Transport: "wireguard", Controls: true, Model: "big", ModelsList: "small", ModelError: "pull failed", Enabled: true, Busy: true, LastSeen: ago(time.Minute)},
		// Direct, managed elsewhere, switched off by an admin.
		{Serial: "S0004", Name: "gpu-box", Transport: "direct", APIBase: "https://gpu.example", APIKey: "secret-api-key", Controls: false, ModelsList: "llama", Enabled: false, LastSeen: ago(time.Minute)},
		// Approved but not enrolled since.
		{Serial: "S0005", Name: "wimac05", Transport: "wireguard", Enabled: true},
		// Pending and blocked: only counted.
		{Serial: "S0006", Name: "stranger", Pending: true, Blocked: true, PublicKey: key0},
		{Serial: "S0007", Name: "retired", Blocked: true},
	}
	if err := st.Update(ctx, func([]Peer) ([]Peer, []string, error) { return peers, nil, nil }); err != nil {
		t.Fatal(err)
	}
	raw, err := svc.Metrics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)
	for _, want := range []string{
		"aiaas_fleet_enabled 1\n",
		"aiaas_fleet_reenroll_interval_seconds 3600\n",
		`aiaas_fleet_machines{state="approved"} 5` + "\n",
		`aiaas_fleet_machines{state="pending"} 1` + "\n",
		`aiaas_fleet_machines{state="blocked"} 1` + "\n",
		`aiaas_fleet_machine_ready{name="wimac01",site="Raum 1.23",kind="fleet"} 1` + "\n",
		`aiaas_fleet_machine_ready{name="wimac02",site="Lab \"B\"",kind="fleet"} 0` + "\n",
		`aiaas_fleet_machine_ready{name="wimac03",site="",kind="fleet"} 0` + "\n",
		`aiaas_fleet_machine_ready{name="gpu-box",site="",kind="fleet-direct"} 0` + "\n",
		`aiaas_fleet_machine_ready{name="wimac05",site="",kind="fleet"} 0` + "\n",
		`aiaas_fleet_machine_last_seen_timestamp_seconds{name="wimac01",site="Raum 1.23",kind="fleet"} ` + "1792236600\n",
		`aiaas_fleet_machine_last_seen_timestamp_seconds{name="wimac05",site="",kind="fleet"} 0` + "\n",
		`aiaas_fleet_machine_busy{name="wimac03",site="",kind="fleet"} 1` + "\n",
		`aiaas_fleet_machine_model_error{name="wimac03",site="",kind="fleet"} 1` + "\n",
		`aiaas_fleet_machine_expected{name="gpu-box",site="",kind="fleet-direct"} 0` + "\n",
		"# TYPE aiaas_fleet_machine_ready gauge\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	// Pending and blocked machines get no series of their own, and nothing secret or addressable leaks into a label.
	for _, never := range []string{"stranger", "retired", "S000", "10.90.", key1, key2, key0, "secret-api-key", "gpu.example", "pull failed"} {
		if strings.Contains(out, never) {
			t.Errorf("%q must not appear in the metrics:\n%s", never, out)
		}
	}
	// One series per approved machine and metric.
	if n := strings.Count(out, "aiaas_fleet_machine_ready{"); n != 5 {
		t.Errorf("ready series: %d, want 5", n)
	}
}

func TestMetricsLabelEscaping(t *testing.T) {
	if got := machineLabels(Peer{Name: "a", Location: "x\\y\"z\nw"}); got != `name="a",site="x\\y\"z\nw",kind="fleet"` {
		t.Fatalf("labels: %s", got)
	}
}

func TestMetricsEmpty(t *testing.T) {
	svc, _, _ := testService(t, func(c *Config) { c.Enabled = false })
	raw, err := svc.Metrics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)
	if !strings.Contains(out, "aiaas_fleet_enabled 0\n") || !strings.Contains(out, `aiaas_fleet_machines{state="approved"} 0`) {
		t.Fatalf("empty fleet:\n%s", out)
	}
	if !strings.Contains(out, "# TYPE aiaas_fleet_machine_ready gauge\n") || strings.Contains(out, "aiaas_fleet_machine_ready{") {
		t.Fatalf("no machine series expected:\n%s", out)
	}
}
