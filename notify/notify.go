// Package notify defines notices and actionable inbox items shared by host
// applications. Authentication, rendering, delivery channels and the action
// that remedies a problem belong to the application. Reading never resolves
// an item. Existing domain records can be projected as Items without copying
// their authoritative state into another queue.
package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Audience selects an authorization boundary, not a delivery channel.
type Audience string

const (
	AudienceUser     Audience = "user"
	AudienceOperator Audience = "operator"
)

// Recipient names a user or an operator queue within the store's scope. Hosts
// authorize this pair; an email address is never an identity join.
type Recipient struct {
	Audience Audience `json:"audience"`
	ID       string   `json:"id"`
}

// Origin preserves a producer's deployment, namespace and stable item ID.
// Scope can identify a merchant, project or account issuer. Each recipient
// receives at most one item for the same Origin in one local store scope.
type Origin struct {
	System string `json:"system"`
	Scope  string `json:"scope"`
	ID     string `json:"id"`
}

// Status is independent of read state. A general notice has no workflow;
// actionable items begin open and are explicitly resolved or dismissed.
type Status string

const (
	StatusNotice    Status = ""
	StatusOpen      Status = "open"
	StatusResolved  Status = "resolved"
	StatusDismissed Status = "dismissed"
)

// Input is trusted application-authored content. Kind is extensible. Data must
// contain only fields the recipient may see. CreatedAt is the source time.
// Publish accepts StatusNotice or StatusOpen; workflow transitions are separate.
type Input struct {
	Recipient Recipient       `json:"recipient"`
	Origin    Origin          `json:"origin"`
	Kind      string          `json:"kind"`
	Data      json.RawMessage `json:"data"`
	CreatedAt time.Time       `json:"created_at"`
	Status    Status          `json:"status,omitempty"`
	// ActionRequested records the item's category at creation. With empty Status
	// it is historical information, not a claim that the issue is still open.
	ActionRequested bool   `json:"action_requested,omitempty"`
	Severity        string `json:"severity,omitempty"`
}

// Item is an inbox record or a projection of an existing domain record.
// ReadAt and ResolvedAt describe different operations; neither implies the other.
// Revision is positive in the durable store and zero for unversioned projections.
type Item struct {
	ID string `json:"id"`
	Input
	ReadAt     *time.Time `json:"read_at"`
	ResolvedAt *time.Time `json:"resolved_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	Revision   int64      `json:"revision"`
}

// Errors are stable outcomes of validation, scoped lookup and concurrent edits.
var (
	ErrInvalid         = errors.New("notify: invalid input")
	ErrNotFound        = errors.New("notify: item not found")
	ErrConflict        = errors.New("notify: item changed")
	ErrRecipientErased = errors.New("notify: recipient erased")
)

// ValidateRecipient checks the scope-local identity; it grants no authority.
func ValidateRecipient(r Recipient) error {
	if (r.Audience != AudienceUser && r.Audience != AudienceOperator) || !validText(r.ID) {
		return fmt.Errorf("%w: audience and recipient ID are required", ErrInvalid)
	}
	return nil
}

// Validate checks a publish input and supplies an empty JSON object when omitted.
func Validate(in Input) (Input, error) {
	if err := ValidateRecipient(in.Recipient); err != nil {
		return in, err
	}
	if !validText(in.Origin.System) || !validText(in.Origin.Scope) || !validText(in.Origin.ID) || !validText(in.Kind) || len(in.Severity) > 64 || strings.ContainsRune(in.Severity, 0) || in.CreatedAt.IsZero() {
		return in, fmt.Errorf("%w: origin, kind and created time are required", ErrInvalid)
	}
	if in.Status != StatusNotice && in.Status != StatusOpen {
		return in, fmt.Errorf("%w: publish status must be notice or open", ErrInvalid)
	}
	if in.Status == StatusOpen {
		in.ActionRequested = true
	}
	if len(in.Data) == 0 {
		in.Data = json.RawMessage(`{}`)
	}
	var object map[string]json.RawMessage
	if len(in.Data) > 64<<10 || json.Unmarshal(in.Data, &object) != nil || object == nil {
		return in, fmt.Errorf("%w: data must be a JSON object up to 64 KiB", ErrInvalid)
	}
	return in, nil
}

func validText(v string) bool {
	return strings.TrimSpace(v) != "" && len(v) <= 512 && !strings.ContainsRune(v, 0)
}

// ListOptions selects a bounded, newest-first page. Status nil includes notices
// and work items; a pointer to StatusNotice selects only general notices.
type ListOptions struct {
	Limit      int
	Cursor     string
	UnreadOnly bool
	Status     *Status
}

// Page is a bounded page; NextCursor is empty at the end of a current scan.
type Page struct {
	Items      []Item `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// Publisher is implemented by durable stores and application adapters. A retry
// with the same recipient and origin preserves the original content/read state.
type Publisher interface {
	Publish(context.Context, Input) (Item, error)
}

// Reader is a recipient-scoped inbox. Callers supply an authorized recipient.
type Reader interface {
	List(context.Context, Recipient, ListOptions) (Page, error)
}
