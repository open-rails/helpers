// Package cache is an in-memory, batch-first read-through cache: one in-flight
// load per key, stale-while-revalidate, bounded staleness and failure backoff
// on upstream errors, and invalidation that beats in-flight loads. The caller
// owns the policy.
package cache

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/maypok86/otter/v2"
)

// Load returns the values for keys; a key missing from the map is not found (not cached).
type Load[K comparable, V any] func(ctx context.Context, keys []K) (map[K]V, error)

// Clock supplies the time that drives refresh, staleness and backoff.
type Clock interface{ Now() time.Time }

// Options is a cache's policy. Name, RefreshAfter and MaxStale are required;
// zero mechanical knobs take the defaults in parentheses.
type Options struct {
	Name           string        // metrics/log label; caches sharing a Name share Stats
	RefreshAfter   time.Duration // serve the cached value and reload in the background after this
	MaxStale       time.Duration // past this since the last good load, a failed reload's error surfaces
	LoadTimeout    time.Duration // deadline of a detached load (10s)
	FailureBackoff time.Duration // minimum gap between failed loads of one key (5s)
	MaxEntries     int           // bound on cached keys (10_000)
	Clock          Clock         // nil = real time; for tests
}

// Cache is safe for concurrent use.
type Cache[K comparable, V any] struct {
	o     Options
	c     *otter.Cache[K, V]
	fails *otter.Cache[K, error] // last load error of keys without a value, for FailureBackoff
	epoch atomic.Uint64          // bumped by Invalidate so a failure that raced it is not recorded
	load  Load[K, V]
	stats *counters
}

// New validates o, fills the mechanical defaults and returns an empty cache.
func New[K comparable, V any](o Options, load Load[K, V]) (*Cache[K, V], error) {
	if o.Name == "" || load == nil {
		return nil, errors.New("cache: Name and load are required")
	}
	if o.RefreshAfter <= 0 || o.MaxStale <= o.RefreshAfter {
		return nil, fmt.Errorf("cache %q: need 0 < RefreshAfter < MaxStale", o.Name)
	}
	if o.LoadTimeout < 0 || o.FailureBackoff < 0 || o.MaxEntries < 0 {
		return nil, fmt.Errorf("cache %q: negative LoadTimeout, FailureBackoff or MaxEntries", o.Name)
	}
	o.LoadTimeout = cmp.Or(o.LoadTimeout, 10*time.Second)
	o.FailureBackoff = cmp.Or(o.FailureBackoff, 5*time.Second)
	o.MaxEntries = cmp.Or(o.MaxEntries, 10_000)
	var clk otter.Clock
	if o.Clock != nil {
		clk = clock{o.Clock}
	}
	c := &Cache[K, V]{o: o, load: load, stats: countersFor(o.Name)}
	var err error
	if c.c, err = otter.New(&otter.Options[K, V]{
		MaximumSize:       o.MaxEntries,
		ExpiryCalculator:  otter.ExpiryWriting[K, V](o.MaxStale),
		RefreshCalculator: backoff[K, V]{otter.RefreshWriting[K, V](o.RefreshAfter), o.FailureBackoff},
		Clock:             clk,
		Logger:            logger{o.Name},
	}); err != nil {
		return nil, fmt.Errorf("cache %q: %w", o.Name, err)
	}
	if c.fails, err = otter.New(&otter.Options[K, error]{
		MaximumSize:      o.MaxEntries,
		ExpiryCalculator: otter.ExpiryWriting[K, error](o.FailureBackoff),
		Clock:            clk,
		Logger:           logger{o.Name},
	}); err != nil {
		c.c.StopAllGoroutines()
		return nil, fmt.Errorf("cache %q: %w", o.Name, err)
	}
	return c, nil
}

// Get returns the cached or loaded values for keys; keys Load did not find are
// omitted. Missing keys load once across concurrent callers, detached from
// ctx: a caller whose ctx ends gets ctx.Err() while the load still completes
// and is cached. A key without a value whose load failed less than
// FailureBackoff ago returns that error again without loading. On error the
// map still holds the keys that were available.
func (c *Cache[K, V]) Get(ctx context.Context, keys []K) (map[K]V, error) {
	out := make(map[K]V, len(keys))
	var hits, misses, stale uint64
	var load []K
	var errs []error
	for _, k := range keys {
		e, ok := c.c.GetEntry(k)
		if !ok {
			misses++
			if err, failed := c.fails.GetIfPresent(k); failed {
				errs = appendErr(errs, err)
			} else {
				load = append(load, k)
			}
			continue
		}
		out[k] = e.Value
		hits++
		if e.SnapshotAtNano-(e.ExpiresAtNano-int64(c.o.MaxStale)) >= int64(c.o.RefreshAfter) {
			stale++
		}
		if e.RefreshableAtNano <= e.SnapshotAtNano {
			load = append(load, k) // due: BulkGet starts the background reload
		}
	}
	c.stats.hits.Add(hits)
	c.stats.misses.Add(misses)
	c.stats.stale.Add(stale)
	if len(load) == 0 {
		return out, join(errs)
	}
	type result struct {
		m   map[K]V
		err error
	}
	done := make(chan result, 1)
	go func() {
		m, err := c.c.BulkGet(context.WithoutCancel(ctx), load, otter.BulkLoaderFunc[K, V](c.bulkLoad))
		done <- result{m, err}
	}()
	select {
	case r := <-done:
		maps.Copy(out, r.m)
		if r.err != nil {
			errs = appendErr(errs, r.err)
		}
	case <-ctx.Done():
		errs = append(errs, ctx.Err())
	}
	return out, join(errs)
}

// Refresh reloads keys now, joining a load already in flight for them, and
// waits for it (the load itself is detached from ctx). The map holds each
// key's value afterwards: the reloaded one or, when its load failed, the value
// still cached, which a failed Refresh keeps; the error joins the load errors.
// Keys the load did not find are dropped and omitted. FailureBackoff does not
// delay a Refresh.
func (c *Cache[K, V]) Refresh(ctx context.Context, keys []K) (map[K]V, error) {
	ch := c.c.BulkRefresh(context.WithoutCancel(ctx), keys, otter.BulkLoaderFunc[K, V](c.bulkLoad))
	select {
	case rs := <-ch:
		out := make(map[K]V, len(rs))
		var errs []error
		for _, r := range rs {
			switch {
			case r.Err == nil:
				out[r.Key] = r.Value
			case errors.Is(r.Err, otter.ErrNotFound):
			default:
				if v, ok := c.c.GetIfPresent(r.Key); ok {
					out[r.Key] = v
				}
				errs = appendErr(errs, r.Err)
			}
		}
		return out, join(errs)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Invalidate drops keys and their recorded failures; a load or reload already
// in flight for them is discarded.
func (c *Cache[K, V]) Invalidate(keys ...K) {
	c.epoch.Add(1)
	for _, k := range keys {
		c.c.Invalidate(k)
		c.fails.Invalidate(k)
	}
}

// InvalidateAll drops every key, as Invalidate.
func (c *Cache[K, V]) InvalidateAll() {
	c.epoch.Add(1)
	c.c.InvalidateAll()
	c.fails.InvalidateAll()
}

// Close stops the cache's background expiry goroutines so a discarded cache
// can be collected. It is idempotent; the cache keeps working after Close
// (expired entries are still never served, only removed later).
func (c *Cache[K, V]) Close() {
	c.c.StopAllGoroutines()
	c.fails.StopAllGoroutines()
}

func (c *Cache[K, V]) bulkLoad(ctx context.Context, keys []K) (m map[K]V, err error) {
	epoch := c.epoch.Load()
	ctx, cancel := context.WithTimeout(ctx, c.o.LoadTimeout)
	defer cancel()
	c.stats.loads.Add(1)
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("load panicked: %v", r)
		}
		if err == nil {
			for k := range m {
				c.fails.Invalidate(k)
			}
			return
		}
		c.stats.failures.Add(1)
		err = fmt.Errorf("cache %q: %w", c.o.Name, err)
		for _, k := range keys {
			c.fails.Set(k, err)
		}
		if c.epoch.Load() != epoch {
			for _, k := range keys {
				c.fails.Invalidate(k)
			}
		}
	}()
	return c.load(ctx, keys)
}

// join keeps a single error as is, so a backed-off key returns the recorded error itself.
func join(errs []error) error {
	if len(errs) == 1 {
		return errs[0]
	}
	return errors.Join(errs...)
}

// appendErr skips repeats: one failed batch fails each of its keys with the same error.
func appendErr(errs []error, err error) []error {
	if slices.Contains(errs, err) {
		return errs
	}
	return append(errs, err)
}

// backoff retries a failed reload no sooner than d, serving the stale value meanwhile.
type backoff[K comparable, V any] struct {
	otter.RefreshCalculator[K, V]
	d time.Duration
}

func (b backoff[K, V]) RefreshAfterReloadFailure(otter.Entry[K, V], error) time.Duration { return b.d }

type clock struct{ Clock }

func (c clock) NowNano() int64                      { return c.Now().UnixNano() }
func (clock) Tick(d time.Duration) <-chan time.Time { return time.Tick(d) }

// logger reports failed background reloads, which no caller sees.
type logger struct{ name string }

func (l logger) Warn(ctx context.Context, msg string, err error) {
	slog.WarnContext(ctx, "cache: "+msg, "cache", l.name, "err", err)
}

func (l logger) Error(ctx context.Context, msg string, err error) {
	slog.ErrorContext(ctx, "cache: "+msg, "cache", l.name, "err", err)
}

// Stats are cumulative counts for one cache Name. Hits include StaleServes
// (values served past RefreshAfter); Loads and LoadFailures count Load calls.
type Stats struct {
	Name                                           string
	Hits, Misses, StaleServes, Loads, LoadFailures uint64
}

type counters struct{ hits, misses, stale, loads, failures atomic.Uint64 }

var registry sync.Map // name -> *counters

func countersFor(name string) *counters {
	v, _ := registry.LoadOrStore(name, new(counters))
	return v.(*counters)
}

// Snapshot returns the Stats of every cache Name in this process, by Name.
func Snapshot() []Stats {
	var out []Stats
	registry.Range(func(k, v any) bool {
		c := v.(*counters)
		out = append(out, Stats{k.(string), c.hits.Load(), c.misses.Load(), c.stale.Load(), c.loads.Load(), c.failures.Load()})
		return true
	})
	slices.SortFunc(out, func(a, b Stats) int { return strings.Compare(a.Name, b.Name) })
	return out
}
