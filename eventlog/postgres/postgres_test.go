package postgres

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/helpers/eventlog"
)

func database(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	url := os.Getenv("EVENTLOG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set EVENTLOG_TEST_DATABASE_URL for PostgreSQL tests")
	}
	p, err := pgxpool.New(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	schema := "eventlog_test_" + strings.ToLower(rand.Text())
	_, err = p.Exec(t.Context(), `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := p.Exec(ctx, `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
		p.Close()
		if err != nil {
			t.Error(err)
		}
	})
	if err = Migrate(t.Context(), p, schema); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(t.Context(), p, schema); err != nil {
		t.Fatal(err)
	}
	s, err := New(p, Config{Schema: schema, Source: "product", Scope: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.EnsureScope(t.Context()); err != nil {
		t.Fatal(err)
	}
	return s, p
}
func fact(id, kind string) eventlog.Event {
	return eventlog.Event{ID: id, Type: kind, Version: 1, OccurredAt: time.Now().Add(-time.Hour), Data: []byte(`{"ok":true}`)}
}
func appendCommit(t *testing.T, s *Store, p *pgxpool.Pool, events ...eventlog.Event) {
	t.Helper()
	tx, err := p.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	for _, e := range events {
		if err = s.AppendTx(t.Context(), tx, e); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}
func page(t *testing.T, s *Store, q eventlog.Query) eventlog.Page {
	t.Helper()
	p, err := s.ListEvents(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func TestCommitOrderRollbackAndEmptyAdvancement(t *testing.T) {
	s, p := database(t)
	empty := page(t, s, eventlog.Query{})
	tx1, err := p.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx1.Rollback(t.Context())
	if err = s.AppendTx(t.Context(), tx1, fact("started-first", "a")); err != nil {
		t.Fatal(err)
	}
	appendCommit(t, s, p, fact("committed-first", "b"))
	first := page(t, s, eventlog.Query{After: empty.NextCursor})
	if len(first.Events) != 1 || first.Events[0].ID != "committed-first" {
		t.Fatalf("%#v", first)
	}
	if err = tx1.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	second := page(t, s, eventlog.Query{After: first.NextCursor})
	if len(second.Events) != 1 || second.Events[0].ID != "started-first" {
		t.Fatalf("late commit skipped %#v", second)
	}
	if !second.Events[0].RecordedAt.After(second.Events[0].OccurredAt) {
		t.Fatal("occurred and recorded not distinct")
	}
	tx, err := p.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AppendTx(t.Context(), tx, fact("rolled-back", "a")); err != nil {
		t.Fatal(err)
	}
	tx.Rollback(t.Context())
	last := page(t, s, eventlog.Query{After: second.NextCursor})
	if len(last.Events) != 0 {
		t.Fatal("rollback visible")
	}
	filtered := page(t, s, eventlog.Query{Types: []string{"future"}})
	if len(filtered.Events) != 0 || filtered.NextCursor == "" {
		t.Fatal("empty filter lacks cursor")
	}
	appendCommit(t, s, p, fact("third", "b"))
	next := page(t, s, eventlog.Query{Types: []string{"future"}, After: filtered.NextCursor})
	if next.NextCursor == filtered.NextCursor {
		t.Fatal("empty filtered cursor did not advance")
	}
}
func TestPaginationRetentionPruneAndScope(t *testing.T) {
	s, p := database(t)
	appendCommit(t, s, p, fact("one", "a"), fact("two", "b"), fact("three", "a"))
	first := page(t, s, eventlog.Query{Limit: 1, Types: []string{"a", "a"}})
	if len(first.Events) != 1 || !first.HasMore {
		t.Fatal(first)
	}
	second := page(t, s, eventlog.Query{Limit: 1, Types: []string{"a"}, After: first.NextCursor})
	if len(second.Events) != 1 || second.HasMore || second.Events[0].ID != "three" {
		t.Fatal(second)
	}
	_, err := s.ListEvents(t.Context(), eventlog.Query{After: first.NextCursor, Types: []string{"b"}})
	if !errors.Is(err, eventlog.ErrInvalidCursor) {
		t.Fatalf("wrong filter %v", err)
	}
	other, err := New(p, Config{Schema: s.config.Schema, Source: "product", Scope: "other"})
	if err != nil {
		t.Fatal(err)
	}
	other.EnsureScope(t.Context())
	_, err = other.ListEvents(t.Context(), eventlog.Query{After: first.NextCursor, Types: []string{"a"}})
	if !errors.Is(err, eventlog.ErrInvalidCursor) {
		t.Fatalf("wrong scope %v", err)
	}
	old := page(t, s, eventlog.Query{Limit: 1})
	s.config.Now = func() time.Time { return time.Now().Add(91 * 24 * time.Hour) }
	retained := page(t, s, eventlog.Query{})
	if len(retained.Events) != 0 {
		t.Fatal("retention depended on pruning")
	}
	_, err = s.ListEvents(t.Context(), eventlog.Query{After: old.NextCursor})
	if !errors.Is(err, eventlog.ErrExpiredCursor) {
		t.Fatalf("old cursor %v", err)
	}
	n, err := s.Prune(t.Context(), 1)
	if err != nil || n != 1 {
		t.Fatalf("bounded prune %d %v", n, err)
	}
	after := page(t, s, eventlog.Query{After: retained.NextCursor})
	if len(after.Events) != 0 {
		t.Fatal(after)
	}
	n, err = s.Prune(t.Context(), 10)
	if err != nil || n != 2 {
		t.Fatalf("prune rest %d %v", n, err)
	}
	page(t, s, eventlog.Query{After: retained.NextCursor})
}
func TestOppositeScopeOrderCommitsWithoutDeadlock(t *testing.T) {
	s, p := database(t)
	b, err := New(p, Config{Schema: s.config.Schema, Source: "product", Scope: "b"})
	if err != nil {
		t.Fatal(err)
	}
	tx1, err := p.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx1.Rollback(t.Context())
	tx2, err := p.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx2.Rollback(t.Context())
	for _, x := range []struct {
		s  *Store
		tx pgx.Tx
		id string
	}{{s, tx1, "a1"}, {b, tx2, "b2"}, {b, tx1, "b1"}, {s, tx2, "a2"}} {
		if err = x.s.AppendTx(t.Context(), x.tx, fact(x.id, "x")); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 2)
	go func() { done <- tx1.Commit(ctx) }()
	go func() { done <- tx2.Commit(ctx) }()
	for range 2 {
		if err = <-done; err != nil {
			t.Fatal(err)
		}
	}
	aPage, bPage := page(t, s, eventlog.Query{}), page(t, b, eventlog.Query{})
	if len(aPage.Events) != 2 || len(bPage.Events) != 2 {
		t.Fatal("missing events")
	}
	if aPage.Events[0].ID[1:] != bPage.Events[0].ID[1:] {
		t.Fatal("scopes interleaved")
	}
}

func TestFactsImmutableAndDuplicateRollsBack(t *testing.T) {
	s, p := database(t)
	appendCommit(t, s, p, fact("id", "a"))
	_, err := p.Exec(t.Context(), `UPDATE `+s.schema+`.eventlog_events SET data='{}'`)
	if err == nil {
		t.Fatal("event update accepted")
	}
	tx, err := p.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err = s.AppendTx(t.Context(), tx, fact("new", "a")); err != nil {
		t.Fatal(err)
	}
	if err = s.AppendTx(t.Context(), tx, fact("id", "a")); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err == nil {
		t.Fatal("duplicate committed")
	}
	result := page(t, s, eventlog.Query{})
	if len(result.Events) != 1 || result.Events[0].ID != "id" {
		t.Fatal("partial transaction survived")
	}
	cursor, _ := (eventlog.JSONCursor{}).Decode(result.NextCursor)
	cursor.Boundary = "wrong"
	_, err = s.ListEvents(t.Context(), eventlog.Query{After: (eventlog.JSONCursor{}).Encode(cursor)})
	if !errors.Is(err, eventlog.ErrExpiredCursor) {
		t.Fatalf("boundary mismatch %v", err)
	}
	cursor.Epoch = "restored"
	_, err = s.ListEvents(t.Context(), eventlog.Query{After: (eventlog.JSONCursor{}).Encode(cursor)})
	if !errors.Is(err, eventlog.ErrExpiredCursor) {
		t.Fatalf("epoch mismatch %v", err)
	}
}

func TestSQLCaptureSharesDeferredQueue(t *testing.T) {
	s, p := database(t)
	tx, err := p.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	// This is the insert a product-owned source-journal trigger performs. It uses
	// no Go AppendTx call, and cannot become visible before the domain commit.
	_, err = tx.Exec(t.Context(), `INSERT INTO `+s.schema+`.eventlog_pending(source,scope,id,event_source,event_type,version,occurred_at,data) VALUES($1,$2,'sql-fact','journal','created',1,now(),'{"from":"sql"}')`, s.config.Source, s.config.Scope)
	if err != nil {
		t.Fatal(err)
	}
	before := page(t, s, eventlog.Query{})
	if len(before.Events) != 0 {
		t.Fatal("uncommitted SQL capture visible")
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	after := page(t, s, eventlog.Query{After: before.NextCursor})
	if len(after.Events) != 1 || after.Events[0].ID != "sql-fact" || after.Events[0].Source != "journal" {
		t.Fatalf("%#v", after)
	}
	var pending int
	if err = p.QueryRow(t.Context(), `SELECT count(*) FROM `+s.schema+`.eventlog_pending`).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("pending survived commit: %d %v", pending, err)
	}
}
