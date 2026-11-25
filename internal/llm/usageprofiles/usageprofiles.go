// Package usageprofiles computes how the LLM gateway is used, per model alias, from LiteLLM's spend logs: tokens per request, per active key-hour and per key in a 4-hour window, requests per key-hour, the busiest minute and the requests at once of an active key. Only aggregates leave the package, and only those over enough distinct keys (MinKeys): nothing per person, key, team or project. The resource estimator of openstack-management-api reads them (GET /v1/usage-profiles) to replace its assumed profiles where enough was measured.
package usageprofiles

import (
	"context"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Row is one request of the spend logs, as far as the profiles need it. Key is LiteLLM's hashed key; it never leaves the package.
type Row struct {
	Key       string
	Alias     string
	Input     int
	Output    int
	Start     time.Time
	End       time.Time
	UserAgent string
}

// Stat is a distribution: its median and 90th percentile, over Samples values from Keys distinct keys.
type Stat struct {
	Median  float64 `json:"median"`
	P90     float64 `json:"p90"`
	Samples int     `json:"samples"`
	Keys    int     `json:"keys"`
}

// Profile is what was measured of one model alias. A figure is missing when fewer than MinKeys keys stand behind it.
type Profile struct {
	Alias    string `json:"alias" example:"code"`
	Requests int    `json:"requests"`
	Keys     int    `json:"keys"`
	KeyHours int    `json:"key_hours"`
	// Per request.
	InputTokensPerRequest  *Stat `json:"input_tokens_per_request,omitempty"`
	OutputTokensPerRequest *Stat `json:"output_tokens_per_request,omitempty"`
	// Per hour in which a key sent at least one request.
	TokensPerKeyHour   *Stat `json:"tokens_per_key_hour,omitempty"`
	RequestsPerKeyHour *Stat `json:"requests_per_key_hour,omitempty"`
	// The busiest minute (input and output tokens) and the most requests running at once, of a key in an active hour.
	PeakTPMPerKey     *Stat `json:"peak_tpm_per_key,omitempty"`
	ConcurrencyPerKey *Stat `json:"concurrency_per_key,omitempty"`
	// Tokens of a key in a 4-hour window it was active in (the key budget's window).
	TokensPerKeyWindow *Stat `json:"tokens_per_key_window,omitempty"`
	// UserAgents are the shares of the requests by kind of client, where the spend logs name the user agent; only kinds used by at least MinKeys keys.
	UserAgents map[string]float64 `json:"user_agents,omitempty"`
}

// Report is the profiles of one time range.
type Report struct {
	From       time.Time `json:"from"`
	To         time.Time `json:"to"`
	ComputedAt time.Time `json:"computed_at"`
	MinKeys    int       `json:"min_keys"`
	// WindowHours is the window of TokensPerKeyWindow.
	WindowHours int `json:"window_hours"`
	// Requests is every request counted; UserAgentCoverage the share of them whose user agent the spend logs carry.
	Requests          int       `json:"requests"`
	UserAgentCoverage float64   `json:"user_agent_coverage"`
	Models            []Profile `json:"models"`
}

// Compute builds the report of rows in [from, to). Aliases with fewer than minKeys keys are left out, and so is any figure with fewer.
func Compute(rows []Row, from, to time.Time, minKeys, windowHours int, now time.Time) Report {
	if minKeys < 1 {
		minKeys = 1
	}
	if windowHours < 1 {
		windowHours = 4
	}
	rep := Report{From: from, To: to, ComputedAt: now, MinKeys: minKeys, WindowHours: windowHours, Models: []Profile{}}
	byAlias := map[string][]Row{}
	withUA := 0
	for _, r := range rows {
		if r.Key == "" || r.Alias == "" || r.Start.Before(from) || !r.Start.Before(to) {
			continue
		}
		rep.Requests++
		if r.UserAgent != "" {
			withUA++
		}
		byAlias[r.Alias] = append(byAlias[r.Alias], r)
	}
	if rep.Requests > 0 {
		rep.UserAgentCoverage = round(float64(withUA)/float64(rep.Requests), 3)
	}
	for _, alias := range sortedKeys(byAlias) {
		if p, ok := profile(alias, byAlias[alias], minKeys, windowHours); ok {
			rep.Models = append(rep.Models, p)
		}
	}
	return rep
}

type bucket struct {
	key  string
	slot int64
}

func profile(alias string, rows []Row, minKeys, windowHours int) (Profile, bool) {
	keys := map[string]bool{}
	for _, r := range rows {
		keys[r.Key] = true
	}
	if len(keys) < minKeys {
		return Profile{}, false
	}
	p := Profile{Alias: alias, Requests: len(rows), Keys: len(keys)}
	in := sample{}
	out := sample{}
	hourTokens := map[bucket]float64{}
	hourRequests := map[bucket]float64{}
	minuteTokens := map[bucket]float64{}
	windowTokens := map[bucket]float64{}
	byHour := map[bucket][]Row{}
	uaKeys := map[string]map[string]bool{}
	uaCount := map[string]int{}
	for _, r := range rows {
		in.add(r.Key, float64(r.Input))
		out.add(r.Key, float64(r.Output))
		h := bucket{r.Key, r.Start.Unix() / 3600}
		tok := float64(r.Input + r.Output)
		hourTokens[h] += tok
		hourRequests[h]++
		byHour[h] = append(byHour[h], r)
		minuteTokens[bucket{r.Key, r.Start.Unix() / 60}] += tok
		windowTokens[bucket{r.Key, r.Start.Unix() / int64(3600*windowHours)}] += tok
		if kind := userAgentKind(r.UserAgent); kind != "" {
			if uaKeys[kind] == nil {
				uaKeys[kind] = map[string]bool{}
			}
			uaKeys[kind][r.Key] = true
			uaCount[kind]++
		}
	}
	p.KeyHours = len(hourTokens)
	perHour, reqHour, peak, conc, window := sample{}, sample{}, sample{}, sample{}, sample{}
	for h, v := range hourTokens {
		perHour.add(h.key, v)
		reqHour.add(h.key, hourRequests[h])
	}
	peakOfHour := map[bucket]float64{}
	for m, v := range minuteTokens {
		h := bucket{m.key, m.slot / 60}
		peakOfHour[h] = max(peakOfHour[h], v)
	}
	for h, rs := range byHour {
		peak.add(h.key, peakOfHour[h])
		conc.add(h.key, float64(maxOverlap(rs)))
	}
	for w, v := range windowTokens {
		window.add(w.key, v)
	}
	p.InputTokensPerRequest = in.stat(minKeys)
	p.OutputTokensPerRequest = out.stat(minKeys)
	p.TokensPerKeyHour = perHour.stat(minKeys)
	p.RequestsPerKeyHour = reqHour.stat(minKeys)
	p.PeakTPMPerKey = peak.stat(minKeys)
	p.ConcurrencyPerKey = conc.stat(minKeys)
	p.TokensPerKeyWindow = window.stat(minKeys)
	for kind, ks := range uaKeys {
		if len(ks) >= minKeys {
			if p.UserAgents == nil {
				p.UserAgents = map[string]float64{}
			}
			p.UserAgents[kind] = round(float64(uaCount[kind])/float64(len(rows)), 3)
		}
	}
	return p, true
}

// maxOverlap is the most requests running at the same time: a request runs from Start to End (End missing: an instant).
func maxOverlap(rs []Row) int {
	type ev struct {
		t     time.Time
		delta int
	}
	evs := make([]ev, 0, 2*len(rs))
	for _, r := range rs {
		end := r.End
		if end.Before(r.Start) {
			end = r.Start
		}
		evs = append(evs, ev{r.Start, 1}, ev{end, -1})
	}
	// At the same instant an end comes before a start: back-to-back requests do not overlap.
	sort.Slice(evs, func(i, j int) bool {
		if evs[i].t.Equal(evs[j].t) {
			return evs[i].delta < evs[j].delta
		}
		return evs[i].t.Before(evs[j].t)
	})
	cur, top := 0, 0
	for _, e := range evs {
		cur += e.delta
		top = max(top, cur)
	}
	return max(top, 1)
}

// userAgentKind is a coarse kind of client from a user agent, never the string itself: Positron, VS Code (with Continue, Cline and the like), a browser chat, the OpenAI SDKs, other.
func userAgentKind(ua string) string {
	ua = strings.ToLower(strings.TrimSpace(ua))
	switch {
	case ua == "":
		return ""
	case strings.Contains(ua, "positron"):
		return "positron"
	case strings.Contains(ua, "vscode"), strings.Contains(ua, "continue"), strings.Contains(ua, "cline"), strings.Contains(ua, "roo"), strings.Contains(ua, "copilot"):
		return "vscode"
	case strings.Contains(ua, "librechat"), strings.Contains(ua, "mozilla"):
		return "chat"
	case strings.Contains(ua, "openai/"), strings.Contains(ua, "openai-python"), strings.Contains(ua, "python"), strings.Contains(ua, "node"), strings.Contains(ua, "curl"), strings.Contains(ua, "go-http"):
		return "script"
	}
	return "other"
}

// sample collects values with the key each came from.
type sample struct {
	values []float64
	keys   map[string]bool
}

func (s *sample) add(key string, v float64) {
	if s.keys == nil {
		s.keys = map[string]bool{}
	}
	s.values = append(s.values, v)
	s.keys[key] = true
}

// stat is the distribution, nil when fewer than minKeys keys stand behind it.
func (s *sample) stat(minKeys int) *Stat {
	if len(s.keys) < minKeys || len(s.values) == 0 {
		return nil
	}
	v := slices.Clone(s.values)
	slices.Sort(v)
	return &Stat{Median: round(quantile(v, 0.5), 1), P90: round(quantile(v, 0.9), 1), Samples: len(v), Keys: len(s.keys)}
}

// quantile of sorted values, linearly interpolated.
func quantile(v []float64, q float64) float64 {
	if len(v) == 1 {
		return v[0]
	}
	pos := q * float64(len(v)-1)
	lo := int(math.Floor(pos))
	hi := min(lo+1, len(v)-1)
	return v[lo] + (v[hi]-v[lo])*(pos-float64(lo))
}

func round(v float64, places int) float64 {
	f := math.Pow(10, float64(places))
	return math.Round(v*f) / f
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Source reads the spend logs of a time range.
type Source interface {
	SpendRows(ctx context.Context, from, to time.Time) ([]Row, error)
}

// Service keeps the last report and computes a new one on Refresh (nightly).
type Service struct {
	Source      Source
	Days        int
	MinKeys     int
	WindowHours int
	Now         func() time.Time

	mu   sync.RWMutex
	last *Report
}

// Refresh reads the last Days days and replaces the report; on an error the previous one stays.
func (s *Service) Refresh(ctx context.Context) error {
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	to := now().UTC().Truncate(time.Hour)
	from := to.AddDate(0, 0, -max(s.Days, 1))
	rows, err := s.Source.SpendRows(ctx, from, to)
	if err != nil {
		return err
	}
	rep := Compute(rows, from, to, s.MinKeys, s.WindowHours, now().UTC())
	s.mu.Lock()
	s.last = &rep
	s.mu.Unlock()
	return nil
}

// Report is the last report; false before the first one.
func (s *Service) Report() (Report, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.last == nil {
		return Report{}, false
	}
	return *s.last, true
}
