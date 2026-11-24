package river

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

var migrationSchemaName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// MigrationOptions selects River's schema and where its migrator logs.
type MigrationOptions struct {
	Schema string       // River's tables; empty is public, matching New
	Logger *slog.Logger // nil is slog.Default(), read per call
}

// ApplyMigrations creates opts.Schema and applies River's pending migrations.
// Use the same schema in New's config.
//
// Calls for the same database/schema serialize before any schema creation. A
// dedicated connection holds the advisory lock, leaving even a one-connection
// host pool available to the migrator. The caller retains ownership of pool.
func ApplyMigrations(ctx context.Context, pool *pgxpool.Pool, opts MigrationOptions) error {
	if ctx == nil {
		return errors.New("riverkit: context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if pool == nil {
		return errors.New("riverkit: host pool is required")
	}
	schema, err := validatedMigrationSchema(opts.Schema)
	if err != nil {
		return err
	}
	lock, err := pgx.ConnectConfig(ctx, pool.Config().ConnConfig.Copy())
	if err != nil {
		return fmt.Errorf("riverkit: connect migration lock: %w", err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = lock.Close(cleanup)
	}()
	if _, err := lock.Exec(ctx, "SELECT pg_advisory_lock(hashtext(current_database()), hashtext($1))", "river-migrations:"+schema); err != nil {
		return fmt.Errorf("riverkit: acquire migration lock: %w", err)
	}
	if _, err := pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+pgx.Identifier{schema}.Sanitize()); err != nil {
		return fmt.Errorf("riverkit: create migration schema: %w", err)
	}
	migrator, err := newMigrator(pool, schema, opts.Logger)
	if err != nil {
		return fmt.Errorf("riverkit: construct migrator: %w", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return fmt.Errorf("riverkit: apply migrations: %w", err)
	}
	return NotifyEveryInsert(ctx, pool, schema)
}

func validatedMigrationSchema(schema string) (string, error) {
	if schema == "" {
		schema = "public"
	}
	// River notification channels include the schema in a PostgreSQL identifier.
	if len(schema) > 63-len(".river_leadership") || !migrationSchemaName.MatchString(schema) {
		return "", fmt.Errorf("riverkit: invalid River schema %q", schema)
	}
	return schema, nil
}

// newMigrator logs through logger, never River's stdout default.
func newMigrator(pool *pgxpool.Pool, schema string, logger *slog.Logger) (*rivermigrate.Migrator[pgx.Tx], error) {
	return rivermigrate.New(riverpgxv5.New(pool), &rivermigrate.Config{Schema: schema, Logger: cmp.Or(logger, slog.Default())})
}

// ValidateMigrations checks opts.Schema without creating a schema, acquiring
// a migration lock, or applying any migration.
func ValidateMigrations(ctx context.Context, pool *pgxpool.Pool, opts MigrationOptions) error {
	if ctx == nil {
		return errors.New("riverkit: context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if pool == nil {
		return errors.New("riverkit: host pool is required")
	}
	schema, err := validatedMigrationSchema(opts.Schema)
	if err != nil {
		return err
	}
	migrator, err := newMigrator(pool, schema, opts.Logger)
	if err != nil {
		return fmt.Errorf("riverkit: construct validator: %w", err)
	}
	result, err := migrator.Validate(ctx, nil)
	if err != nil {
		return fmt.Errorf("riverkit: schema %q requires migration; run ApplyMigrations: %w", schema, err)
	}
	if !result.OK {
		return fmt.Errorf("riverkit: schema %q requires migration; run ApplyMigrations: %v", schema, result.Messages)
	}
	return nil
}
