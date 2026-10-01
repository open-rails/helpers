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
// from identity metadata. An error never grants. One matching ErrExpired or
// ErrRevoked (with ErrUnauthenticated) is a credential failure: the credential
// ended after the request was verified, so answer 401. Any other error denotes
// an unavailable check: answer 503.
type PermissionChecker interface {
	Can(context.Context, Scope, string) (bool, error)
}

// RecentSignInChecker is an optional capability of a user Principal, for
// actions that move money or grant access. CheckRecentSignIn is nil when the
// credential's own sign-in is recent enough by the provider's policy, checked
// without verifying the request again. Otherwise its error matches
// ErrStepUpRequired when the user must sign in again (it may carry the
// provider's challenge as Metadata() map[string]any, which consumers return
// to the client unchanged); ErrExpired or ErrRevoked (with
// ErrUnauthenticated) when the credential ended; ErrForbidden for a
// credential with no sign-in of its own; ErrUnavailable when the check could
// not run. Consumers must refuse those actions to a user Principal without
// this capability.
type RecentSignInChecker interface {
	CheckRecentSignIn(context.Context) error
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
	ErrStepUpRequired      = errors.New("step-up required")
)
