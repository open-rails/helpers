package cache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math/rand/v2"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeClock struct{ n atomic.Int64 }

func newClock() *fakeClock {
	c := &fakeClock{}
	c.n.Store(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
	return c
}

func (c *fakeClock) Now() time.Time          { return time.Unix(0, c.n.Load()) }
func (c *fakeClock) Advance(d time.Duration) { c.n.Add(int64(d)) }

// source is the upstream: it records each batch, can hold loads at a gate and fail.
type source struct {
	mu       sync.Mutex
	vals     map[string]string
	err      error
	gate     chan struct{}
	batches  [][]string
	entered  chan []string
	returned atomic.Int32
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newSource(vals map[string]string) *source {
	return &source{vals: vals, entered: make(chan []string, 100)}
}

func (s *source) load(ctx context.Context, keys []string) (map[string]string, error) {
	defer s.returned.Add(1)
	ks := slices.Sorted(slices.Values(keys))
	s.mu.Lock()
	s.batches = append(s.batches, ks)
	gate := s.gate
	s.mu.Unlock()
	s.entered <- ks
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	out := map[string]string{}
	for _, k := range keys {
		if v, ok := s.vals[k]; ok {
			out[k] = v
		}
	}
	return out, nil
}

func (s *source) set(f func(s *source)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

func (s *source) loads() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.batches)
}

func newCache(t *testing.T, src *source, clk Clock, edit ...func(*Options)) *Cache[string, string] {
	t.Helper()
	o := Options{Name: uniq(t), RefreshAfter: time.Minute, MaxStale: time.Hour, FailureBackoff: 30 * time.Second, Clock: clk}
	for _, e := range edit {
		e(&o)
	}
	c, err := New(o, src.load)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

var seq atomic.Int64

// uniq names a cache per run: Stats are per Name and process-wide.
func uniq(t *testing.T) string { return fmt.Sprintf("%s-%d", t.Name(), seq.Add(1)) }

func stats[K comparable, V any](c *Cache[K, V]) Stats {
	for _, s := range Snapshot() {
		if s.Name == c.o.Name {
			return s
		}
	}
	return Stats{}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// backedOff waits until the failed reload of k has pushed its next refresh past now.
func backedOff(t *testing.T, c *Cache[string, string], clk *fakeClock, k string) {
	t.Helper()
	eventually(t, "reload backoff", func() bool {
		e, ok := c.c.GetEntryQuietly(k)
		return ok && e.RefreshableAtNano > clk.Now().UnixNano()
	})
}

func get(t *testing.T, c *Cache[string, string], keys ...string) map[string]string {
	t.Helper()
	m, err := c.Get(context.Background(), keys)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func wantMap(t *testing.T, got, want map[string]string) {
	t.Helper()
	if !maps.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestNewRequiresPolicy(t *testing.T) {
	load := newSource(nil).load
	for name, o := range map[string]Options{
		"no name":            {RefreshAfter: time.Minute, MaxStale: time.Hour},
		"no refresh":         {Name: "x", MaxStale: time.Hour},
		"no max stale":       {Name: "x", RefreshAfter: time.Minute},
		"stale before fresh": {Name: "x", RefreshAfter: time.Hour, MaxStale: time.Minute},
		"negative knob":      {Name: "x", RefreshAfter: time.Minute, MaxStale: time.Hour, MaxEntries: -1},
	} {
		if _, err := New(o, load); err == nil {
			t.Errorf("%s: New accepted %+v", name, o)
		}
	}
	if _, err := New[string, string](Options{Name: "x", RefreshAfter: time.Minute, MaxStale: time.Hour}, nil); err == nil {
		t.Error("New accepted a nil load")
	}
	c, err := New(Options{Name: "x", RefreshAfter: time.Minute, MaxStale: time.Hour}, load)
	if err != nil {
		t.Fatal(err)
	}
	if c.o.LoadTimeout != 10*time.Second || c.o.FailureBackoff != 5*time.Second || c.o.MaxEntries != 10_000 {
		t.Fatalf("defaults not applied: %+v", c.o)
	}
}

func TestGetLoadsOnlyMissingKeysAndOmitsNotFound(t *testing.T) {
	src := newSource(map[string]string{"a": "A", "b": "B"})
	c := newCache(t, src, newClock())

	wantMap(t, get(t, c, "a"), map[string]string{"a": "A"})
	wantMap(t, get(t, c, "a", "b", "x"), map[string]string{"a": "A", "b": "B"})
	wantMap(t, get(t, c, "a", "b", "x"), map[string]string{"a": "A", "b": "B"})
	if got, want := fmt.Sprint(src.loads()), "[[a] [b x] [x]]"; got != want {
		t.Fatalf("batches %s, want %s (cached keys skipped, not-found never cached)", got, want)
	}
	if s := stats(c); s.Hits != 3 || s.Misses != 4 || s.Loads != 3 || s.LoadFailures != 0 || s.StaleServes != 0 {
		t.Fatalf("stats %+v", s)
	}
}

func TestConcurrentCallersShareOneLoad(t *testing.T) {
	src := newSource(map[string]string{"k": "v"})
	src.gate = make(chan struct{})
	c := newCache(t, src, newClock())

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() { wantMap(t, get(t, c, "k"), map[string]string{"k": "v"}) })
	}
	<-src.entered
	time.Sleep(20 * time.Millisecond) // let the others join
	close(src.gate)
	wg.Wait()
	if n := len(src.loads()); n != 1 {
		t.Fatalf("%d loads, want 1", n)
	}
}

func TestOverlappingBatchesLoadEachKeyOnce(t *testing.T) {
	src := newSource(map[string]string{"a": "A", "b": "B", "c": "C"})
	src.gate = make(chan struct{})
	c := newCache(t, src, newClock())

	var wg sync.WaitGroup
	wg.Go(func() { wantMap(t, get(t, c, "a", "b"), map[string]string{"a": "A", "b": "B"}) })
	if ks := <-src.entered; fmt.Sprint(ks) != "[a b]" {
		t.Fatalf("first batch %v", ks)
	}
	wg.Go(func() { wantMap(t, get(t, c, "b", "c"), map[string]string{"b": "B", "c": "C"}) })
	if ks := <-src.entered; fmt.Sprint(ks) != "[c]" {
		t.Fatalf("second batch %v, want only the key not already loading", ks)
	}
	close(src.gate)
	wg.Wait()
}

func TestCallerCancellationIsDetachedFromLoad(t *testing.T) {
	src := newSource(map[string]string{"k": "v", "j": "w"})
	src.gate = make(chan struct{})
	c := newCache(t, src, newClock())

	owner, cancel := context.WithCancel(context.Background())
	ownerErr := make(chan error, 1)
	go func() { _, err := c.Get(owner, []string{"k"}); ownerErr <- err }()
	<-src.entered
	joined := make(chan map[string]string, 1)
	go func() { joined <- get(t, c, "k") }()

	short, cancelShort := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelShort()
	if _, err := c.Get(short, []string{"j"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("caller deadline while loading: %v", err)
	}
	<-src.entered
	cancel()
	if err := <-ownerErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled owner got %v", err)
	}

	close(src.gate)
	wantMap(t, <-joined, map[string]string{"k": "v"})
	eventually(t, "abandoned load to finish", func() bool { return src.returned.Load() == 2 })
	wantMap(t, get(t, c, "k", "j"), map[string]string{"k": "v", "j": "w"})
	if n := len(src.loads()); n != 2 {
		t.Fatalf("%d loads, want 2: abandoned loads must complete and be cached", n)
	}
}

func TestLoadTimeout(t *testing.T) {
	src := newSource(map[string]string{"k": "v"})
	src.gate = make(chan struct{})
	c := newCache(t, src, newClock(), func(o *Options) { o.LoadTimeout = 30 * time.Millisecond })

	if _, err := c.Get(context.Background(), []string{"k"}); !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), c.o.Name) {
		t.Fatalf("got %v, want the load's deadline error naming the cache", err)
	}
	if s := stats(c); s.Loads != 1 || s.LoadFailures != 1 {
		t.Fatalf("stats %+v", s)
	}
}

func TestStaleWhileRevalidate(t *testing.T) {
	src := newSource(map[string]string{"k": "v1"})
	clk := newClock()
	c := newCache(t, src, clk)
	get(t, c, "k")
	<-src.entered

	src.set(func(s *source) { s.vals["k"], s.gate = "v2", make(chan struct{}) })
	clk.Advance(time.Minute + time.Second)
	wantMap(t, get(t, c, "k"), map[string]string{"k": "v1"}) // returns while the reload is held
	<-src.entered
	wantMap(t, get(t, c, "k"), map[string]string{"k": "v1"})
	close(src.gate)
	eventually(t, "refresh to commit", func() bool { v, _ := c.c.GetIfPresent("k"); return v == "v2" })
	wantMap(t, get(t, c, "k"), map[string]string{"k": "v2"})
	if n := len(src.loads()); n != 2 {
		t.Fatalf("%d loads, want 2", n)
	}
	if s := stats(c); s.StaleServes < 2 || s.Misses != 1 {
		t.Fatalf("stats %+v", s)
	}
}

func TestFailedReloadServesStaleWithBackoffUntilMaxStale(t *testing.T) {
	logs := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	down := errors.New("upstream down")
	src := newSource(map[string]string{"k": "v1"})
	clk := newClock()
	c := newCache(t, src, clk)
	get(t, c, "k")

	src.set(func(s *source) { s.err = down })
	clk.Advance(time.Minute + time.Second)
	wantMap(t, get(t, c, "k"), map[string]string{"k": "v1"})
	eventually(t, "failed reload", func() bool { return stats(c).LoadFailures == 1 })
	backedOff(t, c, clk, "k")

	for range 5 { // inside FailureBackoff: stale value, no new reload
		clk.Advance(5 * time.Second)
		wantMap(t, get(t, c, "k"), map[string]string{"k": "v1"})
	}
	time.Sleep(20 * time.Millisecond)
	if n := len(src.loads()); n != 2 {
		t.Fatalf("%d loads inside the backoff, want 2", n)
	}
	clk.Advance(6 * time.Second)
	wantMap(t, get(t, c, "k"), map[string]string{"k": "v1"})
	eventually(t, "reload after backoff", func() bool { return stats(c).LoadFailures == 2 })
	eventually(t, "reload failure log", func() bool {
		return strings.Contains(logs.String(), "cache="+c.o.Name) && strings.Contains(logs.String(), "upstream down")
	})

	clk.Advance(time.Hour)
	m, err := c.Get(context.Background(), []string{"k"})
	if !errors.Is(err, down) || len(m) != 0 {
		t.Fatalf("past MaxStale got %v, %v; want the load error", m, err)
	}
}

func TestFailedLoadOfAbsentKeyBacksOff(t *testing.T) {
	down := errors.New("upstream down")
	src := newSource(map[string]string{"k": "v", "j": "w"})
	src.err = down
	clk := newClock()
	c := newCache(t, src, clk)

	_, first := c.Get(context.Background(), []string{"k"})
	if !errors.Is(first, down) {
		t.Fatalf("got %v", first)
	}
	for range 3 {
		clk.Advance(9 * time.Second)
		if _, err := c.Get(context.Background(), []string{"k"}); err != first {
			t.Fatalf("inside FailureBackoff got %v, want the recorded %v", err, first)
		}
	}
	if n := len(src.loads()); n != 1 {
		t.Fatalf("%d loads inside the backoff, want 1", n)
	}

	src.set(func(s *source) { s.err = nil })
	m, err := c.Get(context.Background(), []string{"k", "j"})
	if err != first || !maps.Equal(m, map[string]string{"j": "w"}) {
		t.Fatalf("mixed batch got %v, %v", m, err)
	}
	clk.Advance(4 * time.Second)
	wantMap(t, get(t, c, "k"), map[string]string{"k": "v"})
	if got := fmt.Sprint(src.loads()); got != "[[k] [j] [k]]" {
		t.Fatalf("batches %s", got)
	}

	src.set(func(s *source) { s.err = down })
	c.Invalidate("k")
	c.Get(context.Background(), []string{"k"})
	c.Invalidate("k")
	src.set(func(s *source) { s.err = nil })
	wantMap(t, get(t, c, "k"), map[string]string{"k": "v"}) // Invalidate clears the failure
}

func TestRefreshFailureKeepsTheCachedValue(t *testing.T) {
	down := errors.New("upstream down")
	src := newSource(map[string]string{"k": "v1"})
	c := newCache(t, src, newClock())
	get(t, c, "k")

	src.set(func(s *source) { s.err = down })
	m, err := c.Refresh(context.Background(), []string{"k"})
	if !errors.Is(err, down) || !maps.Equal(m, map[string]string{"k": "v1"}) {
		t.Fatalf("failed refresh got %v, %v; want the kept value and the error", m, err)
	}
	wantMap(t, get(t, c, "k"), map[string]string{"k": "v1"})

	src.set(func(s *source) { s.err, s.vals["k"] = nil, "v2" })
	m, err = c.Refresh(context.Background(), []string{"k"})
	if err != nil || !maps.Equal(m, map[string]string{"k": "v2"}) {
		t.Fatalf("refresh got %v, %v", m, err)
	}
	wantMap(t, get(t, c, "k"), map[string]string{"k": "v2"})

	src.set(func(s *source) { delete(s.vals, "k") })
	if m, err = c.Refresh(context.Background(), []string{"k"}); err != nil || len(m) != 0 {
		t.Fatalf("refresh of a removed key got %v, %v", m, err)
	}
	if _, ok := c.c.GetIfPresent("k"); ok {
		t.Fatal("not-found refresh kept the entry")
	}
	if n := len(src.loads()); n != 4 {
		t.Fatalf("%d loads, want 4", n)
	}
}

func TestRefreshJoinsInFlightLoad(t *testing.T) {
	src := newSource(map[string]string{"k": "v"})
	src.gate = make(chan struct{})
	c := newCache(t, src, newClock())

	refreshed := make(chan map[string]string, 1)
	go func() {
		m, err := c.Refresh(context.Background(), []string{"k"})
		if err != nil {
			t.Error(err)
		}
		refreshed <- m
	}()
	<-src.entered
	got := make(chan map[string]string, 1)
	go func() { got <- get(t, c, "k") }()
	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.Refresh(short, []string{"k"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting Refresh past its deadline got %v", err)
	}
	close(src.gate)
	wantMap(t, <-refreshed, map[string]string{"k": "v"})
	wantMap(t, <-got, map[string]string{"k": "v"})
	if n := len(src.loads()); n != 1 {
		t.Fatalf("%d loads, want 1", n)
	}
}

func TestCloseStopsBackgroundGoroutines(t *testing.T) {
	before := runtime.NumGoroutine()
	var cs []*Cache[string, string]
	for range 20 {
		c, err := New(Options{Name: uniq(t), RefreshAfter: time.Minute, MaxStale: time.Hour}, newSource(nil).load)
		if err != nil {
			t.Fatal(err)
		}
		cs = append(cs, c)
	}
	if n := runtime.NumGoroutine(); n < before+20 {
		t.Fatalf("%d goroutines after New, %d before", n, before)
	}
	for _, c := range cs {
		c.Close()
		c.Close()
	}
	eventually(t, "goroutines to stop", func() bool { return runtime.NumGoroutine() <= before })
	src := newSource(map[string]string{"k": "v"})
	c, _ := New(Options{Name: uniq(t), RefreshAfter: time.Minute, MaxStale: time.Hour}, src.load)
	c.Close()
	wantMap(t, get(t, c, "k"), map[string]string{"k": "v"})
	wantMap(t, get(t, c, "k"), map[string]string{"k": "v"})
	if n := len(src.loads()); n != 1 {
		t.Fatalf("%d loads after Close, want 1", n)
	}
}

func TestInvalidateBeatsInFlightLoad(t *testing.T) {
	src := newSource(map[string]string{"k": "old"})
	src.gate = make(chan struct{})
	c := newCache(t, src, newClock())

	done := make(chan map[string]string, 1)
	go func() { done <- get(t, c, "k") }()
	<-src.entered
	c.Invalidate("k")
	src.set(func(s *source) { s.vals["k"] = "new" })
	close(src.gate)
	<-done
	if v, ok := c.c.GetIfPresent("k"); ok {
		t.Fatalf("load that started before Invalidate was cached: %q", v)
	}
	src.set(func(s *source) { s.gate = nil })
	wantMap(t, get(t, c, "k"), map[string]string{"k": "new"})
}

func TestInvalidateBeatsInFlightRefresh(t *testing.T) {
	src := newSource(map[string]string{"k": "old"})
	clk := newClock()
	c := newCache(t, src, clk)
	get(t, c, "k")
	<-src.entered

	gate := make(chan struct{})
	src.set(func(s *source) { s.gate = gate })
	clk.Advance(time.Minute + time.Second)
	get(t, c, "k")
	<-src.entered
	c.Invalidate("k")
	src.set(func(s *source) { s.vals["k"], s.gate = "new", nil })
	close(gate)
	eventually(t, "refresh to finish", func() bool { return src.returned.Load() == 2 })
	time.Sleep(20 * time.Millisecond)
	if v, ok := c.c.GetIfPresent("k"); ok {
		t.Fatalf("invalidated key resurrected by refresh: %q", v)
	}
	wantMap(t, get(t, c, "k"), map[string]string{"k": "new"})
}

func TestInvalidateAll(t *testing.T) {
	src := newSource(map[string]string{"a": "A", "b": "B"})
	c := newCache(t, src, newClock())
	get(t, c, "a", "b")
	c.InvalidateAll()
	get(t, c, "a", "b")
	if n := len(src.loads()); n != 2 {
		t.Fatalf("%d loads, want 2", n)
	}
}

func TestMaxEntriesBoundsTheCache(t *testing.T) {
	vals := map[string]string{}
	keys := make([]string, 200)
	for i := range keys {
		keys[i] = fmt.Sprint(i)
		vals[keys[i]] = keys[i]
	}
	c := newCache(t, newSource(vals), newClock(), func(o *Options) { o.MaxEntries = 10 })
	if m := get(t, c, keys...); len(m) != 200 {
		t.Fatalf("batch returned %d values", len(m))
	}
	c.c.CleanUp()
	if n := c.c.EstimatedSize(); n > 10 {
		t.Fatalf("%d entries cached, want <= 10", n)
	}
}

func TestLoadPanicIsALoadFailure(t *testing.T) {
	c, err := New(Options{Name: uniq(t), RefreshAfter: time.Minute, MaxStale: time.Hour},
		func(context.Context, []string) (map[string]string, error) { panic("boom") })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(context.Background(), []string{"k"}); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("got %v", err)
	}
	if s := stats(c); s.LoadFailures != 1 {
		t.Fatalf("stats %+v", s)
	}
}

func TestConcurrentUse(t *testing.T) {
	vals := map[string]string{}
	for i := range 20 {
		vals[fmt.Sprint(i)] = fmt.Sprint(i)
	}
	clk := newClock()
	c, err := New(Options{Name: uniq(t), RefreshAfter: time.Second, MaxStale: time.Minute, MaxEntries: 15, Clock: clk},
		func(ctx context.Context, keys []string) (map[string]string, error) {
			if rand.N(5) == 0 {
				return nil, errors.New("flaky")
			}
			out := map[string]string{}
			for _, k := range keys {
				out[k] = vals[k]
			}
			return out, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	stop := time.Now().Add(300 * time.Millisecond)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for time.Now().Before(stop) {
				keys := []string{fmt.Sprint(rand.N(20)), fmt.Sprint(rand.N(20))}
				switch rand.N(10) {
				case 0:
					c.Invalidate(keys...)
				case 1:
					clk.Advance(time.Duration(rand.N(3)) * time.Second)
				default:
					ctx, cancel := context.WithTimeout(context.Background(), time.Duration(rand.N(5))*time.Millisecond)
					if m, err := c.Get(ctx, keys); err == nil {
						for k, v := range m {
							if v != k {
								t.Errorf("key %s = %q", k, v)
							}
						}
					}
					cancel()
				}
			}
		})
	}
	wg.Wait()
}
