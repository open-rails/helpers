package deps

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// outageProxy forwards TCP to a real Redis and can cut every connection and
// refuse new ones, which is what a restarting Redis pod looks like to clients.
type outageProxy struct {
	ln     net.Listener
	target string
	down   atomic.Bool
	mu     sync.Mutex
	conns  []net.Conn
}

func newOutageProxy(t *testing.T, target string) *outageProxy {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &outageProxy{ln: ln, target: target}
	go p.serve()
	t.Cleanup(func() { _ = ln.Close(); p.cut() })
	return p
}

func (p *outageProxy) addr() string { return p.ln.Addr().String() }

func (p *outageProxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		if p.down.Load() {
			_ = c.Close()
			continue
		}
		u, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = c.Close()
			continue
		}
		p.mu.Lock()
		p.conns = append(p.conns, c, u)
		p.mu.Unlock()
		go func() { _, _ = io.Copy(u, c); _ = u.Close() }()
		go func() { _, _ = io.Copy(c, u); _ = c.Close() }()
	}
}

func (p *outageProxy) cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		_ = c.Close()
	}
	p.conns = nil
}

func (p *outageProxy) setDown(down bool) {
	p.down.Store(down)
	if down {
		p.cut()
	}
}

func testRedisAddr(t *testing.T) string {
	addr := os.Getenv("DEPS_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("set DEPS_TEST_REDIS_ADDR to a Redis for integration tests")
	}
	return addr
}

var fastTiming = Timing{Interval: 50 * time.Millisecond, Timeout: 200 * time.Millisecond, BackoffBase: 20 * time.Millisecond, BackoffMax: 100 * time.Millisecond, RecoverAfter: 2}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// counterStore is the kind of backend a Switch selects between.
type counterStore interface {
	Incr(ctx context.Context, key string) (int64, error)
}

type redisCounter struct{ c redis.UniversalClient }

func (r redisCounter) Incr(ctx context.Context, key string) (int64, error) {
	return r.c.Incr(ctx, key).Result()
}

type memCounter struct {
	mu sync.Mutex
	m  map[string]int64
}

func (m *memCounter) Incr(_ context.Context, key string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.m[key]++
	return m.m[key], nil
}

func TestRedisOutageFallsBackAndRecovers(t *testing.T) {
	proxy := newOutageProxy(t, testRedisAddr(t))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	sup := New(WithTiming(fastTiming))
	client := NewRedis(RedisConfig{Addrs: []string{proxy.addr()}})
	defer client.Close()
	dep := sup.AddRedis("redis", client)
	var ups, downs atomic.Int32
	dep.OnUp(func(context.Context) { ups.Add(1) })
	dep.OnDown(func(context.Context) { downs.Add(1) })
	sw := NewSwitch[counterStore](dep, redisCounter{client}, func() counterStore { return &memCounter{m: map[string]int64{}} })
	sup.Start(ctx)

	key := "deps-test:" + strconv.FormatInt(time.Now().UnixNano(), 36)
	defer client.Del(context.Background(), key)
	incr := func() (int64, bool) {
		_, primary := sw.Get()
		n, err := Call(sw, func(s counterStore) (int64, error) { return s.Incr(ctx, key) })
		if err != nil {
			t.Fatalf("incr: %v", err)
		}
		return n, primary
	}

	eventually(t, "redis up", dep.Up)
	if n, primary := incr(); !primary || n != 1 {
		t.Fatalf("first incr = %d primary=%v", n, primary)
	}

	// Outage: the first failing call is served by the fallback, not an error.
	proxy.setDown(true)
	if n, _ := incr(); n != 1 {
		t.Fatalf("fallback incr = %d, want fresh fallback count 1", n)
	}
	if dep.Up() {
		t.Fatal("dependency still up after an unavailable call")
	}
	if n, primary := incr(); primary || n != 2 {
		t.Fatalf("second fallback incr = %d primary=%v", n, primary)
	}
	eventually(t, "down hook", func() bool { return downs.Load() == 1 })

	// Stays down while probes keep failing.
	time.Sleep(200 * time.Millisecond)
	if dep.Up() {
		t.Fatal("up while redis unreachable")
	}
	if st := dep.Status(); st.LastError == "" || st.ConsecutiveFailures == 0 {
		t.Fatalf("status %+v", st)
	}

	// Recovery: back to Redis, fallback state discarded.
	proxy.setDown(false)
	eventually(t, "redis recovered", dep.Up)
	eventually(t, "up hook", func() bool { return ups.Load() == 1 })
	if n, primary := incr(); !primary || n != 2 {
		t.Fatalf("incr after recovery = %d primary=%v, want redis count 2", n, primary)
	}
	proxy.setDown(true)
	if n, _ := incr(); n != 1 {
		t.Fatalf("fallback after second outage = %d, want fresh fallback", n)
	}
}

func TestUnresolvableRedisNeverUpAndNeverFails(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sup := New(WithTiming(fastTiming))
	client := NewRedis(RedisConfig{Addrs: []string{"redis-v1.deps-test.invalid:6379"}})
	defer client.Close()
	dep := sup.AddRedis("redis", client)
	sw := NewSwitch[counterStore](dep, redisCounter{client}, func() counterStore { return &memCounter{m: map[string]int64{}} })
	sup.Start(ctx)
	eventually(t, "several failed probes", func() bool { return dep.Status().ConsecutiveFailures >= 3 })
	if dep.Up() {
		t.Fatal("unresolvable redis reported up")
	}
	n, err := Call(sw, func(s counterStore) (int64, error) { return s.Incr(ctx, "k") })
	if err != nil || n != 1 {
		t.Fatalf("fallback incr = %d, %v", n, err)
	}
	if !RedisUnavailable(client.Ping(ctx).Err()) {
		t.Fatal("NXDOMAIN not classified as unavailable")
	}
}

func TestProbeEndpoints(t *testing.T) {
	proxy := newOutageProxy(t, testRedisAddr(t))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sup := New(WithTiming(fastTiming))
	client := NewRedis(RedisConfig{Addrs: []string{proxy.addr()}})
	defer client.Close()
	dep := sup.AddRedis("redis", client)
	dropped := sup.Counter("app_signals_dropped_total", "Signals dropped while ClickHouse was down.")
	sup.Start(ctx)
	gate := sup.Gate()
	srv := httptest.NewServer(gate)
	defer srv.Close()
	ops := httptest.NewServer(sup.OpsHandler())
	defer ops.Close()

	get := func(base, path string) (int, string) {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	expect := func(base, path string, code int) string {
		t.Helper()
		got, body := get(base, path)
		if got != code {
			t.Fatalf("%s = %d (%s), want %d", path, got, body, code)
		}
		return body
	}

	expect(srv.URL, LivePath, 200)
	expect(srv.URL, ReadyPath, 503)
	expect(srv.URL, "/api/anything", 503)

	gate.Open(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("app")) }))
	expect(srv.URL, ReadyPath, 200)
	if body := expect(srv.URL, "/api/anything", 200); body != "app" {
		t.Fatalf("app body %q", body)
	}
	eventually(t, "redis up", dep.Up)

	// A down optional dependency never affects liveness or readiness.
	proxy.setDown(true)
	eventually(t, "redis down", func() bool { return !dep.Up() })
	expect(srv.URL, LivePath, 200)
	expect(srv.URL, ReadyPath, 200)
	dropped.Add(3)
	metrics := expect(ops.URL, MetricsPath, 200)
	for _, want := range []string{
		`app_dependency_up{dependency="redis",class="optional"} 0`,
		`app_dependency_transitions_total{dependency="redis",to="down"} 1`,
		"app_ready 1",
		"app_signals_dropped_total 3",
	} {
		if !strings.Contains(metrics, want) {
			t.Fatalf("metrics missing %q:\n%s", want, metrics)
		}
	}
	if status := expect(ops.URL, StatusPath, 200); !strings.Contains(status, `"up":false`) || !strings.Contains(status, `"last_error"`) {
		t.Fatalf("status %s", status)
	}

	sup.Drain()
	expect(srv.URL, ReadyPath, 503)
	expect(srv.URL, LivePath, 200)
}

func TestRetryWaitsForRequiredDependency(t *testing.T) {
	proxy := newOutageProxy(t, testRedisAddr(t))
	proxy.setDown(true)
	sup := New(WithTiming(fastTiming))
	client := NewRedis(RedisConfig{Addrs: []string{proxy.addr()}})
	defer client.Close()
	go func() { time.Sleep(300 * time.Millisecond); proxy.setDown(false) }()
	attempts := 0
	err := sup.Retry(t.Context(), "postgres", func(ctx context.Context) error {
		attempts++
		return client.Ping(ctx).Err()
	})
	if err != nil || attempts < 2 {
		t.Fatalf("retry = %v after %d attempts", err, attempts)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := sup.Retry(ctx, "never", func(context.Context) error { return errors.New("no") }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("retry after ctx end = %v", err)
	}
}

// A primary that refuses writes (min-replicas-to-write with no replica) is
// down for the data path and for the probe, so callers stay on the fallback.
func TestRedisRefusingWritesIsDown(t *testing.T) {
	addr := testRedisAddr(t)
	admin := redis.NewClient(&redis.Options{Addr: addr})
	defer admin.Close()
	ctx := t.Context()
	if err := admin.ConfigSet(ctx, "min-replicas-to-write", "1").Err(); err != nil {
		t.Fatal(err)
	}
	defer admin.ConfigSet(context.Background(), "min-replicas-to-write", "0")

	sup := New(WithTiming(fastTiming))
	client := NewRedis(RedisConfig{Addrs: []string{addr}})
	defer client.Close()
	dep := sup.AddRedis("redis", client)
	sw := NewSwitch[counterStore](dep, redisCounter{client}, func() counterStore { return &memCounter{m: map[string]int64{}} })
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	sup.Start(runCtx)
	eventually(t, "failed probes", func() bool { return dep.Status().ConsecutiveFailures >= 2 })
	if dep.Up() || !strings.Contains(dep.Status().LastError, "NOREPLICAS") {
		t.Fatalf("status %+v", dep.Status())
	}
	if !RedisUnavailable(client.Incr(ctx, "deps-test:noreplicas").Err()) {
		t.Fatal("NOREPLICAS not classified as unavailable")
	}
	n, err := Call(sw, func(s counterStore) (int64, error) { return s.Incr(ctx, "k") })
	if err != nil || n != 1 {
		t.Fatalf("fallback incr = %d, %v", n, err)
	}
	admin.ConfigSet(ctx, "min-replicas-to-write", "0")
	eventually(t, "writable again", dep.Up)
}

// DEPS_TEST_SENTINEL_ADDR points at a Sentinel monitoring master "mymaster".
func TestSentinelConfigFollowsMaster(t *testing.T) {
	sentinel := os.Getenv("DEPS_TEST_SENTINEL_ADDR")
	if sentinel == "" {
		t.Skip("set DEPS_TEST_SENTINEL_ADDR for the Sentinel integration test")
	}
	cfg := RedisConfig{MasterName: "mymaster", SentinelAddrs: []string{sentinel}}
	if err := cfg.Validate(); err != nil || !cfg.Configured() {
		t.Fatalf("config: %v", err)
	}
	client := NewRedis(cfg)
	defer client.Close()
	sup := New(WithTiming(fastTiming))
	dep := sup.AddRedis("redis", client)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sup.Start(ctx)
	eventually(t, "sentinel-resolved master up", dep.Up)
	if err := client.Set(ctx, "deps-test:sentinel", "1", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
}

func TestRedisConfigValidate(t *testing.T) {
	for _, c := range []RedisConfig{
		{MasterName: "redis"},
		{SentinelAddrs: []string{"s:26379"}},
	} {
		if c.Validate() == nil {
			t.Fatalf("%+v accepted", c)
		}
	}
	if (RedisConfig{Addrs: []string{" "}}).Configured() {
		t.Fatal("blank address counted as configured")
	}
}
