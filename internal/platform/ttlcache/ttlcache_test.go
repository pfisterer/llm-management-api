package ttlcache

import (
	"context"
	"errors"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func TestMap(t *testing.T) {
	c := &clock{time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
	m := NewMap[string, int](Options{TTL: time.Minute})
	m.Now = c.now
	m.Set("a", 1)
	if v, ok := m.Get("a"); !ok || v != 1 {
		t.Fatalf("fresh: %v %v", v, ok)
	}
	c.t = c.t.Add(time.Minute)
	if _, ok := m.Get("a"); ok {
		t.Fatal("served after its TTL")
	}
	if v, ok := m.Stale("a"); !ok || v != 1 {
		t.Fatal("stale value gone before a sweep")
	}
	m.Set("b", 2) // sweeps: a expired
	if _, ok := m.Stale("a"); ok || m.Len() != 1 {
		t.Fatalf("expired value not swept: %d", m.Len())
	}
	m.Delete("b")
	if _, ok := m.Get("b"); ok {
		t.Fatal("deleted value served")
	}
}

func TestMapKeepStaleAndMaxLen(t *testing.T) {
	c := &clock{time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
	m := NewMap[string, string](Options{TTL: time.Minute, KeepStale: -1})
	m.Now = c.now
	m.Set("p", "kurs")
	c.t = c.t.Add(24 * time.Hour)
	m.Set("q", "x")
	if v, ok := m.Stale("p"); !ok || v != "kurs" {
		t.Fatal("KeepStale < 0 must keep expired values")
	}
	small := NewMap[int, int](Options{TTL: time.Hour, MaxLen: 2})
	small.Set(1, 1)
	small.Set(2, 2)
	small.Set(2, 3) // a known key does not count
	if small.Len() != 2 {
		t.Fatalf("len %d", small.Len())
	}
	small.Set(3, 3)
	if small.Len() != 1 {
		t.Fatalf("MaxLen: len %d, want the map emptied before the new key", small.Len())
	}
}

func TestValue(t *testing.T) {
	c := &clock{time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
	ctx := context.Background()
	loads, fail := 0, false
	load := func(context.Context) (string, error) {
		loads++
		if fail {
			return "", errors.New("down")
		}
		return "v", nil
	}
	v := &Value[string]{TTL: time.Minute, Now: c.now}
	if _, err := v.Get(ctx, load); err != nil {
		t.Fatal(err)
	}
	_, _ = v.Get(ctx, load)
	if loads != 1 {
		t.Fatalf("%d loads within the TTL", loads)
	}
	c.t = c.t.Add(time.Minute)
	fail = true
	if _, err := v.Get(ctx, load); err == nil {
		t.Fatal("an error must not be hidden without StaleOnError")
	}
	stale := 0
	sv := &Value[string]{TTL: time.Minute, Now: c.now, StaleOnError: true, OnStale: func(error) { stale++ }}
	if _, err := sv.Get(ctx, load); err == nil {
		t.Fatal("no previous value: the error counts")
	}
	fail = false
	_, _ = sv.Get(ctx, load)
	c.t = c.t.Add(time.Minute)
	fail = true
	if got, err := sv.Get(ctx, load); err != nil || got != "v" || stale != 1 {
		t.Fatalf("stale on error: %q %v %d", got, err, stale)
	}
	n := loads
	_, _ = sv.Get(ctx, load)
	if loads != n {
		t.Fatal("a stale value served is fresh for another TTL")
	}
}
