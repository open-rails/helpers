package auth_test

import (
	"context"
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
