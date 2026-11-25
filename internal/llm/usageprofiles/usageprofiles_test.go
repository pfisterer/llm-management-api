package usageprofiles

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 23, 8, 0, 0, 0, time.UTC)

// course is n keys of the alias code, each with reqs requests a minute apart from 08:00 on, input in and output out tokens, each request 10 s long.
func course(n, reqs, in, out int, alias, ua string) []Row {
	var rows []Row
	for k := 0; k < n; k++ {
		for i := 0; i < reqs; i++ {
			s := t0.Add(time.Duration(i) * time.Minute)
			rows = append(rows, Row{Key: fmt.Sprintf("hash-%s-%d", alias, k), Alias: alias, Input: in, Output: out, Start: s, End: s.Add(10 * time.Second), UserAgent: ua})
		}
	}
	return rows
}

func TestComputeFigures(t *testing.T) {
	rows := course(6, 10, 13000, 200, "code", "Positron/2026.11 (darwin)")
	rep := Compute(rows, t0.Add(-time.Hour), t0.Add(24*time.Hour), 5, 4, t0)
	if len(rep.Models) != 1 || rep.Requests != 60 || rep.UserAgentCoverage != 1 {
		t.Fatalf("report = %+v", rep)
	}
	p := rep.Models[0]
	if p.Alias != "code" || p.Keys != 6 || p.KeyHours != 6 || p.Requests != 60 {
		t.Errorf("profile = %+v", p)
	}
	check := func(name string, s *Stat, median float64, samples int) {
		t.Helper()
		if s == nil || s.Median != median || s.Samples != samples || s.Keys != 6 {
			t.Errorf("%s = %+v, want median %v over %d", name, s, median, samples)
		}
	}
	check("input per request", p.InputTokensPerRequest, 13000, 60)
	check("output per request", p.OutputTokensPerRequest, 200, 60)
	check("tokens per key-hour", p.TokensPerKeyHour, 132000, 6)
	check("requests per key-hour", p.RequestsPerKeyHour, 10, 6)
	check("peak per minute", p.PeakTPMPerKey, 13200, 6)
	check("concurrency", p.ConcurrencyPerKey, 1, 6)
	check("tokens per 4-hour window", p.TokensPerKeyWindow, 132000, 6)
	if p.UserAgents["positron"] != 1 {
		t.Errorf("user agents = %v", p.UserAgents)
	}
	// Nothing names a key.
	raw, _ := json.Marshal(rep)
	if strings.Contains(string(raw), "hash-") {
		t.Errorf("a key left the package: %s", raw)
	}
}

func TestTooFewKeysAndRange(t *testing.T) {
	rows := append(course(4, 10, 1000, 100, "agent", ""), course(5, 2, 4000, 800, "chat-default", "")...)
	// A request outside the range does not count.
	rows = append(rows, Row{Key: "hash-x", Alias: "chat-default", Input: 1, Output: 1, Start: t0.Add(-48 * time.Hour)})
	rep := Compute(rows, t0.Add(-time.Hour), t0.Add(time.Hour), 5, 4, t0)
	if len(rep.Models) != 1 || rep.Models[0].Alias != "chat-default" || rep.Models[0].Requests != 10 {
		t.Fatalf("an alias with 4 keys is left out: %+v", rep.Models)
	}
	if rep.UserAgentCoverage != 0 || rep.Models[0].UserAgents != nil {
		t.Errorf("no user agents: %+v", rep)
	}
}

func TestPercentilesConcurrencyAndPeak(t *testing.T) {
	var rows []Row
	for k := 0; k < 5; k++ {
		key := fmt.Sprintf("k%d", k)
		// three requests at once, then one more in the same minute
		for i := 0; i < 3; i++ {
			rows = append(rows, Row{Key: key, Alias: "agent", Input: 1000 * (k + 1), Output: 0, Start: t0, End: t0.Add(30 * time.Second)})
		}
		rows = append(rows, Row{Key: key, Alias: "agent", Input: 1000 * (k + 1), Start: t0.Add(40 * time.Second), End: t0.Add(50 * time.Second)})
	}
	p := Compute(rows, t0.Add(-time.Hour), t0.Add(time.Hour), 5, 4, t0).Models[0]
	if p.ConcurrencyPerKey.Median != 3 {
		t.Errorf("concurrency = %+v", p.ConcurrencyPerKey)
	}
	// peaks 4000, 8000, …, 20000: median 12000, P90 between 16000 and 20000
	if p.PeakTPMPerKey.Median != 12000 || p.PeakTPMPerKey.P90 != 18400 {
		t.Errorf("peak = %+v", p.PeakTPMPerKey)
	}
}

func TestUserAgentKinds(t *testing.T) {
	cases := map[string]string{
		"": "", "Positron/2026.11": "positron", "Continue/1.2 VSCode": "vscode", "Mozilla/5.0 (X11)": "chat",
		"OpenAI/Python 1.55.0": "script", "curl/8.0": "script", "SomethingElse/1": "other",
	}
	for ua, want := range cases {
		if got := userAgentKind(ua); got != want {
			t.Errorf("userAgentKind(%q) = %q, want %q", ua, got, want)
		}
	}
}

type fakeSource struct {
	rows []Row
	err  error
	from time.Time
}

func (f *fakeSource) SpendRows(_ context.Context, from, _ time.Time) ([]Row, error) {
	f.from = from
	return f.rows, f.err
}

func TestServiceKeepsTheLastReport(t *testing.T) {
	src := &fakeSource{rows: course(5, 1, 10, 1, "code", "")}
	s := &Service{Source: src, Days: 14, MinKeys: 5, WindowHours: 4, Now: func() time.Time { return t0.Add(90 * time.Minute) }}
	if _, ok := s.Report(); ok {
		t.Fatal("no report before the first computation")
	}
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !src.from.Equal(t0.Add(time.Hour).AddDate(0, 0, -14)) {
		t.Errorf("from = %v", src.from)
	}
	src.err = errors.New("litellm down")
	if err := s.Refresh(context.Background()); err == nil {
		t.Fatal("the error is reported")
	}
	if rep, ok := s.Report(); !ok || len(rep.Models) != 1 {
		t.Errorf("the previous report stays: %+v", rep)
	}
}
