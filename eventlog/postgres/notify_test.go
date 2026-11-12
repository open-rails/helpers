package postgres

import (
	"context"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// listen holds a connection LISTENing on schema's channel.
func listen(t *testing.T, p *pgxpool.Pool, schema string) *pgxpool.Conn {
	t.Helper()
	conn, err := p.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(conn.Release)
	if _, err := conn.Exec(t.Context(), "LISTEN "+pgx.Identifier{NotifyChannel(schema)}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	return conn
}

func heard(t *testing.T, conn *pgxpool.Conn, within time.Duration) (string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), within)
	defer cancel()
	note, err := conn.Conn().WaitForNotification(ctx)
	if err != nil {
		return "", false
	}
	return note.Payload, true
}

// A commit that records an event tells its stream's listeners; a rollback tells no one.
func TestCommitNotifiesItsStream(t *testing.T) {
	s, p := database(t)
	conn := listen(t, p, s.config.Schema)
	tx, err := p.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AppendTx(t.Context(), tx, fact("rolled-back", "kind")); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if payload, ok := heard(t, conn, 300*time.Millisecond); ok {
		t.Fatalf("a rolled-back append notified %q", payload)
	}
	appendCommit(t, s, p, fact("committed", "kind"))
	payload, ok := heard(t, conn, 5*time.Second)
	if !ok || payload != "product a" {
		t.Fatalf("a committed append notified %q (heard %v), want %q", payload, ok, "product a")
	}
}

// A product that installs the engine through MigrationSQL gets the notification from version 2.
func TestMigrationSQLVersion2Notifies(t *testing.T) {
	_, p := database(t)
	schema := "eventlog_sql_" + strings.ToLower(rand.Text())
	if _, err := p.Exec(t.Context(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = p.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	})
	for version := 1; version <= 2; version++ {
		sql, err := MigrationSQL(schema, version)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.Exec(t.Context(), sql); err != nil {
			t.Fatalf("version %d: %v", version, err)
		}
	}
	if err := Migrate(t.Context(), p, schema); err != nil {
		t.Fatal("Migrate after MigrationSQL:", err)
	}
	if _, err := MigrationSQL(schema, 3); err == nil {
		t.Fatal("MigrationSQL accepted an unknown version")
	}
	s, err := New(p, Config{Schema: schema, Source: "product", Scope: "b"})
	if err != nil {
		t.Fatal(err)
	}
	conn := listen(t, p, schema)
	appendCommit(t, s, p, fact("one", "kind"))
	if payload, ok := heard(t, conn, 5*time.Second); !ok || payload != "product b" {
		t.Fatalf("heard %q (%v), want %q", payload, ok, "product b")
	}
}
