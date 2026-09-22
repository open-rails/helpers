package river

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	riverqueue "github.com/riverqueue/river"
)

type firstArgs struct{}

func (firstArgs) Kind() string { return "first" }

type firstWorker struct {
	riverqueue.WorkerDefaults[firstArgs]
}

func (*firstWorker) Work(context.Context, *riverqueue.Job[firstArgs]) error { return nil }

type secondArgs struct{}

func (secondArgs) Kind() string { return "second" }

type secondWorker struct {
	riverqueue.WorkerDefaults[secondArgs]
}

func (*secondWorker) Work(context.Context, *riverqueue.Job[secondArgs]) error { return nil }

func lazyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), "postgres://unused@127.0.0.1:1/unused?sslmode=disable&pool_max_conns=1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}
func firstRegistration(_ context.Context, cfg *riverqueue.Config) error {
	if _, exists := cfg.Queues["first"]; !exists {
		cfg.Queues["first"] = riverqueue.QueueConfig{MaxWorkers: 1}
	}
	return riverqueue.AddWorkerSafely(cfg.Workers, &firstWorker{})
}
func periodic(id string) *riverqueue.PeriodicJob {
	return riverqueue.NewPeriodicJob(riverqueue.PeriodicInterval(time.Hour), func() (riverqueue.JobArgs, *riverqueue.InsertOpts) {
		return firstArgs{}, &riverqueue.InsertOpts{Queue: "first"}
	}, &riverqueue.PeriodicJobOpts{ID: id})
}

func TestComposeCollectsBeforeOneUnstartedClientAndPreservesOptions(t *testing.T) {
	pool := lazyPool(t)
	var order []string
	var bound []*riverqueue.Client[pgx.Tx]
	original := periodic("host")
	options := &riverqueue.Config{Schema: "public", Queues: map[string]riverqueue.QueueConfig{"first": {MaxWorkers: 4}}, PeriodicJobs: []*riverqueue.PeriodicJob{original}}
	first := NewContribution("first", func(ctx context.Context, cfg *riverqueue.Config) error {
		order = append(order, "register first")
		cfg.PeriodicJobs = append(cfg.PeriodicJobs, periodic("first"))
		return firstRegistration(ctx, cfg)
	}, func(_ context.Context, binding Binding) error {
		c := binding.Client
		if binding.Pool != pool {
			t.Fatal("binding did not carry the actual host pool")
		}
		order = append(order, "bind first")
		bound = append(bound, c)
		return nil
	}, func() error { return nil })
	second := NewContribution("second", func(_ context.Context, cfg *riverqueue.Config) error {
		order = append(order, "register second")
		cfg.Queues["second"] = riverqueue.QueueConfig{MaxWorkers: 1}
		return riverqueue.AddWorkerSafely(cfg.Workers, &secondWorker{})
	}, func(_ context.Context, binding Binding) error {
		c := binding.Client
		if binding.Pool != pool {
			t.Fatal("binding did not carry the actual host pool")
		}
		order = append(order, "bind second")
		bound = append(bound, c)
		return nil
	}, func() error { return nil })
	client, err := New(t.Context(), pool, options, first, second)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(order, ","); got != "register first,register second,bind first,bind second" {
		t.Fatal(got)
	}
	if len(bound) != 2 || bound[0] != client || bound[1] != client || client.Schema() != "public" {
		t.Fatal("contributions did not receive the same client")
	}
	if client.Stopped() != nil || pool.Stat().TotalConns() != 0 {
		t.Fatal("composition started work or connected to the database")
	}
	if len(options.Queues) != 1 || options.Queues["first"].MaxWorkers != 4 || len(options.PeriodicJobs) != 1 || options.PeriodicJobs[0] != original || options.Workers != nil {
		t.Fatal("composition changed caller options")
	}
	if _, err := New(t.Context(), pool, nil, first); err == nil {
		t.Fatal("contribution reused")
	}
}

func TestCompositionFailureAbortsOnlyThisAttemptInReverseOrder(t *testing.T) {
	for _, where := range []string{"register", "bind", "constructor"} {
		t.Run(where, func(t *testing.T) {
			pool := lazyPool(t)
			var events []string
			bound := false
			first := NewContribution("first", firstRegistration, func(context.Context, Binding) error { bound = true; return nil }, func() error { events = append(events, "first"); bound = false; return errors.New("cleanup proof") })
			second := NewContribution("second", func(context.Context, *riverqueue.Config) error {
				if where == "register" {
					return errors.New("register proof")
				}
				return nil
			}, func(context.Context, Binding) error { return errors.New("bind proof") }, func() error { events = append(events, "second"); return nil })
			options := &riverqueue.Config{}
			if where == "constructor" {
				options.Queues = map[string]riverqueue.QueueConfig{"invalid": {MaxWorkers: -1}}
			}
			client, err := New(t.Context(), pool, options, first, second)
			if client != nil || err == nil || !strings.Contains(err.Error(), "cleanup proof") || strings.Join(events, ",") != "second,first" || bound {
				t.Fatalf("bad failure cleanup: %v %v %v bound=%v", client, err, events, bound)
			}
			if pool.Stat().TotalConns() != 0 {
				t.Fatal("failed composition used host database")
			}
		})
	}
}

func TestDuplicateAndIncompleteContributionsFailBeforeBinding(t *testing.T) {
	for _, name := range []string{"worker", "queue", "periodic", "removed periodic", "registry", "schema", "nil periodic"} {
		t.Run(name, func(t *testing.T) {
			pool := lazyPool(t)
			binds := 0
			first := NewContribution("first", func(ctx context.Context, cfg *riverqueue.Config) error {
				cfg.PeriodicJobs = append(cfg.PeriodicJobs, periodic("same"))
				return firstRegistration(ctx, cfg)
			}, func(context.Context, Binding) error { binds++; return nil }, func() error { return nil })
			second := NewContribution("second", func(_ context.Context, cfg *riverqueue.Config) error {
				switch name {
				case "worker":
					return riverqueue.AddWorkerSafely(cfg.Workers, &firstWorker{})
				case "queue":
					cfg.Queues["first"] = riverqueue.QueueConfig{MaxWorkers: 2}
				case "periodic":
					cfg.PeriodicJobs = append(cfg.PeriodicJobs, periodic("same"))
				case "removed periodic":
					cfg.PeriodicJobs = nil
				case "registry":
					cfg.Workers = riverqueue.NewWorkers()
				case "schema":
					cfg.Schema = "other"
				case "nil periodic":
					cfg.PeriodicJobs = append(cfg.PeriodicJobs, nil)
				}
				return nil
			}, nil, nil)
			if client, err := New(t.Context(), pool, nil, first, second); err == nil || client != nil {
				t.Fatalf("accepted %s corruption", name)
			}
			if binds != 0 {
				t.Fatal("partial config bound before validation")
			}
		})
	}
}

func TestDuplicateNameAndConcurrentDescriptorReuse(t *testing.T) {
	pool := lazyPool(t)
	first := NewContribution("first", firstRegistration, nil, nil)
	if _, err := New(t.Context(), pool, nil, first, first); err == nil {
		t.Fatal("duplicate descriptor accepted")
	}
	other := NewContribution("first", firstRegistration, nil, nil)
	if _, err := New(t.Context(), pool, nil, first, other); err == nil {
		t.Fatal("duplicate name accepted")
	}
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { _, err := New(t.Context(), pool, nil, first); results <- err })
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("one descriptor composed %d clients", successes)
	}
}

func TestCanceledCompositionDoesNotConsumeContribution(t *testing.T) {
	pool := lazyPool(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	contribution := NewContribution("first", firstRegistration, nil, nil)
	if _, err := New(ctx, pool, nil, contribution); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := New(t.Context(), pool, nil, contribution); err != nil {
		t.Fatal(err)
	}
}

func TestProducerBindingRequiresInvalidation(t *testing.T) {
	pool := lazyPool(t)
	unsafe := NewContribution("unsafe", firstRegistration, func(context.Context, Binding) error { return nil }, nil)
	if _, err := New(t.Context(), pool, nil, unsafe); err == nil {
		t.Fatal("producer binding accepted without an abort hook")
	}
}

func TestFinalBindCancellationInvalidatesProducer(t *testing.T) {
	pool := lazyPool(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var producer *riverqueue.Client[pgx.Tx]
	contribution := NewContribution("first", firstRegistration, func(_ context.Context, binding Binding) error {
		c := binding.Client
		if binding.Pool != pool {
			t.Fatal("binding did not carry the actual host pool")
		}
		producer = c
		cancel()
		return nil
	}, func() error { producer = nil; return nil })
	client, err := New(ctx, pool, nil, contribution)
	if !errors.Is(err, context.Canceled) || client != nil || producer != nil {
		t.Fatalf("canceled last bind stayed usable: %v %v %v", err, client, producer)
	}
	if pool.Stat().TotalConns() != 0 {
		t.Fatal("composition touched host pool")
	}
}

func TestGroupsCannotHideDuplicateContributions(t *testing.T) {
	pool := lazyPool(t)
	contribution := NewContribution("first", firstRegistration, nil, nil)
	if _, err := New(t.Context(), pool, nil, Group(contribution, Group(contribution))); err == nil {
		t.Fatal("nested duplicate contribution accepted")
	}
	if _, err := New(t.Context(), pool, nil, Group(Group(contribution))); err != nil {
		t.Fatal(err)
	}
	if _, err := New(t.Context(), pool, nil, Group()); err == nil {
		t.Fatal("empty composition accepted")
	}
}

func TestBindingPinsDefaultQueueNamespace(t *testing.T) {
	pool := lazyPool(t)
	var bound Binding
	contribution := NewContribution("first", firstRegistration, func(_ context.Context, binding Binding) error { bound = binding; return nil }, func() error { return nil })
	client, err := New(t.Context(), pool, nil, contribution)
	if err != nil {
		t.Fatal(err)
	}
	if bound.Client != client || bound.Pool != pool || client.Schema() != "public" {
		t.Fatal("binding must carry the actual client, pool, and explicit public default")
	}
	if pool.Stat().TotalConns() != 0 {
		t.Fatal("neutral composition must not acquire a database connection")
	}
}
