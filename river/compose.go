// Package river composes library workers into one host-owned River client.
// It does not migrate a database, own a pool, or start or stop the returned client.
package river

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	riverqueue "github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

// Binding is the final client and the exact pool used to construct it. Both are
// borrowed from the host: contributions must not start/stop the client or close
// the pool. A contribution may use Pool to verify its transactional producers
// address the same physical database before enabling them.
type Binding struct {
	Client *riverqueue.Client[pgx.Tx]
	Pool   *pgxpool.Pool
}

// Contribution is one library's startup registration and producer binding.
// Copies share the same single-use identity. Libraries also guard their own
// lifecycle so requesting a fresh contribution cannot compose a runtime twice.
type Contribution struct {
	hooks *contribution
	group []Contribution
}
type contribution struct {
	mu       sync.Mutex
	used     bool
	name     string
	register func(context.Context, *riverqueue.Config) error
	bind     func(context.Context, Binding) error
	abort    func() error
}

// NewContribution describes one library's jobs. register adds workers, active
// queues and periodic jobs; bind attaches the final unstarted client to any
// request-side producers. abort must invalidate partial registration/binding
// and release only the library's composition resources, never the host pool.
// Construction is side-effect free. Nil bind/abort hooks are allowed for a
// stateless contribution with no producer or resources to clean up. A bind
// callback requires an abort callback to invalidate any partial producer binding.
func NewContribution(name string, register func(context.Context, *riverqueue.Config) error, bind func(context.Context, Binding) error, abort func() error) Contribution {
	return Contribution{hooks: &contribution{name: name, register: register, bind: bind, abort: abort}}
}

// Group bundles a library's own jobs with already attached components. It does
// not register or bind anything. Children are flattened before any composition
// validation, so duplicate registrations cannot hide inside a group.
func Group(jobs ...Contribution) Contribution {
	return Contribution{group: append([]Contribution{}, jobs...)}
}

func flatten(jobs []Contribution) []Contribution {
	var out []Contribution
	for _, job := range jobs {
		if job.group != nil {
			out = append(out, flatten(job.group)...)
		} else {
			out = append(out, job)
		}
	}
	return out
}

// New registers every contribution before constructing one unstarted client,
// then binds its producers. A failed composition consumes its contributions;
// discard the participating library runtimes and construct fresh ones.
// The input config's maps and slices are copied. An existing Workers registry
// may be extended during registration and must also be discarded on failure.
func New(ctx context.Context, pool *pgxpool.Pool, options *riverqueue.Config, jobs ...Contribution) (client *riverqueue.Client[pgx.Tx], err error) {
	if ctx == nil {
		return nil, errors.New("riverkit: context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if pool == nil {
		return nil, errors.New("riverkit: host pool is required")
	}
	jobs = flatten(jobs)
	if len(jobs) == 0 {
		return nil, errors.New("riverkit: at least one contribution is required")
	}
	names := map[string]bool{}
	for _, job := range jobs {
		h := job.hooks
		if h == nil || strings.TrimSpace(h.name) == "" || h.register == nil || (h.bind != nil && h.abort == nil) {
			return nil, errors.New("riverkit: invalid contribution")
		}
		if names[h.name] {
			return nil, fmt.Errorf("riverkit: duplicate contribution %q", h.name)
		}
		names[h.name] = true
	}
	var claimed []*contribution
	defer func() {
		if err == nil {
			return
		}
		for i := len(claimed) - 1; i >= 0; i-- {
			if abort := claimed[i].abort; abort != nil {
				if cleanup := abort(); cleanup != nil {
					err = errors.Join(err, fmt.Errorf("riverkit: abort %s: %w", claimed[i].name, cleanup))
				}
			}
		}
		client = nil
	}()
	for _, job := range jobs {
		h := job.hooks
		h.mu.Lock()
		if h.used {
			h.mu.Unlock()
			return nil, fmt.Errorf("riverkit: contribution %q already composed", h.name)
		}
		h.used = true
		h.mu.Unlock()
		claimed = append(claimed, h)
	}
	cfg := riverqueue.Config{}
	if options != nil {
		cfg = *options
	}
	// Pin a namespace before constructing the client: an empty River schema
	// otherwise lets worker and InsertTx connections use different search paths.
	if cfg.Schema == "" {
		cfg.Schema = "public"
	}
	cfg.Queues = maps.Clone(cfg.Queues)
	if cfg.Queues == nil {
		cfg.Queues = map[string]riverqueue.QueueConfig{}
	}
	cfg.PeriodicJobs = slices.Clone(cfg.PeriodicJobs)
	if cfg.Workers == nil {
		cfg.Workers = riverqueue.NewWorkers()
	}
	for _, h := range claimed {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		workers, queues, periodic, schema := cfg.Workers, maps.Clone(cfg.Queues), slices.Clone(cfg.PeriodicJobs), cfg.Schema
		if err := h.register(ctx, &cfg); err != nil {
			return nil, fmt.Errorf("riverkit: register %s: %w", h.name, err)
		}
		if cfg.Workers != workers {
			return nil, fmt.Errorf("riverkit: %s replaced the composed worker registry", h.name)
		}
		if cfg.Schema != schema {
			return nil, fmt.Errorf("riverkit: %s changed the host queue schema", h.name)
		}
		for name, before := range queues {
			if after, present := cfg.Queues[name]; !present || after != before {
				return nil, fmt.Errorf("riverkit: %s changed existing queue %q", h.name, name)
			}
		}
		for _, job := range periodic {
			if !slices.Contains(cfg.PeriodicJobs, job) {
				return nil, fmt.Errorf("riverkit: %s removed a composed periodic job", h.name)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// River 0.47's constructor panics on duplicate periodic IDs. All schedules
	// are already composed; add them through its checked API before binding or
	// Start, so an invalid schedule reports an error and runs failure cleanup.
	if len(cfg.Queues) == 0 {
		return nil, errors.New("riverkit: contributions require an active worker queue")
	}
	for _, job := range cfg.PeriodicJobs {
		if job == nil {
			return nil, errors.New("riverkit: nil periodic job")
		}
	}
	periodic := cfg.PeriodicJobs
	cfg.PeriodicJobs = nil
	client, err = riverqueue.NewClient(riverpgxv5.New(pool), &cfg)
	if err != nil {
		return nil, fmt.Errorf("riverkit: construct client: %w", err)
	}
	if len(periodic) > 0 {
		if _, err = client.PeriodicJobs().AddManySafely(periodic); err != nil {
			return nil, fmt.Errorf("riverkit: periodic jobs: %w", err)
		}
	}
	for _, h := range claimed {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if h.bind != nil {
			if err := h.bind(ctx, Binding{Client: client, Pool: pool}); err != nil {
				return nil, fmt.Errorf("riverkit: bind %s: %w", h.name, err)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return client, nil
}
