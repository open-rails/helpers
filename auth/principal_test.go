package auth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-rails/helpers/auth"
)

// An independent consumer owns this interface. Exact shared result types let a
// provider satisfy it without importing that consumer or a particular verifier.
type authenticator interface {
	AuthenticateRequest(context.Context, *http.Request) (auth.Principal, error)
}

type identityOnly auth.Identity

func (p identityOnly) Identity() auth.Identity { return auth.Identity(p) }

// This deliberately small provider stands in for a host's existing verified
// session store. It has no permission API and no dependency on AuthKit.
type sessionProvider struct{ sessions map[string]auth.Identity }

func (p sessionProvider) AuthenticateRequest(_ context.Context, r *http.Request) (auth.Principal, error) {
	i, ok := p.sessions[r.Header.Get("Authorization")]
	if !ok {
		return nil, auth.ErrUnauthenticated
	}
	return identityOnly(i), nil
}

var _ authenticator = sessionProvider{}

func TestIndependentIdentityOnlyProvider(t *testing.T) {
	var provider authenticator = sessionProvider{sessions: map[string]auth.Identity{
		"Bearer known-session": {Kind: auth.KindUser, Issuer: "https://host.example", Subject: "user-7"},
	}}
	r := httptest.NewRequest(http.MethodGet, "https://host.example/account", nil)
	r.Header.Set("Authorization", "Bearer known-session")
	p, err := provider.AuthenticateRequest(r.Context(), r)
	if err != nil || p.Identity().Subject != "user-7" {
		t.Fatalf("session identity: %v, %v", p, err)
	}
	if _, ok := p.(auth.PermissionChecker); ok {
		t.Fatal("identity-only provider unexpectedly grants permission checks")
	}
	r.Header.Set("Authorization", "Bearer unknown-session")
	if _, err := provider.AuthenticateRequest(r.Context(), r); err != auth.ErrUnauthenticated {
		t.Fatalf("unknown session accepted: %v", err)
	}
}

// stepUpPrincipal is a user whose sign-in is too old; its refusal carries the
// provider's challenge for the client.
type stepUpPrincipal struct{ identityOnly }

type challenge map[string]any

func (challenge) Error() string              { return "sign in again" }
func (c challenge) Metadata() map[string]any { return c }

func (stepUpPrincipal) CheckRecentSignIn(context.Context) error {
	return errors.Join(auth.ErrStepUpRequired, challenge{"step_up_methods": []string{"password"}})
}

// A consumer refuses a sensitive action unless the principal proves a recent
// sign-in, and hands the client the provider's challenge.
func requireRecentSignIn(ctx context.Context, p auth.Principal) (map[string]any, error) {
	checker, ok := p.(auth.RecentSignInChecker)
	if !ok {
		return nil, auth.ErrStepUpRequired
	}
	err := checker.CheckRecentSignIn(ctx)
	var m interface{ Metadata() map[string]any }
	if errors.As(err, &m) {
		return m.Metadata(), err
	}
	return nil, err
}

func TestRecentSignInIsOptionalAndFailsClosed(t *testing.T) {
	ctx := context.Background()
	user := identityOnly{Kind: auth.KindUser, Issuer: "https://host.example", Subject: "user-7"}
	if _, err := requireRecentSignIn(ctx, user); !errors.Is(err, auth.ErrStepUpRequired) {
		t.Fatalf("a principal without the capability passed: %v", err)
	}
	metadata, err := requireRecentSignIn(ctx, stepUpPrincipal{user})
	if !errors.Is(err, auth.ErrStepUpRequired) || metadata["step_up_methods"] == nil {
		t.Fatalf("stale sign-in: %v, %v", metadata, err)
	}
}
