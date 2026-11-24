package river

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestValidateMigrationsReadOnly(t *testing.T) {
	pool := migrationTestPool(t)
	ctx := t.Context()
	const schema = "validated_river"
	if err := ValidateMigrations(ctx, pool, MigrationOptions{Schema: schema}); err == nil {
		t.Fatal("missing River schema accepted")
	}
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regnamespace($1) IS NOT NULL`, schema).Scan(&exists); err != nil || exists {
		t.Fatalf("validation created schema: %t %v", exists, err)
	}
	var logs bytes.Buffer
	if err := ApplyMigrations(ctx, pool, MigrationOptions{Schema: schema, Logger: slog.New(slog.NewTextHandler(&logs, nil))}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "Applied migration") {
		t.Fatalf("migrator did not log through the given logger: %q", logs.String())
	}
	cfg := pool.Config()
	cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	read, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	if err := ValidateMigrations(ctx, read, MigrationOptions{Schema: schema}); err != nil {
		t.Fatalf("read-only validation failed: %v", err)
	}
}
