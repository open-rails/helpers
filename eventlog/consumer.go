package eventlog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Subscription identifies a consumer checkpoint. Changing any field selects a
// different checkpoint. A filter change must not reuse the old cursor.
type Subscription struct {
	Source, Scope, Name string
	Types               []string
}

func (s Subscription) Key() (string, error) {
	if s.Source == "" || s.Scope == "" || s.Name == "" {
		return "", fmt.Errorf("%w: source, scope and subscription name required", ErrInvalidQuery)
	}
	q, err := Normalize(Query{Types: s.Types})
	if err != nil {
		return "", err
	}
	s.Types = q.Types
	// nil and empty filters are equivalent.
	if len(s.Types) == 0 {
		s.Types = nil
	}
	b, _ := json.Marshal(s)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

// Consumer polls one page. The owner schedules retries and polling. Load returns
// an empty cursor only for a new subscription. Commit must atomically apply
// idempotent effects and persist page.NextCursor, conditional on the checkpoint
// still equaling previous (or serialize consumers). It must commit empty pages,
// too. With external effects, use event IDs as idempotency keys before saving.
// Neither Load nor Commit is producer-side state.
type Consumer struct {
	Reader       Reader
	Subscription Subscription
	Limit        int
	Load         func(ctx context.Context, key string) (string, error)
	Commit       func(ctx context.Context, key, previous string, page Page) error
}

func (c Consumer) Poll(ctx context.Context) (hasMore bool, err error) {
	if c.Reader == nil || c.Load == nil || c.Commit == nil {
		return false, fmt.Errorf("%w: reader, load and commit required", ErrInvalidQuery)
	}
	key, err := c.Subscription.Key()
	if err != nil {
		return false, err
	}
	q, err := Normalize(Query{Types: c.Subscription.Types, Limit: c.Limit})
	if err != nil {
		return false, err
	}
	q.After, err = c.Load(ctx, key)
	if err != nil {
		return false, err
	}
	page, err := c.Reader.ListEvents(ctx, q)
	if err != nil {
		return false, err
	}
	if page.NextCursor == "" {
		return false, fmt.Errorf("eventlog: reader returned empty next cursor")
	}
	if err = c.Commit(ctx, key, q.After, page); err != nil {
		return false, err
	}
	return page.HasMore, nil
}
