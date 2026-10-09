package auth_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-rails/helpers/auth"
)

// An independent consumer owns this interface. Exact shared result types let a
// provider satisfy it without importing that consumer or a particular verifier.
type authenticator interface {
	AuthenticateRequest(context.Context, *http.Request) (auth.Verified, error)
}

type identityOnly auth.Identity

func (p identityOnly) Identity() auth.Identity { return auth.Identity(p) }

// This deliberately small provider stands in for a host's existing verified
// session store. It has no permission API and no dependency on AuthKit.
type sessionProvider struct{ sessions map[string]auth.Identity }

func (p sessionProvider) AuthenticateRequest(_ context.Context, r *http.Request) (auth.Verified, error) {
	i, ok := p.sessions[r.Header.Get("Authorization")]
	if !ok {
		return nil, auth.ErrUnauthenticated
	}
	return identityOnly(i), nil
}

var _ authenticator = sessionProvider{}

func TestIndependentIdentityOnlyProvider(t *testing.T) {
	var provider authenticator = sessionProvider{sessions: map[string]auth.Identity{
		"Bearer known-session": {Issuer: "https://host.example", Subject: "user-7", SubjectKind: auth.SubjectUser, Invoker: auth.Invoker{Issuer: "https://host.example", ID: "user-7"}, Credential: auth.Credential{Kind: auth.CredentialSession, ID: "s-1"}},
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

// staleSignIn is a user whose sign-in is too old; its refusal carries the
// provider's challenge for the client.
type staleSignIn struct{ identityOnly }

type challenge map[string]any

func (challenge) Error() string              { return "sign in again" }
func (c challenge) Metadata() map[string]any { return c }

func (staleSignIn) CheckRecentSignIn(context.Context) error {
	return errors.Join(auth.ErrStepUpRequired, challenge{"step_up_methods": []string{"password"}})
}

// A consumer refuses a sensitive action unless the request proves a recent
// sign-in, and hands the client the provider's challenge.
func requireRecentSignIn(ctx context.Context, p auth.Verified) (map[string]any, error) {
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
	user := identityOnly{Issuer: "https://host.example", Subject: "user-7", SubjectKind: auth.SubjectUser, Invoker: auth.Invoker{Issuer: "https://host.example", ID: "user-7"}, Credential: auth.Credential{Kind: auth.CredentialSession, ID: "s-1"}}
	if _, err := requireRecentSignIn(ctx, user); !errors.Is(err, auth.ErrStepUpRequired) {
		t.Fatalf("a request without the capability passed: %v", err)
	}
	metadata, err := requireRecentSignIn(ctx, staleSignIn{user})
	if !errors.Is(err, auth.ErrStepUpRequired) || metadata["step_up_methods"] == nil {
		t.Fatalf("stale sign-in: %v, %v", metadata, err)
	}
}

// A provider's credential state is opaque: encoding drops it, so an Identity
// rebuilt from data carries none, and only the provider reads it.
func TestCredentialStateIsNotEncoded(t *testing.T) {
	type bounds struct{ ceiling []string }
	state := &bounds{ceiling: []string{"merchant:payments:read"}}
	id := auth.Identity{Issuer: "https://host.example", Subject: "user-7", SubjectKind: auth.SubjectUser,
		Invoker: auth.Invoker{Issuer: "https://host.example", ID: "user-7"}, Credential: auth.Credential{Kind: auth.CredentialSession, ID: "s-1"}.WithState(state)}
	if id.Credential.State() != state {
		t.Fatal("the provider's state is lost")
	}
	b, err := json.Marshal(id)
	if err != nil {
		t.Fatal(err)
	}
	var decoded auth.Identity
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Credential.State() != nil || decoded.Subject != id.Subject || decoded.Credential.ID != "s-1" {
		t.Fatalf("decoded = %+v", decoded)
	}
}
