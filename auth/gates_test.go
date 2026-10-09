package auth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-rails/helpers/auth"
)

// A provider satisfies Auth with this package's types alone: it imports
// neither the library that mounts it nor any verifier.
type tableAuth struct {
	identities map[string]auth.Identity
	grants     map[string]string // subject -> permission
}

var _ auth.Auth = tableAuth{}

type identityKey struct{}

func (a tableAuth) gate(admit func(auth.Identity) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, ok := a.identities[r.Header.Get("Authorization")]
			switch {
			case !ok:
				w.WriteHeader(http.StatusUnauthorized)
			case !admit(id):
				w.WriteHeader(http.StatusForbidden)
			default:
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey{}, id)))
			}
		})
	}
}

func (a tableAuth) Required() func(http.Handler) http.Handler {
	return a.gate(func(id auth.Identity) bool { return id.SubjectKind == auth.SubjectUser && id.SelfInvoked() })
}

func (a tableAuth) RequirePermission(permission string) func(http.Handler) http.Handler {
	return a.gate(func(id auth.Identity) bool { return a.grants[id.Subject] == permission })
}

func (a tableAuth) Sensitive() func(http.Handler) http.Handler {
	return a.gate(func(auth.Identity) bool { return false })
}

func (tableAuth) Identity(ctx context.Context) (auth.Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(auth.Identity)
	return id, ok
}

func TestIndependentAuthProvider(t *testing.T) {
	const host = "https://host.example"
	ci := auth.Identity{Issuer: host, Subject: "app-ci", SubjectKind: auth.SubjectApplication, Invoker: auth.Invoker{Issuer: host, ID: "app-ci"}}
	rotated := ci
	ci.Credential = auth.Credential{Kind: auth.CredentialAPIKey, ID: "key-1"}
	rotated.Credential = auth.Credential{Kind: auth.CredentialAPIKey, ID: "key-2"}
	var a auth.Auth = tableAuth{
		identities: map[string]auth.Identity{
			"Bearer phone":  {Issuer: host, Subject: "user-7", SubjectKind: auth.SubjectUser, Invoker: auth.Invoker{Issuer: host, ID: "user-7"}, Credential: auth.Credential{Kind: auth.CredentialDeviceKey, ID: "dk-1"}},
			"Bearer key-1":  ci,
			"Bearer key-2":  rotated,
			"Bearer viaapp": {Issuer: host, Subject: "app-ci", SubjectKind: auth.SubjectApplication, Invoker: auth.Invoker{Issuer: "https://app.example", ID: "u_42"}, Credential: auth.Credential{Kind: auth.CredentialSignedToken}},
		},
		grants: map[string]string{"app-ci": "merchant:payments:refund"},
	}
	var seen auth.Identity
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen, _ = a.Identity(r.Context()) })
	serve := func(gate func(http.Handler) http.Handler, credential string) int {
		r := httptest.NewRequest(http.MethodPost, "https://billing.example/v1/merchant/payments/p/refunds", nil)
		r.Header.Set("Authorization", credential)
		w := httptest.NewRecorder()
		gate(handler).ServeHTTP(w, r)
		return w.Code
	}
	if code := serve(a.Required(), "Bearer phone"); code != http.StatusOK || seen.Subject != "user-7" || !seen.SelfInvoked() {
		t.Fatalf("a device key's person: %d, %+v", code, seen)
	}
	for _, credential := range []string{"Bearer key-1", "Bearer viaapp"} {
		if code := serve(a.Required(), credential); code != http.StatusForbidden {
			t.Fatalf("%s passed Required: %d", credential, code)
		}
	}
	for _, credential := range []string{"Bearer key-1", "Bearer key-2"} {
		if code := serve(a.RequirePermission("merchant:payments:refund"), credential); code != http.StatusOK || seen.Subject != "app-ci" {
			t.Fatalf("%s: rotating a key changed the subject: %d, %+v", credential, code, seen)
		}
	}
	if code := serve(a.RequirePermission("merchant:payments:refund"), "Bearer anonymous"); code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", code)
	}
	if code := serve(a.Sensitive(), "Bearer key-1"); code != http.StatusForbidden {
		t.Fatalf("an application passed Sensitive: %d", code)
	}
	if _, ok := a.Identity(context.Background()); ok {
		t.Fatal("an identity outside any gate")
	}
}
