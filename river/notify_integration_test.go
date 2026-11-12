package river

import (
	"context"
	"sync"
	"testing"
	"time"

	riverqueue "github.com/riverqueue/river"
)

type burstArgs struct{ N int }

func (burstArgs) Kind() string { return "burst" }

type burstStart struct {
	n  int
	at time.Time
}

type burstWorker struct {
	riverqueue.WorkerDefaults[burstArgs]
	started chan<- burstStart
}

func (w *burstWorker) Work(_ context.Context, job *riverqueue.Job[burstArgs]) error {
	w.started <- burstStart{job.Args.N, time.Now()}
	return nil
}

// Two jobs one client inserts 50 ms apart, inside River's 100 ms insert-notification window,
// both start within a second though the backstop poll is 10 s: the trigger notified the second.
func TestNotifyEveryInsertStartsJobsInsideTheNotifyWindow(t *testing.T) {
	pool := migrationTestPool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	const schema = "river_burst"
	if err := ApplyMigrations(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	if err := ApplyMigrations(ctx, pool, schema); err != nil {
		t.Fatal("idempotent install:", err)
	}
	started := make(chan burstStart, 8)
	client, err := New(ctx, pool, &riverqueue.Config{Schema: schema, FetchPollInterval: 10 * time.Second},
		NewContribution("burst", func(_ context.Context, cfg *riverqueue.Config) error {
			cfg.Queues["burst"] = riverqueue.QueueConfig{MaxWorkers: 4}
			return riverqueue.AddWorkerSafely(cfg.Workers, &burstWorker{started: started})
		}, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stop, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		_ = client.Stop(stop)
	})
	// The first job proves the producer's LISTEN is up (River fetches once at start).
	if _, err := client.Insert(ctx, burstArgs{N: 0}, &riverqueue.InsertOpts{Queue: "burst"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("warm-up job never started")
	}
	time.Sleep(200 * time.Millisecond) // past the warm-up insert's notification window
	var mu sync.Mutex
	inserted := map[int]time.Time{}
	for n := 1; n <= 2; n++ {
		mu.Lock()
		inserted[n] = time.Now()
		mu.Unlock()
		if _, err := client.Insert(ctx, burstArgs{N: n}, &riverqueue.InsertOpts{Queue: "burst"}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	for range 2 {
		select {
		case start := <-started:
			mu.Lock()
			waited := start.at.Sub(inserted[start.n])
			mu.Unlock()
			t.Logf("job %d started %s after its insert", start.n, waited.Round(time.Millisecond))
			if waited > time.Second {
				t.Errorf("job %d waited %s for the poll: its insert sent no notification", start.n, waited)
			}
		case <-ctx.Done():
			t.Fatal("a job never started")
		}
	}
}
