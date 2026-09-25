package deps

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The Postgres probe uses its own connection: a saturated application pool
// does not read as Postgres being down.
func TestPostgresProbeIgnoresSaturatedPool(t *testing.T) {
	dsn := os.Getenv("RIVER_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set RIVER_TEST_DATABASE_URL")
	}
	ctx := t.Context()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	held, err := pool.Acquire(ctx) // the app pool is exhausted
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	probe, closeProbe := PostgresProbe(cfg.ConnConfig)
	defer closeProbe()
	pctx, cancel := context.WithTimeout(ctx, fastTiming.Timeout)
	defer cancel()
	if err := probe(pctx); err != nil {
		t.Fatalf("dedicated probe failed under pool saturation: %v", err)
	}
	if err := pool.Ping(pctx); err == nil {
		t.Fatal("expected the saturated pool itself to time out")
	}

	bad, _ := pgx.ParseConfig("postgres://x:y@127.0.0.1:1/db?connect_timeout=1")
	badProbe, _ := PostgresProbe(bad)
	err = badProbe(ctx)
	if err == nil || !PostgresUnavailable(err) {
		t.Fatalf("refused connection classified %v", err)
	}
	if PostgresUnavailable(context.Canceled) {
		t.Fatal("cancellation classified as unavailable")
	}
}

// Probe connections are released when a dependency is replaced by a
// same-named Add (a retried build) and when the supervisor stops.
func TestPostgresProbeConnectionsAreClosed(t *testing.T) {
	dsn := os.Getenv("RIVER_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set RIVER_TEST_DATABASE_URL")
	}
	ctx := t.Context()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	app := "deps_probe_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	cfg.RuntimeParams["application_name"] = app
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	open := func() int {
		var n int
		if err := admin.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE application_name = $1`, app).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	runCtx, cancel := context.WithCancel(ctx)
	sup := New(WithTiming(fastTiming))
	sup.Start(runCtx)
	var last *Dependency
	for range 5 {
		last = sup.AddPostgres("postgres", cfg)
		eventually(t, "probe connected", last.Up)
	}
	eventually(t, "replaced probes closed", func() bool { return open() == 1 })
	cancel()
	<-last.Done()
	eventually(t, "probe closed on stop", func() bool { return open() == 0 })
}
