package river

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestValidateMigrationsReadOnly(t *testing.T) {
	pool := migrationTestPool(t)
	ctx := t.Context()
	const schema = "validated_river"
	if err := ValidateMigrations(ctx, pool, schema); err == nil {
		t.Fatal("missing River schema accepted")
	}
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regnamespace($1) IS NOT NULL`, schema).Scan(&exists); err != nil || exists {
		t.Fatalf("validation created schema: %t %v", exists, err)
	}
	if err := ApplyMigrations(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	cfg := pool.Config()
	cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	read, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	if err := ValidateMigrations(ctx, read, schema); err != nil {
		t.Fatalf("read-only validation failed: %v", err)
	}
}
