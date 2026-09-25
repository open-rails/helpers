package deps

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Probe endpoint paths.
const (
	LivePath    = "/livez"
	ReadyPath   = "/readyz"
	StatusPath  = "/statusz"
	MetricsPath = "/metrics"
)

// Livez answers 200 whenever the process can serve HTTP. It never checks
// dependencies: restarting a process does not fix a missing dependency.
func (s *Supervisor) Livez(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// Readyz answers 200 once the process is built and until it starts draining.
// Shared dependencies are deliberately not consulted: when one is down every
// replica would fail together and the ingress would have nothing to route to.
func (s *Supervisor) Readyz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	switch {
	case s.draining.Load():
		http.Error(w, "draining", http.StatusServiceUnavailable)
	case !s.ready.Load():
		http.Error(w, "starting", http.StatusServiceUnavailable)
	default:
		_, _ = w.Write([]byte("ok\n"))
	}
}

// Statusz reports every dependency as JSON. Always 200.
func (s *Supervisor) Statusz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(struct {
		Ready        bool     `json:"ready"`
		Draining     bool     `json:"draining"`
		Dependencies []Status `json:"dependencies"`
	}{s.ready.Load(), s.draining.Load(), s.Statuses()})
}

// Counter is a monotonically increasing value exported on /metrics.
type Counter struct {
	name, help string
	v          atomic.Int64
}

func (c *Counter) Add(n int64)  { c.v.Add(n) }
func (c *Counter) Inc()         { c.v.Add(1) }
func (c *Counter) Value() int64 { return c.v.Load() }

// Counter registers (or returns the existing) counter exported on /metrics.
// name must be a valid Prometheus metric name ending in _total.
func (s *Supervisor) Counter(name, help string) *Counter {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.counters {
		if c.name == name {
			return c
		}
	}
	c := &Counter{name: name, help: help}
	s.counters = append(s.counters, c)
	return c
}

// Label is one Prometheus label of a Sample.
type Label struct{ Name, Value string }

// Sample is one labelled value of a GaugeFunc.
type Sample struct {
	Labels []Label
	Value  float64
}

type gaugeFunc struct {
	name, help string
	fn         func() []Sample
}

// GaugeFunc exports fn's samples as a gauge on /metrics, read at scrape time
// (e.g. the age of a peer's cached keys). name must be a valid Prometheus
// metric name; fn must be cheap and safe for concurrent use.
func (s *Supervisor) GaugeFunc(name, help string, fn func() []Sample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gauges = append(s.gauges, gaugeFunc{name: name, help: help, fn: fn})
}

// Metrics writes Prometheus text exposition.
func (s *Supervisor) Metrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	var b strings.Builder
	b.WriteString("# HELP app_ready Whether the process is built and not draining.\n# TYPE app_ready gauge\n")
	fmt.Fprintf(&b, "app_ready %d\n", boolInt(s.Ready()))
	st := s.Statuses()
	b.WriteString("# HELP app_dependency_up Whether the dependency is usable (0 means degraded or unavailable).\n# TYPE app_dependency_up gauge\n")
	for _, d := range st {
		fmt.Fprintf(&b, "app_dependency_up{dependency=%q,class=%q} %d\n", d.Name, d.Class, boolInt(d.Up))
	}
	b.WriteString("# HELP app_dependency_transitions_total Dependency state transitions.\n# TYPE app_dependency_transitions_total counter\n")
	for _, d := range st {
		fmt.Fprintf(&b, "app_dependency_transitions_total{dependency=%q,to=\"up\"} %d\n", d.Name, d.TransitionsUp)
		fmt.Fprintf(&b, "app_dependency_transitions_total{dependency=%q,to=\"down\"} %d\n", d.Name, d.TransitionsDown)
	}
	s.mu.Lock()
	counters := slices.Clone(s.counters)
	s.mu.Unlock()
	for _, c := range counters {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", c.name, c.help, c.name, c.name, c.Value())
	}
	s.mu.Lock()
	gauges := slices.Clone(s.gauges)
	s.mu.Unlock()
	for _, g := range gauges {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", g.name, g.help, g.name)
		for _, sm := range g.fn() {
			b.WriteString(g.name)
			if len(sm.Labels) > 0 {
				b.WriteByte('{')
				for i, l := range sm.Labels {
					if i > 0 {
						b.WriteByte(',')
					}
					fmt.Fprintf(&b, "%s=%q", l.Name, l.Value)
				}
				b.WriteByte('}')
			}
			fmt.Fprintf(&b, " %s\n", strconv.FormatFloat(sm.Value, 'g', -1, 64))
		}
	}
	_, _ = w.Write([]byte(b.String()))
}

// OpsHandler serves /livez, /readyz, /statusz and /metrics, for a listener that
// is not exposed through the ingress.
func (s *Supervisor) OpsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(LivePath, s.Livez)
	mux.HandleFunc(ReadyPath, s.Readyz)
	mux.HandleFunc(StatusPath, s.Statusz)
	mux.HandleFunc(MetricsPath, s.Metrics)
	return mux
}

// Gate is the handler for the application listener. It answers /livez and
// /readyz itself and returns 503 for everything else until Open installs the
// application, so the listener can bind before the application is built.
type Gate struct {
	sup *Supervisor
	mu  sync.RWMutex
	app http.Handler
}

func (s *Supervisor) Gate() *Gate { return &Gate{sup: s} }

// Open installs the application handler and marks the process ready.
func (g *Gate) Open(app http.Handler) {
	g.mu.Lock()
	g.app = app
	g.mu.Unlock()
	g.sup.SetReady()
}

func (g *Gate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case LivePath:
		g.sup.Livez(w, r)
		return
	case ReadyPath:
		g.sup.Readyz(w, r)
		return
	}
	g.mu.RLock()
	app := g.app
	g.mu.RUnlock()
	if app == nil {
		w.Header().Set("Retry-After", "5")
		http.Error(w, "starting", http.StatusServiceUnavailable)
		return
	}
	app.ServeHTTP(w, r)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
