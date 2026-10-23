package eventlog

import (
	"context"
	"testing"
	"time"
)

type legacySnapshot struct{}

func (legacySnapshot) Stream(context.Context, time.Time) (Stream, error) {
	return Stream{Epoch: "legacy", Head: Position{Number: 2, ID: "boundary-two"}}, nil
}
func (legacySnapshot) Boundary(_ context.Context, pos int64) (string, error) {
	return "boundary-one", nil
}
func (legacySnapshot) Events(_ context.Context, after int64, _ []string, _ int) ([]Record, error) {
	if after > 0 {
		return []Record{{Position: 2, Boundary: "boundary-two", Event: Event{ID: "2"}}}, nil
	}
	return []Record{{Position: 1, Boundary: "boundary-one", Event: Event{ID: "1"}}, {Position: 2, Boundary: "boundary-two", Event: Event{ID: "2"}}}, nil
}
func TestLegacyIndependentBoundaryAndNormalizer(t *testing.T) {
	log := Log{Source: "contentkit", Scope: "app", ReadSnapshot: func(_ context.Context, fn func(Snapshot) error) error { return fn(legacySnapshot{}) }, NormalizeQuery: func(q Query) (Query, error) {
		// Existing AuthKit Kinds preserve outer whitespace; keep the legacy exact
		// filter while applying the shared bounds and default page size.
		original := q.Types
		q, err := Normalize(q)
		q.Types = original
		return q, err
	}}
	first, err := log.ListEvents(t.Context(), Query{Limit: 1, Types: []string{" legacy "}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := (JSONCursor{}).Decode(first.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	if c.Boundary != "boundary-one" || c.Types[0] != " legacy " {
		t.Fatalf("legacy cursor changed: %#v", c)
	}
	next, err := log.ListEvents(t.Context(), Query{After: first.NextCursor, Types: []string{" legacy "}})
	if err != nil || len(next.Events) != 1 || next.Events[0].ID != "2" {
		t.Fatalf("resume %#v %v", next, err)
	}
}
