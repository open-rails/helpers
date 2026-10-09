package auth

import (
	"context"
	"net/http"
)

// Auth is the host's auth provider as a library that serves a merchant's
// routes (OpenRails) mounts it: middleware for each route tier, and the
// identity the gates verified. Each gate answers the request itself when it
// refuses, in the provider's format. Gates of one provider stack: the request
// is verified once.
type Auth interface {
	// Required admits a user signed in, checked live: a revoked sign-in or a
	// banned or deleted account is refused, and so is an application.
	Required() func(http.Handler) http.Handler
	// RequirePermission admits a request holding exactly permission (never
	// a pattern) in the group that controls the merchant, checked live. With
	// no such group configured it refuses every request.
	RequirePermission(permission string) func(http.Handler) http.Handler
	// Sensitive admits a person who signed in recently enough for an action
	// that moves money or grants access, else asks them to sign in again. A
	// credential with no sign-in of its own is refused.
	Sensitive() func(http.Handler) http.Handler
	// Identity is the identity a gate of this provider verified for the
	// request whose context ctx is; false without one.
	Identity(ctx context.Context) (Identity, bool)
}
