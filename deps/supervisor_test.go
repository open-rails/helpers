package deps

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBackoffIsCappedFullJitter(t *testing.T) {
	for attempt := 0; attempt < 64; attempt++ {
		ceil := min(30*time.Second, (500*time.Millisecond)<<min(attempt, 20))
		for range 50 {
			if d := Backoff(attempt, 500*time.Millisecond, 30*time.Second); d < 0 || d > ceil {
				t.Fatalf("attempt %d: %v outside [0,%v]", attempt, d, ceil)
			}
		}
	}
}

func TestNilDependencyIsDownAndSwitchUsesFallback(t *testing.T) {
	var d *Dependency
	if d.Up() || d.Report(context.DeadlineExceeded) {
		t.Fatal("nil dependency must read as down and ignore reports")
	}
	sw := NewSwitch[string](nil, "primary", func() string { return "fallback" })
	if b, primary := sw.Get(); primary || b != "fallback" {
		t.Fatalf("got %q primary=%v", b, primary)
	}
}

func TestApplicationErrorsDoNotMarkDown(t *testing.T) {
	sup := New(WithTiming(fastTiming))
	dep := sup.Add("pg", Required, func(context.Context) error { return nil }, nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sup.Start(ctx)
	eventually(t, "up", dep.Up)
	if dep.Report(errors.New("duplicate key")) || !dep.Up() {
		t.Fatal("application error marked dependency down")
	}
}

// gatedProbe blocks each probe until the test releases it with a result.
type gatedProbe struct {
	started chan struct{}
	results chan error
}

func newGatedProbe() *gatedProbe {
	return &gatedProbe{started: make(chan struct{}, 16), results: make(chan error)}
}

func (g *gatedProbe) probe(ctx context.Context) error {
	g.started <- struct{}{}
	select {
	case err := <-g.results:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// A data-path failure that lands while a successful probe is in flight must
// not be undone by that probe: the down transition is recorded, hooks fire,
// and recovery takes fresh successful probes.
func TestReportDuringInFlightProbeIsNotUndone(t *testing.T) {
	g := newGatedProbe()
	sup := New(WithTiming(Timing{Interval: 5 * time.Millisecond, Timeout: time.Minute, BackoffBase: time.Millisecond, BackoffMax: 2 * time.Millisecond, RecoverAfter: 2}))
	dep := sup.Add("redis", Optional, g.probe, nil)
	var ups, downs atomic.Int32
	dep.OnUp(func(context.Context) { ups.Add(1) })
	dep.OnDown(func(context.Context) { downs.Add(1) })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sup.Start(ctx)

	<-g.started
	g.results <- nil
	eventually(t, "up", dep.Up)

	<-g.started // a probe is in flight
	if !dep.Report(context.DeadlineExceeded) {
		t.Fatal("timeout not classified as unavailable")
	}
	g.results <- nil // the in-flight probe succeeds, but started before the failure
	eventually(t, "down hook", func() bool { return downs.Load() == 1 })
	if dep.Up() {
		t.Fatal("stale probe success restored the dependency")
	}
	st := dep.Status()
	if st.TransitionsDown != 1 {
		t.Fatalf("status %+v", st)
	}
	for range 2 {
		<-g.started
		g.results <- nil
	}
	eventually(t, "up hook", func() bool { return ups.Load() == 1 && dep.Up() })
}

// A slow hook runs off the probe loop: probing and transitions continue.
func TestSlowHookDoesNotBlockProbing(t *testing.T) {
	var fail atomic.Bool
	sup := New(WithTiming(fastTiming))
	dep := sup.Add("redis", Optional, func(context.Context) error {
		if fail.Load() {
			return errors.New("down")
		}
		return nil
	}, nil)
	hookCtx := make(chan context.Context, 1)
	dep.OnDown(func(ctx context.Context) {
		hookCtx <- ctx
		<-ctx.Done() // blocks until the supervisor stops
	})
	ctx, cancel := context.WithCancel(t.Context())
	sup.Start(ctx)
	eventually(t, "up", dep.Up)
	fail.Store(true)
	eventually(t, "down", func() bool { return !dep.Up() })
	hc := <-hookCtx
	fail.Store(false)
	eventually(t, "recovered while the down hook is still running", dep.Up)
	cancel()
	select {
	case <-hc.Done():
	case <-time.After(time.Second):
		t.Fatal("hook context not cancelled with the supervisor")
	}
}

func TestRequiredDependencyNeedsConsecutiveFailures(t *testing.T) {
	var failing atomic.Int32
	sup := New(WithTiming(Timing{Interval: 5 * time.Millisecond, Timeout: time.Second, BackoffBase: time.Millisecond, BackoffMax: 2 * time.Millisecond, RequiredDownAfter: 3}))
	dep := sup.Add("postgres", Required, func(context.Context) error {
		if failing.Load() > 0 {
			failing.Add(-1)
			return errors.New("pool saturated")
		}
		return nil
	}, nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sup.Start(ctx)
	eventually(t, "up", dep.Up)
	failing.Store(2) // two isolated failures stay up
	eventually(t, "failures consumed", func() bool { return failing.Load() == 0 })
	time.Sleep(20 * time.Millisecond)
	if dep.Status().TransitionsDown != 0 {
		t.Fatal("required dependency went down on fewer than 3 consecutive failures")
	}
	failing.Store(1 << 20)
	eventually(t, "down after 3", func() bool { return !dep.Up() })
}

func TestAddReplacesDependencyOfSameName(t *testing.T) {
	sup := New(WithTiming(fastTiming))
	var oldCalls atomic.Int32
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sup.Start(ctx)
	sup.Add("redis", Optional, func(context.Context) error { oldCalls.Add(1); return nil }, nil)
	eventually(t, "old probing", func() bool { return oldCalls.Load() > 0 })
	next := sup.Add("redis", Optional, func(context.Context) error { return nil }, nil, ProbeTimeout(3*time.Second))
	if sup.Get("redis") != next || len(sup.Statuses()) != 1 {
		t.Fatal("dependency not replaced")
	}
	n := oldCalls.Load()
	time.Sleep(150 * time.Millisecond)
	if oldCalls.Load() > n+1 {
		t.Fatal("replaced dependency still probing")
	}
}

// A Report that lands between a probe's staleness check and its up
// transition is not lost: it is ordered after the transition and wins.
func TestReportAtUpTransitionIsNotLost(t *testing.T) {
	var fail atomic.Bool
	sup := New(WithTiming(Timing{Interval: time.Hour, Timeout: time.Second, BackoffBase: time.Millisecond, BackoffMax: 2 * time.Millisecond, RecoverAfter: 1}))
	dep := sup.Add("redis", Optional, func(context.Context) error {
		if fail.Load() {
			return errors.New("down")
		}
		return nil
	}, nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sup.Start(ctx)
	eventually(t, "up", dep.Up)
	fail.Store(true)
	dep.Report(context.DeadlineExceeded)
	eventually(t, "failing probes", func() bool { return dep.Status().ConsecutiveFailures > 0 })

	reported := make(chan struct{})
	var once sync.Once
	testHookBeforeUp = func() {
		once.Do(func() {
			go func() { fail.Store(true); dep.Report(context.DeadlineExceeded); close(reported) }()
			time.Sleep(20 * time.Millisecond) // the window the race needs
		})
	}
	t.Cleanup(func() { testHookBeforeUp = nil })
	fail.Store(false)
	<-reported
	eventually(t, "the up transition completed", func() bool { return dep.Status().TransitionsUp >= 1 })
	time.Sleep(20 * time.Millisecond)
	if dep.Up() {
		t.Fatal("a Report racing the up transition was lost")
	}
}

// A hook that never returns cannot stall the probe loop, and later
// transitions still reach the hooks once it is unblocked.
func TestHungHookDoesNotStallProbeLoop(t *testing.T) {
	var fail atomic.Bool
	sup := New(WithTiming(Timing{Interval: time.Millisecond, Timeout: time.Second, BackoffBase: time.Millisecond, BackoffMax: time.Millisecond, RecoverAfter: 1}))
	dep := sup.Add("redis", Optional, func(context.Context) error {
		if fail.Load() {
			return errors.New("down")
		}
		return nil
	}, nil)
	release := make(chan struct{})
	var downs, ups atomic.Int32
	dep.OnDown(func(ctx context.Context) {
		if downs.Add(1) == 1 {
			<-release
		}
	})
	dep.OnUp(func(context.Context) { ups.Add(1) })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sup.Start(ctx)
	eventually(t, "up", dep.Up)
	for range 200 { // far more transitions than any fixed queue could hold
		fail.Store(true)
		eventually(t, "down", func() bool { return !dep.Up() })
		fail.Store(false)
		eventually(t, "up", dep.Up)
	}
	if got := dep.Status().TransitionsDown; got < 200 {
		t.Fatalf("probe loop stalled: %d down transitions", got)
	}
	close(release)
	eventually(t, "coalesced hooks delivered", func() bool { return downs.Load() >= 2 && ups.Load() >= 1 })
}

func TestProbeIntervalSlowsProbesWhileUp(t *testing.T) {
	sup := New(WithTiming(fastTiming))
	var fast, slow atomic.Int32
	sup.Add("fast", Optional, func(context.Context) error { fast.Add(1); return nil }, nil)
	sup.Add("slow", Optional, func(context.Context) error { slow.Add(1); return nil }, nil, ProbeInterval(time.Hour))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sup.Start(ctx)
	eventually(t, "fast probed repeatedly", func() bool { return fast.Load() >= 5 })
	if n := slow.Load(); n != 1 {
		t.Fatalf("slow dependency probed %d times, want 1", n)
	}
}

func TestNonPositiveProbeIntervalKeepsTheDefault(t *testing.T) {
	sup := New(WithTiming(fastTiming))
	var n atomic.Int32
	sup.Add("zero", Optional, func(context.Context) error { n.Add(1); return nil }, nil, ProbeInterval(0))
	sup.Add("negative", Optional, func(context.Context) error { return nil }, nil, ProbeInterval(-time.Second))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sup.Start(ctx)
	time.Sleep(300 * time.Millisecond)
	// fastTiming probes every 50ms while up: ~6 probes, never a busy loop.
	if got := n.Load(); got > 20 {
		t.Fatalf("ProbeInterval(0) probed %d times in 300ms", got)
	}
}

func TestUpIntervalIsJittered(t *testing.T) {
	seen := map[time.Duration]bool{}
	for range 50 {
		d := jitter(10 * time.Second)
		if d < 9*time.Second || d > 11*time.Second {
			t.Fatalf("jitter(10s) = %v outside ±10%%", d)
		}
		seen[d] = true
	}
	if len(seen) < 2 {
		t.Fatal("up-interval is not jittered")
	}
}
