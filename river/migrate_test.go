package river

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestApplyMigrationsRejectsInvalidInputsBeforeConnecting(t *testing.T) {
	if err := ApplyMigrations(nil, nil, ""); err == nil {
		t.Fatal("nil context accepted")
	}
	if err := ApplyMigrations(t.Context(), nil, ""); err == nil {
		t.Fatal("nil host pool accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := ApplyMigrations(ctx, nil, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context: %v", err)
	}
	// An uninitialized pool proves invalid schema names cannot reach a connection.
	for _, schema := range []string{" ", "a.b", "quoted\"schema", "1schema", "bad\x00name", strings.Repeat("a", 47)} {
		if err := ApplyMigrations(t.Context(), &pgxpool.Pool{}, schema); err == nil {
			t.Errorf("invalid schema accepted: %q", schema)
		}
	}
}
