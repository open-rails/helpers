package river

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	riverqueue "github.com/riverqueue/river"
)

type periodicTestArgs struct{}

func (periodicTestArgs) Kind() string { return "periodic_checkpoint_test" }

type periodicTestWorker struct {
	riverqueue.WorkerDefaults[periodicTestArgs]
	runs    *atomic.Int64
	started chan time.Time
}

func (w *periodicTestWorker) Work(context.Context, *riverqueue.Job[periodicTestArgs]) error {
	w.runs.Add(1)
	w.started <- time.Now()
	return nil
}

func TestPeriodicRestartAndParallelInstances(t *testing.T) {
	dsn := os.Getenv("RIVER_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set RIVER_TEST_DATABASE_URL for owned PostgreSQL integration")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	schema := "periodic_" + strings.ToLower(rand.Text())
	defer func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	}()
	if err := ApplyMigrations(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	var runs atomic.Int64
	started := make(chan time.Time, 20)
	const every = 2 * time.Second
	newClient := func() *riverqueue.Client[pgx.Tx] {
		t.Helper()
		c, err := New(ctx, pool, &riverqueue.Config{Schema: schema, FetchCooldown: time.Millisecond, FetchPollInterval: 10 * time.Millisecond},
			NewContribution("periodic-test", func(_ context.Context, cfg *riverqueue.Config) error {
				cfg.Queues["periodic"] = riverqueue.QueueConfig{MaxWorkers: 1}
				if err := riverqueue.AddWorkerSafely(cfg.Workers, &periodicTestWorker{runs: &runs, started: started}); err != nil {
					return err
				}
				cfg.PeriodicJobs = append(cfg.PeriodicJobs, NewPeriodicJob(cfg, every, func() (riverqueue.JobArgs, *riverqueue.InsertOpts) {
					return periodicTestArgs{}, &riverqueue.InsertOpts{Queue: "periodic", UniqueOpts: riverqueue.UniqueOpts{ByArgs: true, ByPeriod: every}}
				}, &riverqueue.PeriodicJobOpts{ID: "checkpoint", RunOnStart: true}))
				return nil
			}, nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	stop := func(c *riverqueue.Client[pgx.Tx]) {
		t.Helper()
		if err := c.StopAndCancel(ctx); err != nil {
			t.Fatal(err)
		}
	}
	await := func() time.Time {
		t.Helper()
		select {
		case at := <-started:
			return at
		case <-ctx.Done():
			t.Fatal(ctx.Err())
			return time.Time{}
		}
	}
	first := newClient()
	completed, unsubscribe := first.Subscribe(riverqueue.EventKindJobCompleted)
	defer unsubscribe()
	if err := first.Start(ctx); err != nil {
		t.Fatal(err)
	}
	firstAt := await()
	select {
	case <-completed:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	stop(first)
	// Both replicas recover the same database checkpoint. Only the elected
	// leader schedules the next run, at the old deadline rather than restart+every.
	second, third := newClient(), newClient()
	if err := second.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer stop(second)
	if err := third.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer stop(third)
	select {
	case <-started:
		t.Fatal("restart reran a recent periodic job")
	case <-time.After(200 * time.Millisecond):
	}
	nextAt := await()
	if elapsed := nextAt.Sub(firstAt); elapsed < every-150*time.Millisecond {
		t.Fatalf("restart ran before the deadline: elapsed %s", elapsed)
	}
	select {
	case <-started:
		t.Fatal("parallel instances duplicated the periodic run")
	case <-time.After(200 * time.Millisecond):
	}
	if runs.Load() != 2 {
		t.Fatalf("runs = %d, want 2", runs.Load())
	}
	// Explicit events still run immediately even though periodic work is recent.
	if _, err := second.Insert(ctx, periodicTestArgs{}, &riverqueue.InsertOpts{Queue: "periodic"}); err != nil {
		t.Fatal(err)
	}
	await()
}

func TestPeriodicCheckpointOverdueAndRetainedRecords(t *testing.T) {
	dsn := os.Getenv("RIVER_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set RIVER_TEST_DATABASE_URL for owned PostgreSQL integration")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	schema := "checkpoint_" + strings.ToLower(rand.Text())
	defer func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	}()
	if err := ApplyMigrations(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	// Exercise the actual SQL checkpoint with existing job metadata, including
	// an older legacy kind: the stable periodic ID survives worker renames.
	table := pgx.Identifier{schema, "river_job"}.Sanitize()
	at := time.Now().Add(-5 * time.Hour).Truncate(time.Microsecond)
	metadata, _ := json.Marshal(map[string]any{"river:periodic_job_id": "purge", "periodic": true})
	_, err = pool.Exec(ctx, "INSERT INTO "+table+` (kind, args, queue, state, created_at, scheduled_at, attempted_at, finalized_at, metadata)
		VALUES ('old_purge', '{}', 'default', 'completed', $1, $1, $1, $1, $2)`, at, metadata)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &riverqueue.Config{Schema: schema}
	NewPeriodicJob(cfg, 4*time.Hour, func() (riverqueue.JobArgs, *riverqueue.InsertOpts) { return periodicTestArgs{}, nil }, &riverqueue.PeriodicJobOpts{ID: "purge", RunOnStart: true})
	h := cfg.Hooks[0].(*periodicCheckpoint)
	if err := h.bind(pool, cfg); err != nil {
		t.Fatal(err)
	}
	if err := h.Start(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if !h.jobs[0].last.Equal(at) {
		t.Fatalf("wrong checkpoint: %s", h.jobs[0].last)
	}
	if args, _ := h.jobs[0].construct(); args == nil {
		t.Fatal("overdue purge did not catch up")
	}
}
