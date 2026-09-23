// Package auth defines the small values shared by request authentication
// providers and their consumers. It contains no verifier or authorization policy.
package auth

import (
	"context"
	"errors"
)

// Kind records verified credential provenance, never a role or permission.
type Kind string

const (
	KindUser              Kind = "user"
	KindAPIKey            Kind = "api_key"
	KindRemoteApplication Kind = "remote_application"
	KindDelegated         Kind = "delegated"
	KindDeviceKey         Kind = "device_key"
)

// Identity identifies the authenticated actor within its issuer's namespace.
// Subject is stable: it is not a display name or an application-specific account
// mapping. Contact and session fields are optional metadata, not authority.
type Identity struct {
	Kind          Kind
	Issuer        string
	Subject       string
	Email         string
	Username      string
	SessionID     string
	EmailVerified bool
}

// Principal is the result of verifying one HTTP request, including any required
// sender proof. Consumers retain it only for that request; they must not reuse it
// for another request or after changing the credential, method, or signed URL.
// Identity-only providers need not implement PermissionChecker.
type Principal interface {
	Identity() Identity
}

// Scope names an authority-owned immutable resource. Names and request selectors
// do not grant authority; the host resolves this value before checking access.
type Scope struct {
	Authority string
	ID        string
}

// PermissionChecker is an optional capability of an authenticated Principal.
// Can evaluates the exact scope and permission without verifying the request
// again. It must retain credential ceilings and scope bindings. Consumers must
// deny privileged access when this capability is absent; never infer a grant
// from identity metadata. An error denotes an unavailable check, not permission.
type PermissionChecker interface {
	Can(context.Context, Scope, string) (bool, error)
}

// Authentication failures are classified with errors.Is. Providers may wrap
// their own errors; consumers must not expose provider error text to clients.
var (
	ErrUnauthenticated     = errors.New("authentication required")
	ErrForbidden           = errors.New("authentication policy refused")
	ErrUnavailable         = errors.New("authentication unavailable")
	ErrSenderProofRequired = errors.New("sender proof required")
	ErrExpired             = errors.New("credential expired")
	ErrRevoked             = errors.New("credential revoked")
)
