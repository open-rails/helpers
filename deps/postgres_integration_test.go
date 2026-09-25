package deps

import (
	"context"
	"os"
	"testing"

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

	probe := PostgresProbe(cfg.ConnConfig)
	pctx, cancel := context.WithTimeout(ctx, fastTiming.Timeout)
	defer cancel()
	if err := probe(pctx); err != nil {
		t.Fatalf("dedicated probe failed under pool saturation: %v", err)
	}
	if err := pool.Ping(pctx); err == nil {
		t.Fatal("expected the saturated pool itself to time out")
	}

	bad, _ := pgx.ParseConfig("postgres://x:y@127.0.0.1:1/db?connect_timeout=1")
	err = PostgresProbe(bad)(ctx)
	if err == nil || !PostgresUnavailable(err) {
		t.Fatalf("refused connection classified %v", err)
	}
	if PostgresUnavailable(context.Canceled) {
		t.Fatal("cancellation classified as unavailable")
	}
}
