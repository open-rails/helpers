package river

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

var migrationSchemaName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// ApplyMigrations creates schema and applies River's pending migrations. Empty
// schema selects public, matching New. Use the same schema for both calls.
//
// Calls for the same database/schema serialize before any schema creation. A
// dedicated connection holds the advisory lock, leaving even a one-connection
// host pool available to the migrator. The caller retains ownership of pool.
func ApplyMigrations(ctx context.Context, pool *pgxpool.Pool, schema string) error {
	if ctx == nil {
		return errors.New("riverkit: context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if pool == nil {
		return errors.New("riverkit: host pool is required")
	}
	if schema == "" {
		schema = "public"
	}
	// River v0.47 prefixes its notification channels with the schema. Preserve
	// its identifier rules and avoid PostgreSQL's silent identifier truncation.
	if len(schema) > 63-len(".river_leadership") || !migrationSchemaName.MatchString(schema) {
		return fmt.Errorf("riverkit: invalid River schema %q", schema)
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
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), &rivermigrate.Config{Schema: schema})
	if err != nil {
		return fmt.Errorf("riverkit: construct migrator: %w", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return fmt.Errorf("riverkit: apply migrations: %w", err)
	}
	return nil
}
