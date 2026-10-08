// Package classify decides which alias (chat-default, chat-fast, code) a model
// is offered under — on evidence, not on its name.
//
// Which model sits behind which alias used to be a hand-kept list. With a
// hundred machines at several sites it drifts: a site loads a new model, nobody
// adds it, and it is reachable under its raw name only. So every unknown model
// is measured once (throughput, three coding tasks, a tool call) through the
// gateway, and the measurements decide by fixed thresholds. A name does not
// change its meaning, so the result is stored.
//
// Two levels, in this order:
//  1. `members` in the inventory (modelAliases) — fixed, never overruled.
//  2. The measurements.
//
// A failed measurement is NOT stored: a cold model or a busy machine is tried
// again by the next discovery run instead of staying unassignable.
package classify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.uber.org/zap"
)

// AliasDef is one entry of the inventory's modelAliases.
type AliasDef struct {
	Members []string `json:"members"`
}

// Thresholds are an operating decision ("from when is a model fast"), so they
// live in the inventory (aliasProbes), not in the code.
type Thresholds struct {
	FastTokensPerSecond float64 `json:"fastTokensPerSecond"`
	CodeTasksRequired   int     `json:"codeTasksRequired"`
	RequireToolsForChat *bool   `json:"requireToolsForChat"`
}

func (t Thresholds) fast() float64 {
	if t.FastTokensPerSecond > 0 {
		return t.FastTokensPerSecond
	}
	return 60
}

func (t Thresholds) code() int {
	if t.CodeTasksRequired > 0 {
		return t.CodeTasksRequired
	}
	return 3
}

func (t Thresholds) tools() bool { return t.RequireToolsForChat == nil || *t.RequireToolsForChat }

// Stamp fingerprints the thresholds. When the operator changes them, stored
// results no longer count — or a new threshold would never reach models that
// were already assessed. Same format as the former Node broker, so its cache
// carries over.
func (t Thresholds) Stamp() string {
	return fmt.Sprintf("%g/%d/%t", t.fast(), t.code(), t.tools())
}

// Probes are the measurements behind a result.
type Probes struct {
	TPS       int  `json:"tps"`
	CodeOK    int  `json:"codeOk"`
	CodeTotal int  `json:"codeTotal"`
	Tools     bool `json:"tools"`
}

// Result is the assignment of one model, with its reasons.
type Result struct {
	Aliases []string `json:"aliases"`
	Reason  string   `json:"reason"`
	Source  string   `json:"source"` // inventar | gemessen | nicht messbar
	Probes  *Probes  `json:"probes,omitempty"`
	Stamp   string   `json:"stamp,omitempty"`
	At      string   `json:"at,omitempty"`
}

// Store keeps measured results per model.
type Store interface {
	Get(ctx context.Context, model string) (Result, bool, error)
	Put(ctx context.Context, model string, r Result) error
}

// Service classifies models.
type Service struct {
	aliases    map[string]AliasDef
	thresholds Thresholds
	store      Store
	gateway    string
	key        string
	http       *http.Client
	log        *zap.SugaredLogger
}

func NewService(aliases map[string]AliasDef, t Thresholds, store Store, gatewayURL, masterKey string, log *zap.SugaredLogger) *Service {
	return &Service{aliases: aliases, thresholds: t, store: store, gateway: strings.TrimRight(gatewayURL, "/"),
		key: masterKey, http: &http.Client{}, log: log}
}

// Many classifies models one after another (each measurement loads a machine)
// and returns their aliases. A model that fails gets none, for now.
func (s *Service) Many(ctx context.Context, models []string) map[string][]string {
	out := map[string][]string{}
	for _, m := range models {
		r, err := s.One(ctx, m)
		if err != nil {
			s.log.Errorw("classify failed", "model", m, "error", err)
			out[m] = []string{}
			continue
		}
		out[m] = r.Aliases
	}
	return out
}

// One returns the assignment of one model, measuring it if necessary.
func (s *Service) One(ctx context.Context, model string) (Result, error) {
	var fixed []string
	for name, def := range s.aliases {
		if slices.Contains(def.Members, model) {
			fixed = append(fixed, name)
		}
	}
	if len(fixed) > 0 {
		slices.Sort(fixed)
		return Result{Aliases: fixed, Source: "inventar", Reason: "fest eingetragen"}, nil
	}
	stamp := s.thresholds.Stamp()
	if cached, ok, err := s.store.Get(ctx, model); err != nil {
		return Result{}, err
	} else if ok && cached.Stamp == stamp {
		return cached, nil
	}

	probe := s.probe(ctx, model)
	r := Result{Aliases: s.aliasesFrom(probe), Reason: s.reason(probe), Stamp: stamp,
		At: time.Now().UTC().Format("2006-01-02T15:04:05+00:00")}
	if probe.ok {
		r.Source = "gemessen"
		r.Probes = &Probes{TPS: probe.tps, CodeOK: probe.codeOK, CodeTotal: len(codeTasks), Tools: probe.tools}
	} else {
		r.Source = "nicht messbar"
	}
	s.log.Infow("classified", "model", model, "aliases", r.Aliases, "reason", r.Reason)
	if probe.ok {
		if err := s.store.Put(ctx, model, r); err != nil {
			return r, err
		}
	}
	return r, nil
}

// aliasesFrom turns measurements into aliases — a table, not a second model.
//
// The aliases are PROMISES, and admission checks them:
//
//	chat-default / chat-fast   chat, and tool calls work
//	code                       good enough for programming tasks
//
// Tool calls are a condition for the chat aliases rather than an alias of their
// own: otherwise the router could put a request on a model without tools, and
// the caller's application would break in a way that looks like its own bug.
// Fast and default exclude each other: chat-fast promises throughput, which
// means smaller models; in chat-default they would cost the default exactly
// what it is for. Only aliases that exist in the inventory are handed out.
func (s *Service) aliasesFrom(p probe) []string {
	if !p.ok {
		return []string{}
	}
	var out []string
	if !s.thresholds.tools() || p.tools {
		if float64(p.tps) >= s.thresholds.fast() {
			out = append(out, "chat-fast")
		} else {
			out = append(out, "chat-default")
		}
	}
	if p.codeOK >= s.thresholds.code() {
		out = append(out, "code")
	}
	known := []string{}
	for _, a := range out {
		if _, ok := s.aliases[a]; ok {
			known = append(known, a)
		}
	}
	return known
}

func (s *Service) reason(p probe) string {
	if !p.ok {
		return "nicht messbar: " + p.err
	}
	tools := "keine Werkzeugaufrufe"
	if p.tools {
		tools = "Werkzeugaufrufe ok"
	}
	return fmt.Sprintf("%d Tokens/s (schnell ab %g), Code %d/%d (noetig %d), %s",
		p.tps, s.thresholds.fast(), p.codeOK, len(codeTasks), s.thresholds.code(), tools)
}

// Three tasks whose answer a machine can check. Not about quality, but about
// "does code come out at all" — exactly what tells a coding model from a chat
// model that answers in prose.
var codeTasks = []struct {
	prompt string
	expect *regexp.Regexp
}{
	{"Schreibe eine Python-Funktion is_even(n), die True zurueckgibt, wenn n gerade ist. Nur Code.", regexp.MustCompile(`def\s+is_even`)},
	{"Schreibe eine JavaScript-Funktion sum(a, b), die die Summe zurueckgibt. Nur Code.", regexp.MustCompile(`function\s+sum|const\s+sum\s*=|sum\s*=\s*\(`)},
	{"Schreibe eine SQL-Abfrage, die alle Zeilen der Tabelle kunden liefert. Nur Code.", regexp.MustCompile(`(?i)select\s+\*`)},
}

type probe struct {
	ok     bool
	err    string
	tps    int
	codeOK int
	tools  bool
}

type completion struct {
	Choices []struct {
		Message struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func (s *Service) ask(ctx context.Context, body map[string]any, timeout time.Duration) (completion, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.gateway+"/v1/chat/completions", bytes.NewReader(b))
	if err != nil {
		return completion{}, err
	}
	req.Header.Set("Authorization", "Bearer "+s.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return completion{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		msg := string(raw)
		if len(msg) > 120 {
			msg = msg[:120]
		}
		return completion{}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, msg)
	}
	var c completion
	return c, json.Unmarshal(raw, &c)
}

func msg(text string) []map[string]string {
	return []map[string]string{{"role": "user", "content": text}}
}

// probe measures a model with calls through the gateway, i.e. against exactly
// the machine that serves it. Generous time limits: a cold model first loads
// several gigabytes.
func (s *Service) probe(ctx context.Context, model string) probe {
	// FIRST CALL: reachability and warm-up, deliberately not measured. Up to
	// three attempts, because the discovery run creates the model right before,
	// and the gateway replicas reload their model list only every 30 seconds.
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if _, err = s.ask(ctx, map[string]any{"model": model, "messages": msg("Antworte mit dem Wort: bereit"), "max_tokens": 8}, 4*time.Minute); err == nil {
			break
		}
		if attempt < 2 {
			select {
			case <-ctx.Done():
				return probe{err: ctx.Err().Error()}
			case <-time.After(20 * time.Second):
			}
		}
	}
	if err != nil {
		return probe{err: err.Error()}
	}

	p := probe{ok: true}
	for _, task := range codeTasks {
		c, err := s.ask(ctx, map[string]any{"model": model, "messages": msg(task.prompt), "max_tokens": 200}, 2*time.Minute)
		if err == nil && len(c.Choices) > 0 && task.expect.MatchString(c.Choices[0].Message.Content) {
			p.codeOK++
		}
	}

	// Throughput LAST, when the model is warm. Measured first it included the
	// loading time: the same two models gave 16 and 162, then 45 and 157
	// tokens/s — a stable ranking, unstable absolute values, and the absolute
	// value is what decides at the threshold.
	started := time.Now()
	c, err := s.ask(ctx, map[string]any{"model": model,
		"messages": msg("Zaehle von 1 bis 40, nur die Zahlen, durch Komma getrennt."), "max_tokens": 120}, 4*time.Minute)
	if secs := time.Since(started).Seconds(); err == nil && c.Usage.CompletionTokens > 0 && secs > 0 {
		p.tps = int(float64(c.Usage.CompletionTokens)/secs + 0.5)
	}

	p.tools = s.probeTools(ctx, model)
	return p
}

// probeTools: can the model call tools — really, not according to its model
// card? Checked to the end: right tool name, arguments readable as JSON, the
// required field present with the right type. A JSON parser is a harder judge
// than any second LLM.
func (s *Service) probeTools(ctx context.Context, model string) bool {
	c, err := s.ask(ctx, map[string]any{
		"model":    model,
		"messages": msg("Wie ist das Wetter in Mannheim? Nutze das Werkzeug."),
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{
			"name": "wetter", "description": "Liefert das aktuelle Wetter fuer einen Ort.",
			"parameters": map[string]any{"type": "object",
				"properties": map[string]any{"ort": map[string]any{"type": "string", "description": "Name des Ortes"}},
				"required":   []string{"ort"}},
		}}},
		"tool_choice": "auto", "max_tokens": 200,
	}, 2*time.Minute)
	if err != nil || len(c.Choices) == 0 || len(c.Choices[0].Message.ToolCalls) == 0 {
		return false
	}
	call := c.Choices[0].Message.ToolCalls[0].Function
	if call.Name != "wetter" {
		return false
	}
	var args map[string]any
	if json.Unmarshal([]byte(call.Arguments), &args) != nil {
		// Exactly the case this is about: a call, but arguments that are not
		// valid JSON — an error in the application nobody would trace to the model.
		return false
	}
	_, ok := args["ort"].(string)
	return ok
}
