package river

import (
	"context"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	riverqueue "github.com/riverqueue/river"
)

// The client works through pool and LISTENs on a connection of its own dialed
// from session: a job inserted while the backstop poll is 10 s away starts
// within a second, and the session's one backend leaves with the client.
func TestNewWithSessionListensOnItsOwnConnection(t *testing.T) {
	base := migrationTestPool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	const schema = "river_session"
	if err := ApplyMigrations(ctx, base, MigrationOptions{Schema: schema}); err != nil {
		t.Fatal(err)
	}
	started := make(chan burstStart, 4)
	burst := func() Contribution {
		return NewContribution("burst", func(_ context.Context, cfg *riverqueue.Config) error {
			cfg.Queues["burst"] = riverqueue.QueueConfig{MaxWorkers: 2}
			return riverqueue.AddWorkerSafely(cfg.Workers, &burstWorker{started: started})
		}, nil, nil)
	}
	name := "riverkit_session_" + strings.ToLower(rand.Text())[:8]
	session := base.Config()
	session.ConnConfig.RuntimeParams["application_name"] = name
	options := &riverqueue.Config{Schema: schema, FetchPollInterval: 10 * time.Second}

	// base keeps pgx's default named statements, which a pooler breaks.
	if _, err := NewWithSession(ctx, base, session, options, burst()); err == nil || !strings.Contains(err.Error(), "cache_describe") {
		t.Fatalf("a pooled pool with named statements was accepted: %v", err)
	}

	work := base.Config()
	work.MaxConns = 4
	work.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheDescribe
	pool, err := pgxpool.NewWithConfig(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	client, err := NewWithSession(ctx, pool, session, options, burst())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		stopCtx, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if err := client.Stop(stopCtx); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(stop)
	// River fetches once at start; the warm-up job also proves the LISTEN is up.
	if _, err := client.Insert(ctx, burstArgs{N: 0}, &riverqueue.InsertOpts{Queue: "burst"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("warm-up job never started")
	}
	time.Sleep(200 * time.Millisecond)
	inserted := time.Now()
	if _, err := client.Insert(ctx, burstArgs{N: 1}, &riverqueue.InsertOpts{Queue: "burst"}); err != nil {
		t.Fatal(err)
	}
	select {
	case s := <-started:
		if waited := s.at.Sub(inserted); waited > time.Second {
			t.Fatalf("job waited %s for the poll: the session listener heard nothing", waited)
		}
	case <-ctx.Done():
		t.Fatal("job never started")
	}

	sessions := func() (n int) {
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND application_name = $1", name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := sessions(); n != 1 {
		t.Fatalf("%d session backends while the client runs, want its one listener", n)
	}
	stop()
	for deadline := time.Now().Add(5 * time.Second); sessions() != 0; time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the listener's session outlived the client")
		}
	}
}
