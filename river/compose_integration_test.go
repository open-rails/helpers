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

func TestOneFleetPersistsBeforeStartAndPreservesHostPool(t *testing.T) {
	dsn := os.Getenv("RIVER_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set RIVER_TEST_DATABASE_URL for owned PostgreSQL integration")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	schema := "riverkit_" + strings.ToLower(rand.Text())
	defer func() {
		if _, err := pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			t.Error(err)
		}
	}()
	options := &riverqueue.Config{Schema: schema}
	var firstProducer, secondProducer *riverqueue.Client[pgx.Tx]
	first := NewContribution("first", firstRegistration, func(_ context.Context, binding Binding) error {
		c := binding.Client
		if binding.Pool != pool {
			t.Fatal("binding did not carry the actual host pool")
		}
		firstProducer = c
		return nil
	}, func() error { firstProducer = nil; return nil })
	second := NewContribution("second", func(_ context.Context, cfg *riverqueue.Config) error {
		cfg.Queues["second"] = riverqueue.QueueConfig{MaxWorkers: 1}
		return riverqueue.AddWorkerSafely(cfg.Workers, &secondWorker{})
	}, func(_ context.Context, binding Binding) error {
		c := binding.Client
		if binding.Pool != pool {
			t.Fatal("binding did not carry the actual host pool")
		}
		secondProducer = c
		return nil
	}, func() error { secondProducer = nil; return nil })
	// Composition succeeds before the host has even created River tables.
	client, err := New(ctx, pool, options, Group(first, second))
	if err != nil {
		t.Fatal(err)
	}
	if firstProducer != client || secondProducer != client || client.Stopped() != nil {
		t.Fatal("composition did not return the one unstarted bound client")
	}
	if err := ApplyMigrations(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	events, unsubscribe := client.Subscribe(riverqueue.EventKindJobCompleted)
	defer unsubscribe()
	if _, err := firstProducer.Insert(ctx, firstArgs{}, &riverqueue.InsertOpts{Queue: "first"}); err != nil {
		t.Fatal(err)
	}
	if _, err := secondProducer.Insert(ctx, secondArgs{}, &riverqueue.InsertOpts{Queue: "second"}); err != nil {
		t.Fatal(err)
	}
	var stored int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{schema, "river_job"}.Sanitize()+" WHERE state='available'").Scan(&stored); err != nil || stored != 2 {
		t.Fatalf("producer jobs not durably waiting before Start: %d %v", stored, err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := client.StopAndCancel(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	completed := map[string]bool{}
	for len(completed) < 2 {
		select {
		case event := <-events:
			if event != nil {
				completed[event.Job.Kind] = true
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if err := client.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := pool.Ping(ctx); err != nil {
		t.Fatal("fleet stopped its host pool:", err)
	}
	// A later composition failure must revoke its partial producers, while the
	// caller-owned pool remains usable.
	firstProducer = nil
	retry := NewContribution("retry", firstRegistration, func(_ context.Context, binding Binding) error {
		c := binding.Client
		if binding.Pool != pool {
			t.Fatal("binding did not carry the actual host pool")
		}
		firstProducer = c
		return nil
	}, func() error { firstProducer = nil; return nil })
	fail := NewContribution("fail", func(context.Context, *riverqueue.Config) error { return nil }, func(context.Context, Binding) error { return errors.New("injected bind failure") }, func() error { return nil })
	if failed, err := New(ctx, pool, options, retry, fail); err == nil || failed != nil || firstProducer != nil {
		t.Fatalf("failed composition left a producer: %v %v", failed, err)
	}
	if err := pool.Ping(ctx); err != nil {
		t.Fatal("failed composition closed its host pool:", err)
	}
}
