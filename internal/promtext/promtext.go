// Package promtext writes the Prometheus text exposition format (0.0.4) by hand, for the service's /metrics: a few gauges computed from the database, two counters and one histogram kept in memory need neither client_golang's registry nor its dependency.
//
// Counters and histograms live in the process: a restart sets them back to zero, which Prometheus' rate() and increase() treat as a counter reset.
package promtext

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Writer collects one page.
type Writer struct{ b bytes.Buffer }

// Bytes is the page so far.
func (w *Writer) Bytes() []byte { return w.b.Bytes() }

// Write appends a part written elsewhere (another Writer's Bytes).
func (w *Writer) Write(p []byte) { w.b.Write(p) }

// Header announces a family: its help text and type (gauge, counter, histogram).
func (w *Writer) Header(name, typ, help string) {
	fmt.Fprintf(&w.b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

// Sample writes one series. labels are name/value pairs in the order they are written.
func (w *Writer) Sample(name string, v float64, labels ...string) {
	w.b.WriteString(name)
	if len(labels) > 0 {
		w.b.WriteByte('{')
		for i := 0; i+1 < len(labels); i += 2 {
			if i > 0 {
				w.b.WriteByte(',')
			}
			w.b.WriteString(labels[i])
			w.b.WriteString(`="`)
			w.b.WriteString(LabelValue(labels[i+1]))
			w.b.WriteByte('"')
		}
		w.b.WriteByte('}')
	}
	w.b.WriteByte(' ')
	w.b.WriteString(Float(v))
	w.b.WriteByte('\n')
}

// Float formats a value the way the text format reads it.
func Float(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

// LabelValue escapes a label value as the text format wants it (backslash, double quote, line feed).
func LabelValue(v string) string { return labelEscaper.Replace(v) }

// CounterVec is a counter per combination of label values, kept in memory.
type CounterVec struct {
	name, help string
	labels     []string
	mu         sync.Mutex
	values     map[string]float64
}

// NewCounterVec makes a counter family with fixed label names.
func NewCounterVec(name, help string, labels ...string) *CounterVec {
	return &CounterVec{name: name, help: help, labels: labels, values: map[string]float64{}}
}

// Inc adds one to the series of these label values (in the order of the label names).
func (c *CounterVec) Inc(values ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.values[strings.Join(values, "\x00")]++
}

// Value is the count of one series (for tests).
func (c *CounterVec) Value(values ...string) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.values[strings.Join(values, "\x00")]
}

// WriteTo writes the family, every series seen so far (or made with Init).
func (c *CounterVec) WriteTo(w *Writer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	w.Header(c.name, "counter", c.help)
	for _, k := range slices.Sorted(maps.Keys(c.values)) {
		w.Sample(c.name, c.values[k], pairs(c.labels, strings.Split(k, "\x00"))...)
	}
}

// Init makes the series of these label values exist (at 0), so rate() sees the first event.
func (c *CounterVec) Init(values ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := strings.Join(values, "\x00")
	if _, ok := c.values[k]; !ok {
		c.values[k] = 0
	}
}

// HistogramVec is a histogram per combination of label values, kept in memory.
type HistogramVec struct {
	name, help string
	labels     []string
	buckets    []float64
	mu         sync.Mutex
	series     map[string]*histogram
}

type histogram struct {
	counts []uint64 // per bucket, not cumulative
	count  uint64
	sum    float64
}

// NewHistogramVec makes a histogram family with fixed label names and upper bounds (ascending, without +Inf).
func NewHistogramVec(name, help string, buckets []float64, labels ...string) *HistogramVec {
	return &HistogramVec{name: name, help: help, labels: labels, buckets: buckets, series: map[string]*histogram{}}
}

// Observe records one value for these label values.
func (h *HistogramVec) Observe(v float64, values ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	k := strings.Join(values, "\x00")
	s := h.series[k]
	if s == nil {
		s = &histogram{counts: make([]uint64, len(h.buckets))}
		h.series[k] = s
	}
	for i, b := range h.buckets {
		if v <= b {
			s.counts[i]++
			break
		}
	}
	s.count++
	s.sum += v
}

// WriteTo writes the family: cumulative buckets, +Inf, sum and count per series.
func (h *HistogramVec) WriteTo(w *Writer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	w.Header(h.name, "histogram", h.help)
	for _, k := range slices.Sorted(maps.Keys(h.series)) {
		s := h.series[k]
		labels := pairs(h.labels, strings.Split(k, "\x00"))
		var cum uint64
		for i, b := range h.buckets {
			cum += s.counts[i]
			w.Sample(h.name+"_bucket", float64(cum), append(slices.Clone(labels), "le", Float(b))...)
		}
		w.Sample(h.name+"_bucket", float64(s.count), append(slices.Clone(labels), "le", "+Inf")...)
		w.Sample(h.name+"_sum", s.sum, labels...)
		w.Sample(h.name+"_count", float64(s.count), labels...)
	}
}

func pairs(names, values []string) []string {
	out := make([]string, 0, 2*len(names))
	for i, n := range names {
		v := ""
		if i < len(values) {
			v = values[i]
		}
		out = append(out, n, v)
	}
	return out
}

// Cache keeps a part computed from the database or the cluster for a while, so a scrape (or several at once) does not read them every time. An error is not kept: the next scrape tries again.
type Cache struct {
	ttl    time.Duration
	render func(ctx context.Context) ([]byte, error)
	now    func() time.Time
	mu     sync.Mutex
	at     time.Time
	body   []byte
}

// NewCache wraps render with a cache of ttl.
func NewCache(ttl time.Duration, render func(ctx context.Context) ([]byte, error)) *Cache {
	return &Cache{ttl: ttl, render: render, now: time.Now}
}

// Get is the cached page, rendered again once it is older than the ttl. Concurrent scrapes wait for one rendering.
func (c *Cache) Get(ctx context.Context) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.body != nil && c.now().Sub(c.at) < c.ttl {
		return c.body, nil
	}
	body, err := c.render(ctx)
	if err != nil {
		return nil, err
	}
	c.body, c.at = body, c.now()
	return body, nil
}
