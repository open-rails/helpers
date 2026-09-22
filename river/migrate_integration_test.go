package river

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	riverqueue "github.com/riverqueue/river"
)

func TestApplyMigrationsConcurrentOneConnectionPool(t *testing.T) {
	for _, schema := range []string{"", "river_" + strings.Repeat("a", 40)} {
		t.Run(schema, func(t *testing.T) {
			pool := migrationTestPool(t)
			ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
			defer cancel()
			if _, err := pool.Exec(ctx, "CREATE TABLE public.application_marker (id int PRIMARY KEY); INSERT INTO public.application_marker VALUES (42)"); err != nil {
				t.Fatal(err)
			}
			start := make(chan struct{})
			results := make(chan error, 6)
			for i := range 6 {
				go func() {
					<-start
					selected := schema
					if selected == "" && i%2 == 0 {
						selected = "public"
					}
					results <- ApplyMigrations(ctx, pool, selected)
				}()
			}
			close(start)
			for range 6 {
				if err := <-results; err != nil {
					t.Fatal("concurrent migration on one-slot pool:", err)
				}
			}
			if err := ApplyMigrations(ctx, pool, schema); err != nil {
				t.Fatal("idempotent migration:", err)
			}
			client, err := New(ctx, pool, &riverqueue.Config{Schema: schema}, NewContribution("migration-proof", firstRegistration, nil, nil))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.Insert(ctx, firstArgs{}, &riverqueue.InsertOpts{Queue: "first"}); err != nil {
				t.Fatal("migrations did not initialize the composer's schema:", err)
			}
			selected := schema
			if selected == "" {
				selected = "public"
			}
			var jobs, marker int
			if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{selected, "river_job"}.Sanitize()).Scan(&jobs); err != nil || jobs != 1 {
				t.Fatalf("wrong target schema: jobs=%d err=%v", jobs, err)
			}
			if err := pool.QueryRow(ctx, "SELECT id FROM public.application_marker").Scan(&marker); err != nil || marker != 42 {
				t.Fatalf("application data changed: marker=%d err=%v", marker, err)
			}
			if err := pool.Ping(ctx); err != nil {
				t.Fatal("migration helper closed the host pool:", err)
			}
		})
	}
}

func TestApplyMigrationsLocksBeforeSchemaCreationAndCancels(t *testing.T) {
	pool := migrationTestPool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	blocker, err := pgx.ConnectConfig(ctx, pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close(context.Background())
	const schema = "river_waiting"
	const key = "river-migrations:" + schema
	if _, err := blocker.Exec(ctx, "SELECT pg_advisory_lock(hashtext(current_database()),hashtext($1))", key); err != nil {
		t.Fatal(err)
	}
	waiting, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- ApplyMigrations(waiting, pool, schema) }()
	for {
		var blocked bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND NOT granted AND classid=hashtext(current_database())::oid AND objid=hashtext($1)::oid)`, key).Scan(&blocked); err != nil {
			stop()
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case err := <-done:
			stop()
			t.Fatalf("migration bypassed the shared lock: %v", err)
		case <-ctx.Done():
			stop()
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	var exists bool
	if err := pool.QueryRow(ctx, "SELECT to_regnamespace($1) IS NOT NULL", schema).Scan(&exists); err != nil || exists {
		stop()
		t.Fatalf("schema was created before acquiring the lock: exists=%v err=%v", exists, err)
	}
	stop()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting migration ignored cancellation: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		t.Fatal("cancellation closed or occupied the host pool:", err)
	}
	if _, err := blocker.Exec(ctx, "SELECT pg_advisory_unlock(hashtext(current_database()),hashtext($1))", key); err != nil {
		t.Fatal(err)
	}
	if err := ApplyMigrations(ctx, pool, schema); err != nil {
		t.Fatal("canceled migration prevented a subsequent initializer:", err)
	}
}

func migrationTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("RIVER_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set RIVER_TEST_DATABASE_URL for owned PostgreSQL integration")
	}
	admin, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	name := "helpers_migrate_" + strings.ToLower(rand.Text())
	if _, err := admin.Exec(t.Context(), "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal("create owned migration test database:", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Error("remove owned test database:", err)
		}
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = name
	// Empty schema must mean public even if the host uses another search path.
	cfg.ConnConfig.RuntimeParams["search_path"] = "pg_catalog"
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}
