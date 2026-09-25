// Package deps supervises a process's external dependencies. Each dependency
// is probed forever: on a fixed interval while up, with full-jitter capped
// exponential backoff while down. Callers read Up() to choose between a primary
// backend and a local fallback, and Report data-path errors so an outage is
// noticed on the first failed call instead of the next probe.
package deps

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Class says whether the process can do meaningful work without a dependency.
type Class int

const (
	Required Class = iota
	Optional
)

func (c Class) String() string {
	if c == Required {
		return "required"
	}
	return "optional"
}

// Probe checks a dependency once. It must honor ctx.
type Probe func(ctx context.Context) error

// Timing controls probing. Zero fields take the defaults.
type Timing struct {
	Interval     time.Duration // probe period while up (10s)
	Timeout      time.Duration // per-probe timeout (1s)
	BackoffBase  time.Duration // first backoff ceiling while down (500ms)
	BackoffMax   time.Duration // backoff ceiling cap (30s)
	RecoverAfter int           // consecutive successes before down→up (2)
}

func (t Timing) withDefaults() Timing {
	if t.Interval <= 0 {
		t.Interval = 10 * time.Second
	}
	if t.Timeout <= 0 {
		t.Timeout = time.Second
	}
	if t.BackoffBase <= 0 {
		t.BackoffBase = 500 * time.Millisecond
	}
	if t.BackoffMax <= 0 {
		t.BackoffMax = 30 * time.Second
	}
	if t.RecoverAfter <= 0 {
		t.RecoverAfter = 2
	}
	return t
}

// Backoff returns a full-jitter delay for the given zero-based attempt:
// uniform in [0, min(max, base·2^attempt)].
func Backoff(attempt int, base, max time.Duration) time.Duration {
	ceil := max
	if attempt < 30 {
		if d := base << attempt; d > 0 && d < max {
			ceil = d
		}
	}
	return rand.N(ceil + 1)
}

// Supervisor owns a set of dependencies and the process readiness state.
type Supervisor struct {
	timing   Timing
	log      *slog.Logger
	mu       sync.Mutex
	deps     []*Dependency
	counters []*Counter
	started  bool
	ctx      context.Context
	ready    atomic.Bool
	draining atomic.Bool
}

// Option configures a Supervisor.
type Option func(*Supervisor)

func WithTiming(t Timing) Option       { return func(s *Supervisor) { s.timing = t } }
func WithLogger(l *slog.Logger) Option { return func(s *Supervisor) { s.log = l } }

func New(opts ...Option) *Supervisor {
	s := &Supervisor{log: slog.Default()}
	for _, o := range opts {
		o(s)
	}
	s.timing = s.timing.withDefaults()
	return s
}

// Dependency is one supervised external system.
type Dependency struct {
	sup         *Supervisor
	name        string
	class       Class
	probe       Probe
	unavailable func(error) bool
	up          atomic.Bool
	kick        chan struct{}

	mu          sync.Mutex
	since       time.Time
	lastErr     error
	failures    int
	toUp        int64
	toDown      int64
	onUp        []func()
	onDown      []func()
	reported    bool
	initialized bool
}

// Add registers a dependency. unavailable classifies data-path errors passed to
// Report; nil uses IsConnectivity. Dependencies added after Start start at once.
func (s *Supervisor) Add(name string, class Class, probe Probe, unavailable func(error) bool) *Dependency {
	if unavailable == nil {
		unavailable = IsConnectivity
	}
	d := &Dependency{sup: s, name: name, class: class, probe: probe, unavailable: unavailable, kick: make(chan struct{}, 1), since: time.Now()}
	s.mu.Lock()
	s.deps = append(s.deps, d)
	started, ctx := s.started, s.ctx
	s.mu.Unlock()
	if started {
		go d.run(ctx)
	}
	return d
}

// Start probes every dependency in the background until ctx ends.
func (s *Supervisor) Start(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return
	}
	s.started, s.ctx = true, ctx
	for _, d := range s.deps {
		go d.run(ctx)
	}
}

// SetReady marks the process as built and able to serve.
func (s *Supervisor) SetReady() { s.ready.Store(true) }

// Drain marks the process as shutting down; readiness fails from now on.
func (s *Supervisor) Drain() { s.draining.Store(true) }

// Ready reports whether the process should receive traffic.
func (s *Supervisor) Ready() bool { return s.ready.Load() && !s.draining.Load() }

// Get returns a registered dependency by name, or nil.
func (s *Supervisor) Get(name string) *Dependency {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.deps {
		if d.name == name {
			return d
		}
	}
	return nil
}

func (d *Dependency) Name() string { return d.name }
func (d *Dependency) Class() Class { return d.class }

// Up reports whether the dependency is currently usable. A nil Dependency is
// never up, so an unconfigured dependency reads as permanently down.
func (d *Dependency) Up() bool { return d != nil && d.up.Load() }

// OnUp registers fn to run after each down→up transition (not the first up).
// Hooks run on the supervisor goroutine and must not block for long.
func (d *Dependency) OnUp(fn func()) {
	d.mu.Lock()
	d.onUp = append(d.onUp, fn)
	d.mu.Unlock()
}

// OnDown registers fn to run after each up→down transition.
func (d *Dependency) OnDown(fn func()) {
	d.mu.Lock()
	d.onDown = append(d.onDown, fn)
	d.mu.Unlock()
}

// Report feeds a data-path result back. It returns true when err means the
// dependency is unavailable, in which case the dependency is marked down
// immediately and probed with backoff until it recovers.
func (d *Dependency) Report(err error) bool {
	if d == nil || err == nil || !d.unavailable(err) {
		return false
	}
	if d.up.CompareAndSwap(true, false) {
		d.mu.Lock()
		d.lastErr = err
		d.mu.Unlock()
		select {
		case d.kick <- struct{}{}:
		default:
		}
	}
	return true
}

func (d *Dependency) run(ctx context.Context) {
	t := d.sup.timing
	attempt, successes, failed := 0, 0, false
	for {
		pctx, cancel := context.WithTimeout(ctx, t.Timeout)
		err := d.probe(pctx)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			successes++
			// Hysteresis only guards recovery; a dependency that has never
			// failed is up on its first successful probe.
			if !d.up.Load() && (successes >= t.RecoverAfter || !failed) {
				d.up.Store(true)
			}
		} else {
			successes, failed = 0, true
			d.up.Store(false)
		}
		d.record(err)

		var wait time.Duration
		switch {
		case d.up.Load():
			attempt, wait = 0, t.Interval
		case err == nil:
			wait = t.BackoffBase
		default:
			wait = Backoff(attempt, t.BackoffBase, t.BackoffMax)
			attempt++
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-d.kick:
			timer.Stop()
			attempt, successes, failed = 0, 0, true
			d.record(nil)
		case <-timer.C:
		}
	}
}

// record logs and fires hooks when the observed state differs from the last
// reported one. probeErr is the latest probe error (nil on success or kick).
func (d *Dependency) record(probeErr error) {
	up := d.up.Load()
	d.mu.Lock()
	if probeErr != nil {
		d.lastErr = probeErr
		d.failures++
	} else if up {
		d.failures = 0
	}
	if d.initialized && d.reported == up {
		d.mu.Unlock()
		return
	}
	first := !d.initialized
	d.initialized, d.reported, d.since = true, up, time.Now()
	var hooks []func()
	if up {
		if !first {
			d.toUp++
			hooks = append(hooks, d.onUp...)
		}
	} else if !first {
		d.toDown++
		hooks = append(hooks, d.onDown...)
	}
	lastErr := d.lastErr
	d.mu.Unlock()

	log := d.sup.log.With("dependency", d.name, "class", d.class.String())
	switch {
	case up:
		log.Info("dependency up")
	case d.class == Optional:
		log.Warn("dependency down; running degraded", "error", errString(lastErr))
	default:
		log.Error("dependency down", "error", errString(lastErr))
	}
	for _, h := range hooks {
		h()
	}
}

// Status is a point-in-time view of one dependency.
type Status struct {
	Name                string    `json:"name"`
	Class               string    `json:"class"`
	Up                  bool      `json:"up"`
	Since               time.Time `json:"since"`
	LastError           string    `json:"last_error,omitempty"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	TransitionsUp       int64     `json:"transitions_up"`
	TransitionsDown     int64     `json:"transitions_down"`
}

func (d *Dependency) Status() Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := Status{Name: d.name, Class: d.class.String(), Up: d.up.Load(), Since: d.since,
		ConsecutiveFailures: d.failures, TransitionsUp: d.toUp, TransitionsDown: d.toDown}
	if !st.Up {
		st.LastError = errString(d.lastErr)
	}
	return st
}

// Statuses returns every dependency's status in registration order.
func (s *Supervisor) Statuses() []Status {
	s.mu.Lock()
	deps := append([]*Dependency(nil), s.deps...)
	s.mu.Unlock()
	out := make([]Status, len(deps))
	for i, d := range deps {
		out[i] = d.Status()
	}
	return out
}

// Retry calls fn until it succeeds or ctx ends, sleeping with full-jitter
// capped exponential backoff between attempts. Use it for required
// dependencies at startup instead of exiting.
func (s *Supervisor) Retry(ctx context.Context, name string, fn func(context.Context) error) error {
	t := s.timing
	for attempt := 0; ; attempt++ {
		err := fn(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		wait := Backoff(attempt, t.BackoffBase, t.BackoffMax)
		s.log.Warn("waiting for dependency", "dependency", name, "attempt", attempt+1, "retry_in", wait.Round(time.Millisecond), "error", err.Error())
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// IsConnectivity reports whether err means the remote could not be reached or
// did not answer in time (as opposed to an application-level error).
func IsConnectivity(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	var oe *net.OpError
	var de *net.DNSError
	switch {
	case errors.As(err, &oe), errors.As(err, &de):
		return true
	case errors.As(err, &ne) && ne.Timeout():
		return true
	}
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, net.ErrClosed)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
