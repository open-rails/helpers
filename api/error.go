// Package api is the fleet's HTTP API envelope: one error shape, one type
// and code vocabulary, one status inference, and the list/pagination shapes
// that go with them. It depends on nothing outside the standard library, so
// every library in the fleet can speak it. Framework bindings live in
// sub-modules (adapters/gin).
//
// The canonical body is the shape AuthKit and OpenRails deploy today:
//
//	{"error":{"type":"...","code":"...","message":"...","param":"...","request_id":"...","metadata":{}}}
//
// GinAPI wraps that in a top-level "object":"error" discriminator. That is a
// COMPATIBILITY DIFFERENCE, not the canonical form: see CompatEnvelope, and
// compat/DECISION-object-discriminator.md for the evidence and the open
// decision. Nothing here emits the discriminator on its own.
package api

import (
	"encoding/json"
	"errors"
	"net/http"
)

// Type is the transport-level category of a failure: what a client does next.
// The constants are the ones AuthKit and OpenRails already put on the wire.
// Type is an open string so a domain may add its own.
//
// Adding a transport type is a wire migration for every parser in the fleet.
// A failure that only needs explaining gets a Code instead — that is the field
// a SPA branches on.
type Type string

const (
	// TypeInvalidRequest: fix the input, then retry. 400, and every other
	// unclassified 4xx — including 404, 409, 415 and 422. Both deployed
	// writers map them here; the reason lives in Code.
	TypeInvalidRequest Type = "invalid_request_error"
	// TypeAuthentication: authenticate or refresh, then retry. 401.
	TypeAuthentication Type = "authentication_error"
	// TypeAuthorization: never retry as this principal. 403.
	TypeAuthorization Type = "authorization_error"
	// TypeRateLimit: back off and retry later. 429.
	TypeRateLimit Type = "rate_limit_error"
	// TypeAPI: the server failed or lacks the capability. 5xx, 501 included.
	TypeAPI Type = "api_error"
	// TypeCard is OpenRails' domain extension for 402: collect a new payment
	// method. It is inferred only for 402, which no other fleet service uses.
	TypeCard Type = "card_error"
)

// Code is the stable, machine-readable reason, and the field a client branches
// on. Transport-level codes are below; each library keeps its own domain
// catalog (AuthKit's hundreds of auth codes, OpenRails' decline codes) and
// emits those in the same field.
type Code string

const (
	CodeInvalidParam  Code = "invalid_param"
	CodeMissingParam  Code = "missing_param"
	CodeInvalidFormat Code = "invalid_format"

	CodeResourceNotFound Code = "resource_not_found"
	CodeResourceConflict Code = "resource_conflict"

	CodeAuthenticationRequired Code = "authentication_required"
	CodeInvalidToken           Code = "invalid_token"
	CodeTokenExpired           Code = "token_expired"
	CodeResourceAccessDenied   Code = "resource_access_denied"

	CodeRateLimitExceeded Code = "rate_limit_exceeded"

	// CodeModerationRejected is a moderation/policy refusal of submitted
	// content: 422 under TypeInvalidRequest. The transport category says
	// "the request was not acted on"; this code says why, and is what a SPA
	// branches on to show the author a reason instead of a retry.
	CodeModerationRejected Code = "moderation_rejected"
	// CodeNotConfigured is an optional capability the operator never wired.
	CodeNotConfigured Code = "not_configured"
	// CodeNotImplemented is a capability this build does not have: 501 under
	// TypeAPI. Same transport handling as any 5xx; the code is what tells the
	// client to hide the feature rather than retry.
	CodeNotImplemented Code = "not_implemented"

	CodePaymentFailed      Code = "payment_failed"
	CodeInternalError      Code = "internal_error"
	CodeServiceUnavailable Code = "service_unavailable"
)

// ErrorObject is the error detail carried under the envelope's "error" key.
// Param is a pointer so "absent" and "empty" stay distinguishable in Go; on
// the wire omitempty makes it identical to AuthKit's and OpenRails' shape.
// Callers never build the pointer — the constructors take a plain string.
type ErrorObject struct {
	Type      Type           `json:"type"`
	Code      Code           `json:"code,omitempty"`
	Message   string         `json:"message"`
	Param     *string        `json:"param,omitempty"`
	RequestID string         `json:"request_id,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// Envelope is the canonical error body: {"error":{...}}.
type Envelope struct {
	Error ErrorObject `json:"error"`
}

// CompatEnvelope is Envelope plus GinAPI's top-level discriminator. It exists
// so a host that must keep the discriminator can, and so both candidate bodies
// can be shown side by side. Nothing in api emits it by default: whether
// the discriminator belongs in the canonical envelope is an open owner
// decision (compat/DECISION-object-discriminator.md).
type CompatEnvelope struct {
	Object string      `json:"object"` // always "error"
	Error  ErrorObject `json:"error"`
}

// WithObject renders env in the GinAPI-compatible shape.
func WithObject(env Envelope) CompatEnvelope {
	return CompatEnvelope{Object: "error", Error: env.Error}
}

// NewEnvelope builds the canonical envelope, defaulting the type and
// sanitizing metadata. Every path to the wire goes through it.
func NewEnvelope(obj ErrorObject) Envelope {
	if obj.Type == "" {
		obj.Type = TypeAPI
	}
	if obj.Param != nil && *obj.Param == "" {
		obj.Param = nil
	}
	obj.Metadata = SanitizeMetadata(obj.Metadata)
	return Envelope{Error: obj}
}

// TypeForStatus is the transport category an HTTP status determines, matching
// what AuthKit and OpenRails deploy today. 404, 409, 415 and 422 are
// deliberately TypeInvalidRequest: both writers already map them there, and
// splitting them out is a wire migration, not a bug fix.
func TypeForStatus(status int) Type {
	switch status {
	case http.StatusUnauthorized:
		return TypeAuthentication
	case http.StatusForbidden:
		return TypeAuthorization
	case http.StatusPaymentRequired:
		return TypeCard
	case http.StatusTooManyRequests:
		return TypeRateLimit
	}
	if status >= 500 {
		return TypeAPI
	}
	return TypeInvalidRequest
}

// CodeForStatus is the default code for a status, matching OpenRails'
// inferErrorTypeAndCode. Root writers do NOT apply it: an absent code means
// "no machine reason beyond the status", and filling one in is wire-visible —
// Doujins' client displays code in preference to message, so an invented code
// replaces a human sentence with a machine string (compat/golden/parsers.json).
// OpenRails calls it explicitly because its writers have always emitted a code.
func CodeForStatus(status int) Code {
	switch status {
	case http.StatusUnauthorized:
		return CodeAuthenticationRequired
	case http.StatusForbidden:
		return CodeResourceAccessDenied
	case http.StatusPaymentRequired:
		return CodePaymentFailed
	case http.StatusNotFound:
		return CodeResourceNotFound
	case http.StatusConflict:
		return CodeResourceConflict
	case http.StatusTooManyRequests:
		return CodeRateLimitExceeded
	case http.StatusNotImplemented:
		return CodeNotImplemented
	case http.StatusServiceUnavailable:
		return CodeServiceUnavailable
	}
	if status >= 500 {
		return CodeInternalError
	}
	return CodeInvalidParam
}

// Error is the transport carrier: a library maps its own sentinel onto one of
// these and a single writer renders it. It is not a catalog — libraries keep
// their own.
type Error struct {
	Status    int
	Type      Type
	Code      Code
	Message   string
	RequestID string
	Metadata  map[string]any

	param *string
	cause error
}

// E builds an Error, inferring the type from the status when none is given.
func E(status int, code Code, message string) *Error {
	return &Error{Status: status, Type: TypeForStatus(status), Code: code, Message: message}
}

func (e *Error) Error() string {
	if e.cause != nil {
		return e.Message + ": " + e.cause.Error()
	}
	return e.Message
}

func (e *Error) Unwrap() error { return e.cause }

// Is matches any *Error carrying the same Code, so a sentinel, a fresh E() and
// a wrapped copy are one identity.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code != "" && t.Code == e.Code
}

// Param is the offending request field, or "" when none was named.
func (e *Error) Param() string {
	if e.param == nil {
		return ""
	}
	return *e.param
}

func (e *Error) WithCause(cause error) *Error { e.cause = cause; return e }
func (e *Error) WithType(t Type) *Error       { e.Type = t; return e }

// WithParam names the offending field. It takes a plain string; the pointer
// the wire needs is this package's problem, not the caller's.
func (e *Error) WithParam(param string) *Error {
	if param == "" {
		e.param = nil
		return e
	}
	p := param
	e.param = &p
	return e
}

func (e *Error) WithRequestID(id string) *Error { e.RequestID = id; return e }

// WithMetadata merges public metadata. It sanitizes on the way in, so an
// unsafe value never reaches the struct, let alone the wire.
func (e *Error) WithMetadata(m map[string]any) *Error {
	merged := map[string]any{}
	for k, v := range e.Metadata {
		merged[k] = v
	}
	for k, v := range m {
		merged[k] = v
	}
	e.Metadata = SanitizeMetadata(merged)
	return e
}

// Envelope renders the error as its wire body.
func (e *Error) Envelope() Envelope {
	return NewEnvelope(ErrorObject{
		Type:      e.Type,
		Code:      e.Code,
		Message:   e.Message,
		Param:     e.param,
		RequestID: e.RequestID,
		Metadata:  e.Metadata,
	})
}

// AsError returns the *Error in err's chain, or nil.
func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return nil
}

// EnvelopeFor derives the wire status and envelope for any error. An error
// that is not an *Error — and any 500 — is rendered as a bare internal error,
// so an internal message never reaches the wire. 501, 502 and 503 state a
// deliberate operational condition and keep theirs.
func EnvelopeFor(err error) (int, Envelope) {
	e := AsError(err)
	if e == nil {
		return http.StatusInternalServerError, NewEnvelope(ErrorObject{Type: TypeAPI, Code: CodeInternalError, Message: "internal error"})
	}
	status := e.Status
	if status == 0 {
		status = http.StatusInternalServerError
	}
	if status == http.StatusInternalServerError {
		return status, NewEnvelope(ErrorObject{Type: TypeAPI, Code: CodeInternalError, Message: "internal error", RequestID: e.RequestID})
	}
	env := e.Envelope()
	if env.Error.Type == "" {
		env.Error.Type = TypeForStatus(status)
	}
	return status, env
}

// WriteError writes err as the canonical envelope over net/http.
func WriteError(w http.ResponseWriter, err error) {
	status, env := EnvelopeFor(err)
	WriteJSON(w, status, env)
}

// WriteJSON writes v as a JSON body with the canonical content type.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}
