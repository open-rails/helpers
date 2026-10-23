package river

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	riverqueue "github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// NewPeriodicJob is River's NewPeriodicJob with an interval that survives leader changes
// and restarts. RunOnStart catches up overdue work; it does not run a recent job
// again. Add the result to cfg.PeriodicJobs and compose cfg with New.
//
// River remains responsible for leader election, enqueueing, retries and worker
// claims. New retains finished jobs for at least the longest interval so the
// startup checkpoint cannot disappear before it becomes due. Explicit/event
// inserts are unaffected. IDs must be stable and scoped to the app/tenant.
func NewPeriodicJob(cfg *riverqueue.Config, every time.Duration, constructor riverqueue.PeriodicJobConstructor, opts *riverqueue.PeriodicJobOpts) *riverqueue.PeriodicJob {
	if cfg == nil || every <= 0 || constructor == nil || opts == nil || opts.ID == "" {
		panic("riverkit: periodic interval requires config, positive interval, constructor and stable ID")
	}
	var checkpoint *periodicCheckpoint
	for _, hook := range cfg.Hooks {
		if h, ok := hook.(*periodicCheckpoint); ok {
			checkpoint = h
			break
		}
	}
	if checkpoint == nil {
		checkpoint = &periodicCheckpoint{}
		cfg.Hooks = append(cfg.Hooks, checkpoint)
	}
	p := &periodicInterval{id: opts.ID, every: every, constructor: constructor, runOnStart: opts.RunOnStart}
	checkpoint.jobs = append(checkpoint.jobs, p)
	return riverqueue.NewPeriodicJob(p, p.construct, opts)
}

type periodicInterval struct {
	mu          sync.Mutex
	id          string
	every       time.Duration
	constructor riverqueue.PeriodicJobConstructor
	last        time.Time
	runOnStart  bool
	startup     bool
}

func (p *periodicInterval) Next(now time.Time) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	if due := p.last.Add(p.every); !p.last.IsZero() && due.After(now) {
		return due
	}
	return now.Add(p.every)
}

func (p *periodicInterval) construct() (riverqueue.JobArgs, *riverqueue.InsertOpts) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if p.startup {
		p.startup = false
		if !p.last.IsZero() && now.Before(p.last.Add(p.every)) {
			return nil, nil
		}
	}
	args, opts := p.constructor()
	if args != nil {
		p.last = now
	}
	return args, opts
}

type periodicCheckpoint struct {
	riverqueue.HookDefaults
	pool   *pgxpool.Pool
	schema string
	jobs   []*periodicInterval
}

// bind is deliberately database-free: hosts may compose before migrations.
func (h *periodicCheckpoint) bind(pool *pgxpool.Pool, cfg *riverqueue.Config) error {
	if h.pool != nil {
		return fmt.Errorf("riverkit: periodic config already composed; construct a fresh config per fleet")
	}
	h.pool, h.schema = pool, cfg.Schema
	var longest time.Duration
	for _, p := range h.jobs {
		longest = max(longest, p.every)
	}
	// River defaults to 24h completed and 7d discarded. -1 is infinite.
	retain := func(value *time.Duration, fallback time.Duration) {
		if *value != -1 && ((*value == 0 && fallback < longest) || (*value > 0 && *value < longest)) {
			*value = longest
		}
	}
	retain(&cfg.CompletedJobRetentionPeriod, 24*time.Hour)
	retain(&cfg.DiscardedJobRetentionPeriod, 7*24*time.Hour)
	return nil
}

// Start runs on every newly elected leader, before River computes intervals or
// calls startup constructors. It reads all checkpoints in one query; a follower
// never independently enqueues work. Pending and retrying jobs also count so a
// restart cannot create a second copy while the original waits for a worker.
func (h *periodicCheckpoint) Start(ctx context.Context, _ *rivertype.HookPeriodicJobsStartParams) error {
	filters := make([]string, len(h.jobs))
	for i, p := range h.jobs {
		encoded, _ := json.Marshal(map[string]string{"river:periodic_job_id": p.id})
		filters[i] = string(encoded)
	}
	// Containment can use River's existing GIN metadata index; extracting the
	// key in WHERE would scan all retained jobs on every leader change.
	rows, err := h.pool.Query(ctx, `SELECT metadata->>'river:periodic_job_id',
		max(greatest(created_at, attempted_at, scheduled_at))
		FROM `+pgx.Identifier{h.schema, "river_job"}.Sanitize()+`
		WHERE metadata @> ANY($1::jsonb[]) AND state <> 'cancelled'
		GROUP BY metadata->>'river:periodic_job_id'`, filters)
	if err != nil {
		return fmt.Errorf("riverkit: read periodic checkpoints: %w", err)
	}
	defer rows.Close()
	last := map[string]time.Time{}
	for rows.Next() {
		var id string
		var at time.Time
		if err := rows.Scan(&id, &at); err != nil {
			return err
		}
		last[id] = at
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, p := range h.jobs {
		p.mu.Lock()
		p.last = last[p.id]
		p.startup = p.runOnStart
		p.mu.Unlock()
	}
	return nil
}
