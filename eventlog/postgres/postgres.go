// Package postgres stores independent scoped logs in a host-owned PostgreSQL
// schema. Append and SQL source triggers queue facts in the mutation transaction;
// a deferred trigger assigns commit-safe positions after locking all its scopes.
package postgres

import (
	"context"
	"crypto/rand"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/helpers/eventlog"
)

//go:embed schema.sql
var schemaSQL string

func schemaName(schema string) (string, error) {
	if schema == "" || len(schema) > 63 || strings.ContainsRune(schema, 0) {
		return "", fmt.Errorf("eventlog: valid schema required")
	}
	return pgx.Identifier{schema}.Sanitize(), nil
}

// Migrate installs the shared tables/functions once. The host owns and creates
// the schema. Run before accepting mutations, using a connection allowed DDL.
func Migrate(ctx context.Context, pool *pgxpool.Pool, schema string) error {
	if pool == nil {
		return fmt.Errorf("eventlog: pool required")
	}
	name, err := schemaName(schema)
	if err != nil {
		return err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, schema+"/helpers/eventlog/migrate"); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS `+name+`.eventlog_migrations(version integer PRIMARY KEY)`); err != nil {
		return err
	}
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+name+`.eventlog_migrations WHERE version=1)`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err = tx.Exec(ctx, strings.ReplaceAll(schemaSQL, "{{schema}}", name)); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO `+name+`.eventlog_migrations VALUES(1)`); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

type Config struct {
	Schema, Source, Scope string
	Retention             time.Duration
	Now                   func() time.Time
}
type Store struct {
	pool   *pgxpool.Pool
	schema string
	config Config
}

func New(pool *pgxpool.Pool, c Config) (*Store, error) {
	schema, err := schemaName(c.Schema)
	if err != nil {
		return nil, err
	}
	if pool == nil || c.Source == "" || c.Scope == "" || c.Retention < 0 {
		return nil, fmt.Errorf("eventlog: pool, source, scope and nonnegative retention required")
	}
	if c.Retention == 0 {
		c.Retention = eventlog.DefaultRetention
	}
	return &Store{pool: pool, schema: schema, config: c}, nil
}

// AppendTx queues a fact in the caller's mutation transaction. The log namespace and scope
// come from this authorized store. Event.Source is product-owned provenance. ID must be stable on retries;
// an identical retained fact is a no-op; a retained ID with different content fails
// the transaction rather than overwriting history.
// Products must never reuse an ID for a different fact, including after pruning. Direct SQL
// source triggers may insert these same fields into eventlog_pending. Do not
// force its deferred constraint to immediate or change transaction_id.
func (s *Store) AppendTx(ctx context.Context, tx pgx.Tx, e eventlog.Event) error {
	if e.ID == "" || e.Type == "" || len(e.Type) > 128 || e.Version < 1 || e.OccurredAt.IsZero() {
		return fmt.Errorf("%w: id, type, positive version and occurred_at required", eventlog.ErrInvalidQuery)
	}
	if e.Scope != "" && e.Scope != s.config.Scope {
		return fmt.Errorf("%w: event scope differs from store", eventlog.ErrInvalidQuery)
	}
	_, err := tx.Exec(ctx, `INSERT INTO `+s.schema+`.eventlog_pending(source,scope,id,event_source,event_type,version,occurred_at,subject_type,subject_id,data) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, s.config.Source, s.config.Scope, e.ID, e.Source, e.Type, e.Version, e.OccurredAt, e.Subject.Type, e.Subject.ID, e.Data)
	return err
}
func (s *Store) ListEvents(ctx context.Context, q eventlog.Query) (eventlog.Page, error) {
	log := eventlog.Log{Source: s.config.Source, Scope: s.config.Scope, Retention: s.config.Retention, Now: s.config.Now, ReadSnapshot: func(ctx context.Context, fn func(eventlog.Snapshot) error) error {
		tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		err = fn(&snapshot{s: s, tx: tx})
		if err != nil {
			return err
		}
		return tx.Commit(ctx)
	}}
	return log.ListEvents(ctx, q)
}

type snapshot struct {
	s      *Store
	tx     pgx.Tx
	cutoff time.Time
}

func (r *snapshot) Stream(ctx context.Context, cutoff time.Time) (eventlog.Stream, error) {
	r.cutoff = cutoff
	var stream eventlog.Stream
	err := r.tx.QueryRow(ctx, `SELECT epoch,position,head_id,pruned_through,pruned_id FROM `+r.s.schema+`.eventlog_streams WHERE source=$1 AND scope=$2`, r.s.config.Source, r.s.config.Scope).Scan(&stream.Epoch, &stream.Head.Number, &stream.Head.ID, &stream.Floor.Number, &stream.Floor.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		// An empty scope still needs a persistent epoch before issuing any cursor.
		// This read-only snapshot cannot create one; NewScope does so explicitly.
		return stream, fmt.Errorf("eventlog: scope is not initialized; call EnsureScope before reading")
	}
	if err != nil {
		return stream, err
	}
	var floor eventlog.Position
	err = r.tx.QueryRow(ctx, `SELECT position,id FROM `+r.s.schema+`.eventlog_events WHERE source=$1 AND scope=$2 AND recorded_at<$3 AND position<coalesce((SELECT min(position) FROM `+r.s.schema+`.eventlog_events WHERE source=$1 AND scope=$2 AND recorded_at>=$3),9223372036854775807) ORDER BY position DESC LIMIT 1`, r.s.config.Source, r.s.config.Scope, cutoff).Scan(&floor.Number, &floor.ID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return stream, err
	}
	if floor.Number > stream.Floor.Number {
		stream.Floor = floor
	}
	return stream, nil
}
func (r *snapshot) Boundary(ctx context.Context, position int64) (string, error) {
	var id string
	err := r.tx.QueryRow(ctx, `SELECT id FROM `+r.s.schema+`.eventlog_events WHERE source=$1 AND scope=$2 AND position=$3`, r.s.config.Source, r.s.config.Scope, position).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", eventlog.ErrExpiredCursor
	}
	return id, err
}
func (r *snapshot) Events(ctx context.Context, after int64, types []string, limit int) ([]eventlog.Record, error) {
	rows, err := r.tx.Query(ctx, `SELECT position,id,event_source,event_type,version,occurred_at,recorded_at,subject_type,subject_id,data FROM `+r.s.schema+`.eventlog_events WHERE source=$1 AND scope=$2 AND position>$3 AND (cardinality($4::text[])=0 OR event_type=ANY($4)) AND recorded_at >= $6 ORDER BY position LIMIT $5`, r.s.config.Source, r.s.config.Scope, after, append([]string{}, types...), limit, r.cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []eventlog.Record
	for rows.Next() {
		var rcd eventlog.Record
		e := &rcd.Event
		e.Source = r.s.config.Source
		e.Scope = r.s.config.Scope
		if err = rows.Scan(&rcd.Position, &e.ID, &e.Source, &e.Type, &e.Version, &e.OccurredAt, &e.RecordedAt, &e.Subject.Type, &e.Subject.ID, &e.Data); err != nil {
			return nil, err
		}
		records = append(records, rcd)
	}
	return records, rows.Err()
}

// EnsureScope creates a stable empty stream before its first read. It is
// idempotent; hosts normally call it when creating their tenant/merchant/app.
func (s *Store) EnsureScope(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO `+s.schema+`.eventlog_streams(source,scope) VALUES($1,$2) ON CONFLICT DO NOTHING`, s.config.Source, s.config.Scope)
	return err
}

// Prune deletes at most limit expired rows in this scope and persists their
// boundary. Reads enforce retention even if this method is never called.
func (s *Store) Prune(ctx context.Context, limit int) (int64, error) {
	if limit < 1 || limit > 10000 {
		return 0, fmt.Errorf("%w: prune limit must be 1..10000", eventlog.ErrInvalidQuery)
	}
	now := time.Now()
	if s.config.Now != nil {
		now = s.config.Now()
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT 1 FROM `+s.schema+`.eventlog_streams WHERE source=$1 AND scope=$2 FOR UPDATE`, s.config.Source, s.config.Scope); err != nil {
		return 0, err
	}
	var n int64
	err = tx.QueryRow(ctx, `WITH doomed AS (SELECT position,id FROM `+s.schema+`.eventlog_events WHERE source=$1 AND scope=$2 AND recorded_at<$3 AND position<coalesce((SELECT min(position) FROM `+s.schema+`.eventlog_events WHERE source=$1 AND scope=$2 AND recorded_at>=$3),9223372036854775807) ORDER BY position LIMIT $4), boundary AS (SELECT position,id FROM doomed ORDER BY position DESC LIMIT 1), updated AS (UPDATE `+s.schema+`.eventlog_streams SET pruned_through=boundary.position,pruned_id=boundary.id FROM boundary WHERE source=$1 AND scope=$2), deleted AS (DELETE FROM `+s.schema+`.eventlog_events WHERE source=$1 AND scope=$2 AND position IN (SELECT position FROM doomed) RETURNING 1) SELECT count(*) FROM deleted`, s.config.Source, s.config.Scope, now.Add(-s.config.Retention), limit).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, tx.Commit(ctx)
}

// NewID returns an opaque ID suitable for an event without a product-owned ID.
func NewID() string { return rand.Text() }

// MigrationSQL returns an immutable version of the shared schema for inclusion
// in a product's transactional SQL migration. Keep the returned SQL verbatim in
// a generated segment and verify it against this function in a contract test.
// Products may add their own FK/cascade constraints after this segment. This
// lets a single product migration install the engine, copy retained history and
// switch its SQL capture atomically. Only version 1 is currently defined.
func MigrationSQL(schema string, version int) (string, error) {
	if version != 1 {
		return "", fmt.Errorf("eventlog: unknown schema version %d", version)
	}
	name, err := schemaName(schema)
	if err != nil {
		return "", err
	}
	return "CREATE TABLE IF NOT EXISTS " + name + ".eventlog_migrations(version integer PRIMARY KEY);\n" + strings.ReplaceAll(schemaSQL, "{{schema}}", name) + "\nINSERT INTO " + name + ".eventlog_migrations(version) VALUES(1);\n", nil
}
