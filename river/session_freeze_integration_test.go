package river

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	riverqueue "github.com/riverqueue/river"
)

// freezer is a TCP proxy whose freeze stalls every open link without closing
// it, as a silent network drop does; links opened later pass.
type freezer struct {
	ln       net.Listener
	target   string
	accepted atomic.Int32
	frozenTo atomic.Int32
	done     chan struct{}
	mu       sync.Mutex
	conns    []net.Conn
}

func newFreezer(t *testing.T, target string) *freezer {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &freezer{ln: ln, target: target, done: make(chan struct{})}
	t.Cleanup(func() {
		close(f.done)
		_ = ln.Close()
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, c := range f.conns {
			_ = c.Close()
		}
	})
	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			server, err := net.Dial("tcp", target)
			if err != nil {
				_ = client.Close()
				continue
			}
			f.mu.Lock()
			f.conns = append(f.conns, client, server)
			f.mu.Unlock()
			link := f.accepted.Add(1)
			go f.pipe(server, client, link)
			go f.pipe(client, server, link)
		}
	}()
	return f
}

func (f *freezer) pipe(dst io.Writer, src io.Reader, link int32) {
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if link <= f.frozenTo.Load() {
			<-f.done
			return
		}
		if n > 0 {
			if _, err := dst.Write(buf[:n]); err != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (f *freezer) freeze() { f.frozenTo.Store(f.accepted.Load()) }

func (f *freezer) port() uint16 { return uint16(f.ln.Addr().(*net.TCPAddr).Port) }

// A session listener's link stalls without a reset: the ping fails within its
// bound, and River's notifier redials and hears the next insert at once.
func TestSessionListenerSurvivesAFrozenLink(t *testing.T) {
	base := migrationTestPool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	const schema = "river_frozen"
	if err := ApplyMigrations(ctx, base, MigrationOptions{Schema: schema}); err != nil {
		t.Fatal(err)
	}
	cfg := base.Config().ConnConfig
	proxy := newFreezer(t, net.JoinHostPort(cfg.Host, strconv.Itoa(int(cfg.Port))))
	session := base.Config()
	session.ConnConfig.Host, session.ConnConfig.Port, session.ConnConfig.Fallbacks = "127.0.0.1", proxy.port(), nil

	t.Run("ping", func(t *testing.T) {
		l := &sessionListener{cfg: session, schema: schema}
		if err := l.Connect(ctx); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = l.Close(context.Background()) }()
		if err := l.Ping(ctx); err != nil {
			t.Fatal("ping on a live link:", err)
		}
		proxy.freeze()
		started := time.Now()
		err := l.Ping(context.Background())
		if waited := time.Since(started); err == nil || waited > pingTimeout+5*time.Second {
			t.Fatalf("ping on a frozen link returned %v after %s", err, waited)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Logf("ping failed with %v", err)
		}
	})

	t.Run("notifier_redials", func(t *testing.T) {
		work := base.Config()
		work.MaxConns = 4
		work.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheDescribe
		pool, err := pgxpool.NewWithConfig(ctx, work)
		if err != nil {
			t.Fatal(err)
		}
		defer pool.Close()
		started := make(chan burstStart, 4)
		// A one-minute poll: a job that starts sooner was woken by its notification.
		client, err := NewWithSession(ctx, pool, session, &riverqueue.Config{Schema: schema, FetchPollInterval: time.Minute},
			NewContribution("burst", func(_ context.Context, cfg *riverqueue.Config) error {
				cfg.Queues["burst"] = riverqueue.QueueConfig{MaxWorkers: 1}
				return riverqueue.AddWorkerSafely(cfg.Workers, &burstWorker{started: started})
			}, nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		if err := client.Start(ctx); err != nil {
			t.Fatal(err)
		}
		defer func() {
			stop, done := context.WithTimeout(context.Background(), 10*time.Second)
			defer done()
			_ = client.Stop(stop)
		}()
		run := func(n int) time.Duration {
			inserted := time.Now()
			if _, err := client.Insert(ctx, burstArgs{N: n}, &riverqueue.InsertOpts{Queue: "burst"}); err != nil {
				t.Fatal(err)
			}
			select {
			case s := <-started:
				return s.at.Sub(inserted)
			case <-ctx.Done():
				t.Fatal("job never started")
				return 0
			}
		}
		run(0)
		time.Sleep(time.Second)
		if waited := run(1); waited > 2*time.Second {
			t.Fatalf("job waited %s before the freeze: the listener heard nothing", waited)
		}
		links := proxy.accepted.Load()
		proxy.freeze()
		deadline := time.Now().Add(30 * time.Second)
		for proxy.accepted.Load() == links {
			if time.Now().After(deadline) {
				t.Fatal("the notifier never redialed its frozen listener")
			}
			time.Sleep(100 * time.Millisecond)
		}
		time.Sleep(time.Second) // its LISTENs follow the new connection
		if waited := run(2); waited > 2*time.Second {
			t.Fatalf("job waited %s after the redial: the new listener heard nothing", waited)
		}
	})
}
