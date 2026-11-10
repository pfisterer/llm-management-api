// Package ttlcache keeps values for a while: per key (Map) or a single one (Value). It replaces the caches the service had grown in seven places, each with its own mutex and "until".
package ttlcache

import (
	"context"
	"sync"
	"time"
)

// Options of a Map.
type Options struct {
	// TTL is how long a value is fresh.
	TTL time.Duration
	// KeepStale is how long an expired value is still kept for Stale: 0 = not at all (it goes at the next sweep), < 0 = until it is set anew.
	KeepStale time.Duration
	// MaxLen, when > 0, empties the map when a new key would make it longer: a bound for keys that come from outside (tokens).
	MaxLen int
}

type entry[V any] struct {
	v     V
	until time.Time
}

// Map keeps a value per key for Options.TTL. Expired values are swept from time to time when values are set.
type Map[K comparable, V any] struct {
	opt Options
	// Now is the clock (tests); nil = time.Now.
	Now   func() time.Time
	mu    sync.Mutex
	m     map[K]entry[V]
	swept time.Time
}

// NewMap makes an empty Map.
func NewMap[K comparable, V any](opt Options) *Map[K, V] {
	return &Map[K, V]{opt: opt, m: map[K]entry[V]{}}
}

func (c *Map[K, V]) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Get is the value of k while it is fresh.
func (c *Map[K, V]) Get(k K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[k]
	if !ok || !c.now().Before(e.until) {
		var zero V
		return zero, false
	}
	return e.v, true
}

// Stale is the value of k, fresh or expired (as long as KeepStale keeps it), for when getting a new one failed.
func (c *Map[K, V]) Stale(k K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[k]
	return e.v, ok
}

// Set keeps v for k, fresh for TTL from now.
func (c *Map[K, V]) Set(k K, v V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if _, ok := c.m[k]; !ok && c.opt.MaxLen > 0 && len(c.m) >= c.opt.MaxLen {
		clear(c.m)
	}
	if c.opt.KeepStale >= 0 && now.Sub(c.swept) >= c.opt.TTL {
		c.swept = now
		for key, e := range c.m {
			if now.Sub(e.until) >= c.opt.KeepStale {
				delete(c.m, key)
			}
		}
	}
	c.m[k] = entry[V]{v: v, until: now.Add(c.opt.TTL)}
}

// Delete forgets k.
func (c *Map[K, V]) Delete(k K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, k)
}

// Clear forgets every key.
func (c *Map[K, V]) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	clear(c.m)
}

// Len is the number of keys kept, fresh or not.
func (c *Map[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}

// Value keeps one value for TTL, loaded on demand; concurrent callers wait for one load. An error is not kept: the next call loads again, unless StaleOnError serves the previous value.
type Value[V any] struct {
	TTL time.Duration
	// StaleOnError, when a load fails and a previous value exists, serves that value for another TTL instead of the error and tells OnStale.
	StaleOnError bool
	OnStale      func(err error)
	// Now is the clock (tests); nil = time.Now.
	Now func() time.Time

	mu    sync.Mutex
	v     V
	have  bool
	until time.Time
}

func (c *Value[V]) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Get is the value, loaded with load when there is none or it is TTL old.
func (c *Value[V]) Get(ctx context.Context, load func(ctx context.Context) (V, error)) (V, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.have && c.now().Before(c.until) {
		return c.v, nil
	}
	v, err := load(ctx)
	if err != nil {
		if c.StaleOnError && c.have {
			if c.OnStale != nil {
				c.OnStale(err)
			}
			c.until = c.now().Add(c.TTL)
			return c.v, nil
		}
		var zero V
		return zero, err
	}
	c.v, c.have, c.until = v, true, c.now().Add(c.TTL)
	return v, nil
}
