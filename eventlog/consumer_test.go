package eventlog

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestConsumerRetriesAndCommitsEmptyPages(t *testing.T) {
	ctx := t.Context()
	saved := "old"
	calls := 0
	fail := true
	boom := errors.New("effect failed")
	c := Consumer{Subscription: Subscription{Source: "authkit", Scope: "tenant", Name: "projection", Types: []string{"b", "a", "a"}},
		Reader: ReaderFunc(func(_ context.Context, q Query) (Page, error) {
			if q.After != "old" || !reflect.DeepEqual(q.Types, []string{"a", "b"}) {
				t.Fatalf("query %#v", q)
			}
			return Page{NextCursor: "next", Events: []Event{}}, nil
		}),
		Load: func(context.Context, string) (string, error) { return saved, nil },
		Commit: func(_ context.Context, key, previous string, p Page) error {
			calls++
			if previous != saved || key == "" {
				t.Fatal("checkpoint contract")
			}
			if fail {
				return boom
			}
			saved = p.NextCursor
			return nil
		},
	}
	if _, err := c.Poll(ctx); !errors.Is(err, boom) || saved != "old" {
		t.Fatalf("failed commit changed checkpoint: %s %v", saved, err)
	}
	fail = false
	if more, err := c.Poll(ctx); err != nil || more || saved != "next" || calls != 2 {
		t.Fatalf("empty page not committed: %v %s %v", more, saved, err)
	}
}
func TestSubscriptionKeyCanonicalAndSeparated(t *testing.T) {
	a := Subscription{Source: "one", Scope: "scope", Name: "consumer", Types: []string{"b", "a", "a"}}
	ka, _ := a.Key()
	a.Types = []string{"a", "b"}
	kb, _ := a.Key()
	if ka != kb {
		t.Fatal("equivalent filters differ")
	}
	a.Source = "two"
	kc, _ := a.Key()
	if ka == kc {
		t.Fatal("sources collide")
	}
	a.Types = nil
	ka, _ = a.Key()
	a.Types = []string{}
	kb, _ = a.Key()
	if ka != kb {
		t.Fatal("empty filters differ")
	}
}
func TestNormalizeUnknownTypesAndBounds(t *testing.T) {
	in := []string{" future ", "a", "a"}
	q, err := Normalize(Query{Types: in})
	if err != nil || q.Limit != 100 || !reflect.DeepEqual(q.Types, []string{"a", "future"}) || in[0] != " future " {
		t.Fatalf("%#v %v", q, err)
	}
	for _, q := range []Query{{Limit: 501}, {Limit: -1}, {Types: []string{" "}}, {Types: make([]string, 101)}} {
		if _, err := Normalize(q); !errors.Is(err, ErrInvalidQuery) {
			t.Fatalf("accepted %#v", q)
		}
	}
}
