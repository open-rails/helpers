package eventlog

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"slices"
	"time"
)

// Position and Stream describe storage state within one consistent snapshot.
// Floor includes records expired by read-time retention even before pruning.
type Position struct {
	Number int64
	ID     string
}
type Stream struct {
	Epoch string
	Head  Position
	Floor Position
}
type Record struct {
	Event    Event
	Position int64
}

// Snapshot must expose committed rows from one repeatable-read snapshot.
// Events returns strictly increasing positions above after, filtered by exact
// types (empty means all), with at most limit records. Head's Floor is the last
// expired position at cutoff. Storage must order RecordedAt with positions.
type Snapshot interface {
	Stream(context.Context, time.Time) (Stream, error)
	Boundary(context.Context, int64) (string, error)
	Events(context.Context, int64, []string, int) ([]Record, error)
}

// CursorCodec allows a product to retain its already-issued cursor envelope
// during adoption. Decode must reject malformed/oversized input, unsupported
// versions and negative positions. Missing boundaries must never be invented.
type CursorCodec interface {
	Encode(Cursor) string
	Decode(string) (Cursor, error)
}
type Cursor struct {
	Version  int      `json:"v"`
	Source   string   `json:"source"`
	Scope    string   `json:"scope"`
	Epoch    string   `json:"epoch"`
	Types    []string `json:"types,omitempty"`
	Position int64    `json:"position"`
	Boundary string   `json:"boundary,omitempty"`
}

type JSONCursor struct{}

func (JSONCursor) Encode(c Cursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}
func (JSONCursor) Decode(raw string) (Cursor, error) {
	var c Cursor
	if len(raw) > 32768 {
		return c, ErrInvalidCursor
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return c, ErrInvalidCursor
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil || d.Decode(new(any)) != io.EOF || c.Version != 1 || c.Epoch == "" || c.Position < 0 {
		return c, ErrInvalidCursor
	}
	return c, nil
}

// Log centralizes paging, retention and cursor validation over product storage.
// A codec is optional for new logs; existing logs supply their legacy codec.
// ReadSnapshot must run the callback once using a single committed snapshot.
type Log struct {
	Source       string
	Scope        string
	Retention    time.Duration
	Now          func() time.Time
	Codec        CursorCodec
	ReadSnapshot func(context.Context, func(Snapshot) error) error
}

func (l Log) ListEvents(ctx context.Context, q Query) (Page, error) {
	retention := l.Retention
	if retention == 0 {
		retention = DefaultRetention
	}
	page := Page{Events: []Event{}, RetentionSeconds: int64(retention / time.Second)}
	if retention < 0 || l.Source == "" || l.Scope == "" || l.ReadSnapshot == nil {
		return page, ErrInvalidQuery
	}
	q, err := Normalize(q)
	if err != nil {
		return page, err
	}
	codec := l.Codec
	if codec == nil {
		codec = JSONCursor{}
	}
	var prior Cursor
	if q.After != "" {
		prior, err = codec.Decode(q.After)
		if err != nil {
			return page, err
		}
		if prior.Source != l.Source || prior.Scope != l.Scope || !slices.Equal(prior.Types, q.Types) {
			return page, ErrInvalidCursor
		}
	}
	now := time.Now()
	if l.Now != nil {
		now = l.Now()
	}
	err = l.ReadSnapshot(ctx, func(s Snapshot) error {
		stream, err := s.Stream(ctx, now.Add(-retention))
		if err != nil {
			return err
		}
		after := stream.Floor.Number
		if q.After != "" {
			if prior.Epoch != stream.Epoch || prior.Position < stream.Floor.Number || prior.Position > stream.Head.Number {
				return ErrExpiredCursor
			}
			after = prior.Position
			boundary := stream.Floor.ID
			if after != stream.Floor.Number {
				boundary, err = s.Boundary(ctx, after)
				if err != nil {
					return err
				}
			}
			if boundary != prior.Boundary {
				return ErrExpiredCursor
			}
		}
		rows, err := s.Events(ctx, after, q.Types, q.Limit+1)
		if err != nil {
			return err
		}
		position := stream.Head
		for _, row := range rows {
			if len(page.Events) == q.Limit {
				page.HasMore = true
				break
			}
			page.Events = append(page.Events, row.Event)
			position = Position{Number: row.Position, ID: row.Event.ID}
		}
		if !page.HasMore {
			position = stream.Head
		}
		page.NextCursor = codec.Encode(Cursor{Version: 1, Source: l.Source, Scope: l.Scope, Epoch: stream.Epoch, Types: q.Types, Position: position.Number, Boundary: position.ID})
		return nil
	})
	if err != nil {
		return Page{}, err
	}
	return page, nil
}
