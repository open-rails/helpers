package river

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// notifyFunction sends River's own insert notification for one available row: the channel
// "<schema>.river_insert" and payload {"queue": "<queue>"} that River's producers LISTEN for.
const notifyFunction = `BEGIN
  PERFORM pg_notify(TG_TABLE_SCHEMA || '.river_insert', '{"queue": ' || to_json(NEW.queue)::text || '}');
  RETURN NULL;
END`

// NotifyEveryInsert makes every job inserted available wake its queue's producers at once.
//
// River sends an insert's NOTIFY at most once per queue per FetchCooldown (100 ms) from one
// client, so a second job inside that window is found only by the FetchPollInterval poll. A
// row trigger on <schema>.river_job sends River's notification for every available row, in
// the inserting transaction; Postgres folds a transaction's identical ones into one, and the
// producer's fetch cooldown coalesces the rest. With it the poll is a backstop for a lost
// LISTEN connection, not the path a job takes. ApplyMigrations installs it; a host that runs
// River's migrator itself calls this after. Installing an unchanged trigger takes no lock.
func NotifyEveryInsert(ctx context.Context, pool *pgxpool.Pool, schema string) error {
	if ctx == nil {
		return errors.New("riverkit: context is required")
	}
	if pool == nil {
		return errors.New("riverkit: host pool is required")
	}
	schema, err := validatedMigrationSchema(schema)
	if err != nil {
		return err
	}
	name := pgx.Identifier{schema}.Sanitize()
	var installed bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM pg_trigger t JOIN pg_proc p ON p.oid = t.tgfoid
		WHERE t.tgrelid = to_regclass($1) AND t.tgname = 'river_job_notify_insert' AND p.prosrc = $2)`,
		name+".river_job", notifyFunction).Scan(&installed); err != nil {
		return fmt.Errorf("riverkit: read insert notification trigger: %w", err)
	}
	if installed {
		return nil
	}
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		// The trigger's lock waits on in-flight job writes; a writer stuck past this fails the
		// install (retry it) instead of holding every insert behind it.
		if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '10s'"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "CREATE OR REPLACE FUNCTION "+name+".river_job_notify_insert() RETURNS trigger LANGUAGE plpgsql AS $fn$"+notifyFunction+"$fn$"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "CREATE OR REPLACE TRIGGER river_job_notify_insert AFTER INSERT ON "+name+".river_job "+
			"FOR EACH ROW WHEN (NEW.state = 'available') EXECUTE FUNCTION "+name+".river_job_notify_insert()")
		return err
	})
	if err != nil {
		return fmt.Errorf("riverkit: install insert notification trigger: %w", err)
	}
	return nil
}
