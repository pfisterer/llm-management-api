package promtext

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestWriter(t *testing.T) {
	var w Writer
	w.Header("x_total", "counter", "Help.")
	w.Sample("x_total", 1.5, "a", `q"u\o`+"\n", "b", "")
	w.Sample("y", 3)
	want := "# HELP x_total Help.\n# TYPE x_total counter\n" + `x_total{a="q\"u\\o\n",b=""} 1.5` + "\ny 3\n"
	if got := string(w.Bytes()); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestCounterVec(t *testing.T) {
	c := NewCounterVec("starts_total", "Starts.", "profile", "result")
	c.Init("gpu", "ok")
	c.Inc("cpu", "ok")
	c.Inc("cpu", "ok")
	var w Writer
	c.WriteTo(&w)
	want := "# HELP starts_total Starts.\n# TYPE starts_total counter\n" + `starts_total{profile="cpu",result="ok"} 2` + "\n" + `starts_total{profile="gpu",result="ok"} 0` + "\n"
	if got := string(w.Bytes()); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestHistogramVec(t *testing.T) {
	h := NewHistogramVec("d_seconds", "Durations.", []float64{1, 10}, "p")
	for _, v := range []float64{0.5, 1, 5, 50} {
		h.Observe(v, "x")
	}
	var w Writer
	h.WriteTo(&w)
	for _, want := range []string{"# TYPE d_seconds histogram\n", `d_seconds_bucket{p="x",le="1"} 2` + "\n", `d_seconds_bucket{p="x",le="10"} 3` + "\n", `d_seconds_bucket{p="x",le="+Inf"} 4` + "\n", `d_seconds_sum{p="x"} 56.5` + "\n", `d_seconds_count{p="x"} 4` + "\n"} {
		if !strings.Contains(string(w.Bytes()), want) {
			t.Errorf("missing %q in\n%s", want, w.Bytes())
		}
	}
}

func TestCache(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	calls, fail := 0, false
	c := NewCache(30*time.Second, func(context.Context) ([]byte, error) {
		calls++
		if fail {
			return nil, errors.New("down")
		}
		return []byte("v"), nil
	})
	c.now = func() time.Time { return now }
	ctx := context.Background()
	_, _ = c.Get(ctx)
	_, _ = c.Get(ctx)
	if calls != 1 {
		t.Fatalf("cached: %d calls", calls)
	}
	now = now.Add(31 * time.Second)
	fail = true
	if _, err := c.Get(ctx); err == nil || calls != 2 {
		t.Fatalf("expired and failing: %v %d", err, calls)
	}
	// An error is not kept: the next call tries again.
	fail = false
	if b, err := c.Get(ctx); err != nil || string(b) != "v" || calls != 3 {
		t.Fatalf("after the error: %q %v %d", b, err, calls)
	}
}
