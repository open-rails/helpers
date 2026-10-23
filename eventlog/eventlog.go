// Package eventlog defines retained, independently scoped event logs and
// consumer-owned checkpoints. It does not register consumers or deliver events.
package eventlog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

const (
	DefaultLimit     = 100
	MaxLimit         = 500
	MaxTypes         = 100
	DefaultRetention = 90 * 24 * time.Hour
)

var (
	ErrInvalidQuery  = errors.New("eventlog: invalid query")
	ErrInvalidCursor = errors.New("eventlog: invalid cursor")
	ErrExpiredCursor = errors.New("eventlog: cursor expired; resynchronize current state")
)

// Event is a product-owned fact. IDs are opaque; only cursors define order.
// RecordedAt is storage time; OccurredAt may describe an earlier domain event.
type Event struct {
	ID         string          `json:"id"`
	Source     string          `json:"source"`
	Scope      string          `json:"scope"`
	Type       string          `json:"type"`
	Version    int             `json:"version"`
	OccurredAt time.Time       `json:"occurred_at"`
	RecordedAt time.Time       `json:"recorded_at"`
	Subject    Subject         `json:"subject"`
	Data       json.RawMessage `json:"data"`
}

type Subject struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}
type Query struct {
	After string   `json:"after,omitempty"`
	Types []string `json:"types,omitempty"`
	Limit int      `json:"limit,omitempty"`
}
type Page struct {
	Events           []Event `json:"events"`
	NextCursor       string  `json:"next_cursor"`
	HasMore          bool    `json:"has_more"`
	RetentionSeconds int64   `json:"retention_seconds"`
}

// Reader is already bound to an authorized source and scope. Embedded and HTTP
// product clients implement this same interface; authentication stays with them.
type Reader interface {
	ListEvents(context.Context, Query) (Page, error)
}
type ReaderFunc func(context.Context, Query) (Page, error)

func (f ReaderFunc) ListEvents(ctx context.Context, q Query) (Page, error) { return f(ctx, q) }

// Normalize copies, sorts and deduplicates exact filters. Unknown types are
// valid, allowing older producers and newer consumers to coexist.
func Normalize(q Query) (Query, error) {
	if q.Limit == 0 {
		q.Limit = DefaultLimit
	}
	if q.Limit < 1 || q.Limit > MaxLimit {
		return q, fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalidQuery, MaxLimit)
	}
	if len(q.Types) > MaxTypes {
		return q, fmt.Errorf("%w: at most %d types", ErrInvalidQuery, MaxTypes)
	}
	q.Types = slices.Clone(q.Types)
	for i, t := range q.Types {
		t = strings.TrimSpace(t)
		if t == "" || len(t) > 128 {
			return q, fmt.Errorf("%w: invalid type", ErrInvalidQuery)
		}
		q.Types[i] = t
	}
	slices.Sort(q.Types)
	q.Types = slices.Compact(q.Types)
	return q, nil
}
