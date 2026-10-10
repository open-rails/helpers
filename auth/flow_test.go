package auth_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-rails/helpers/auth"
	"github.com/open-rails/helpers/auth/authtest"
)

const issuer = "https://shop.example"

var (
	billing = auth.Scope{Authority: issuer, ID: "billing"}
	support = auth.Scope{Authority: issuer, ID: "support"}
)

// shop is a host's own auth with no AuthKit: bearer sessions and API keys in
// a table, grants per scope, all read live. It imports this package alone.
type shop struct {
	mu       sync.Mutex
	creds    map[string]*cred
	down     bool
	verified atomic.Int64
}

type cred struct {
	identity auth.Identity
	grants   map[auth.Scope][]string
	signedIn time.Time // zero: no sign-in of its own
	revoked  bool
	nonce    bool // DPoP-bound: refused until it presents a nonce
}

func person(id, credential string, signedIn time.Time, grants map[auth.Scope][]string) *cred {
	return &cred{identity: auth.Identity{
		Issuer: issuer, Subject: id, SubjectKind: auth.SubjectUser,
		Invoker:    auth.Invoker{Issuer: issuer, ID: id},
		Credential: auth.Credential{Kind: auth.CredentialSession, ID: credential},
	}, grants: grants, signedIn: signedIn}
}

func newShop() *shop {
	now := time.Now()
	app := &cred{identity: auth.Identity{
		Issuer: issuer, Subject: "app-backend", SubjectKind: auth.SubjectApplication,
		Invoker:    auth.Invoker{Issuer: issuer, ID: "app-backend"},
		Credential: auth.Credential{Kind: auth.CredentialAPIKey, ID: "key-1"},
	}, grants: map[auth.Scope][]string{billing: {"billing:read", "billing:refund"}}}
	return &shop{creds: map[string]*cred{
		"staff":    person("user-1", "s-1", now, map[auth.Scope][]string{billing: {"billing:read", "billing:refund"}}),
		"customer": person("user-2", "s-2", now, nil),
		"reader":   person("user-3", "s-3", now, map[auth.Scope][]string{billing: {"billing:read"}}),
		"stale":    person("user-4", "s-4", now.Add(-time.Hour), map[auth.Scope][]string{billing: {"billing:read", "billing:refund"}}),
		"elsewhere": person("user-5", "s-5", now, map[auth.Scope][]string{
			support: {"billing:read", "billing:refund"},
		}),
		"key-1":   app,
		"revoked": func() *cred { c := person("user-6", "s-6", now, nil); c.revoked = true; return c }(),
		"bound":   func() *cred { c := person("user-7", "s-7", now, nil); c.nonce = true; return c }(),
	}}
}

func (s *shop) Authenticate(r *http.Request) (auth.Verified, error) {
	s.verified.Add(1)
	token := ""
	if fields := strings.Fields(r.Header.Get("Authorization")); len(fields) == 2 {
		token = fields[1]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.creds[token]
	switch {
	case s.down:
		return nil, errors.Join(auth.ErrUnavailable, errors.New("session store unreachable"))
	case c == nil:
		return nil, auth.ErrUnauthenticated
	case c.revoked:
		return nil, errors.Join(auth.ErrUnauthenticated, auth.ErrRevoked)
	case c.nonce && r.Header.Get("DPoP") == "":
		return nil, &auth.Challenge{
			Err:    errors.Join(auth.ErrUnauthenticated, auth.ErrSenderProofRequired),
			Header: http.Header{"Www-Authenticate": {`DPoP error="use_dpop_nonce"`}, "Dpop-Nonce": {"n-1"}},
		}
	}
	return verified{s, c}, nil
}

func (s *shop) KnownPermission(p string) bool {
	return slices.Contains([]string{"billing:read", "billing:refund"}, p)
}

type verified struct {
	shop *shop
	c    *cred
}

func (v verified) Identity() auth.Identity { return v.c.identity }

func (v verified) Can(_ context.Context, scope auth.Scope, permission string) (bool, error) {
	v.shop.mu.Lock()
	defer v.shop.mu.Unlock()
	switch {
	case v.shop.down:
		return false, auth.ErrUnavailable
	case v.c.revoked:
		return false, errors.Join(auth.ErrUnauthenticated, auth.ErrRevoked)
	}
	return slices.Contains(v.c.grants[scope], permission), nil
}

func (v verified) CheckRecentSignIn(context.Context) error {
	switch {
	case v.c.signedIn.IsZero():
		return auth.ErrForbidden
	case time.Since(v.c.signedIn) > 15*time.Minute:
		return &auth.Challenge{Err: auth.ErrStepUpRequired, MaxAge: 15 * time.Minute, Metadata: map[string]any{"methods": []any{"password"}}}
	}
	return nil
}

// identityOnly is a provider with no permission or sign-in checks.
type identityOnly struct{ *shop }

func (p identityOnly) Authenticate(r *http.Request) (auth.Verified, error) {
	v, err := p.shop.Authenticate(r)
	if err != nil {
		return nil, err
	}
	return idOnly(v.Identity()), nil
}

type idOnly auth.Identity

func (i idOnly) Identity() auth.Identity { return auth.Identity(i) }

// route is a consumer's route, as its catalog declares it.
type route struct {
	permission  string // a staff route's
	sensitive   bool   // moves money: a person must have signed in recently
	application bool   // a backend's route: an application, never a person
}

// gate is the consumer's gate the package documents: authenticate once,
// then decide by the route; every refusal is auth.Refuse's.
func gate(a auth.Authenticator, scope auth.Scope, rt route) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v, err := a.Authenticate(r)
		if err != nil {
			refuse(w, r, err)
			return
		}
		id := v.Identity()
		switch {
		case rt.application && id.SubjectKind != auth.SubjectApplication:
			refuse(w, r, auth.ErrForbidden)
			return
		case rt.permission == "" && !rt.application && (id.SubjectKind != auth.SubjectUser || !id.SelfInvoked()):
			refuse(w, r, auth.ErrForbidden)
			return
		}
		if rt.permission != "" {
			pc, ok := v.(auth.PermissionChecker)
			if !ok {
				refuse(w, r, auth.ErrForbidden)
				return
			}
			allowed, err := pc.Can(r.Context(), scope, rt.permission)
			if err == nil && !allowed {
				err = auth.ErrForbidden
			}
			if err != nil {
				refuse(w, r, err)
				return
			}
		}
		if rt.sensitive && id.SubjectKind == auth.SubjectUser {
			rs, ok := v.(auth.RecentSignInChecker)
			err := auth.ErrForbidden
			if ok {
				err = rs.CheckRecentSignIn(r.Context())
			}
			if err != nil {
				refuse(w, r, err)
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func refuse(w http.ResponseWriter, r *http.Request, err error) {
	f := auth.Refuse(r, err)
	for name, values := range f.Header {
		w.Header()[name] = values
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(f.Status)
	_ = json.NewEncoder(w).Encode(map[string]any{"metadata": f.Metadata})
}

func serve(a auth.Authenticator) *httptest.Server {
	mux := http.NewServeMux()
	mux.Handle("GET /me", gate(a, billing, route{}))
	mux.Handle("GET /admin/payments", gate(a, billing, route{permission: "billing:read"}))
	mux.Handle("POST /admin/refunds", gate(a, billing, route{permission: "billing:refund", sensitive: true}))
	mux.Handle("POST /app/usage", gate(a, billing, route{application: true}))
	return httptest.NewServer(mux)
}

type answer struct {
	status    int
	challenge string
	metadata  map[string]any
}

func call(t *testing.T, srv *httptest.Server, method, path, authorization string) answer {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var body struct{ Metadata map[string]any }
	_ = json.NewDecoder(res.Body).Decode(&body)
	return answer{res.StatusCode, res.Header.Get("WWW-Authenticate"), body.Metadata}
}

func TestConsumerGatesOverAHostsOwnAuth(t *testing.T) {
	s := newShop()
	srv := serve(s)
	defer srv.Close()
	for _, c := range []struct {
		name, method, path, credential string
		status                         int
		challenge                      string
	}{
		{"anonymous", "GET", "/me", "", 401, "Bearer"},
		{"unknown token", "GET", "/me", "Bearer forged", 401, `Bearer error="invalid_token"`},
		{"revoked sign-in", "GET", "/me", "Bearer revoked", 401, `Bearer error="invalid_token"`},
		{"DPoP-bound without a proof", "GET", "/me", "DPoP bound", 401, `DPoP error="use_dpop_nonce"`},
		{"a customer, own route", "GET", "/me", "Bearer customer", 204, ""},
		{"an application on a person's route", "GET", "/me", "Bearer key-1", 403, ""},
		{"a customer on a staff route", "GET", "/admin/payments", "Bearer customer", 403, ""},
		{"staff of another scope", "GET", "/admin/payments", "Bearer elsewhere", 403, ""},
		{"staff", "GET", "/admin/payments", "Bearer staff", 204, ""},
		{"a reader refunding", "POST", "/admin/refunds", "Bearer reader", 403, ""},
		{"staff refunding", "POST", "/admin/refunds", "Bearer staff", 204, ""},
		{"stale staff reading", "GET", "/admin/payments", "Bearer stale", 204, ""},
		{"stale staff refunding", "POST", "/admin/refunds", "Bearer stale", 401, `Bearer error="insufficient_user_authentication", max_age="900"`},
		{"an application refunding", "POST", "/admin/refunds", "Bearer key-1", 204, ""},
		{"an application on its route", "POST", "/app/usage", "Bearer key-1", 204, ""},
		{"a person on an application's route", "POST", "/app/usage", "Bearer staff", 403, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := call(t, srv, c.method, c.path, c.credential)
			if got.status != c.status || got.challenge != c.challenge {
				t.Fatalf("got %d %q, want %d %q", got.status, got.challenge, c.status, c.challenge)
			}
		})
	}

	t.Run("the step-up carries the provider's challenge", func(t *testing.T) {
		if got := call(t, srv, "POST", "/admin/refunds", "Bearer stale"); got.metadata["methods"] == nil {
			t.Fatalf("metadata %v", got.metadata)
		}
	})
	t.Run("one verification per request", func(t *testing.T) {
		before := s.verified.Load()
		call(t, srv, "POST", "/admin/refunds", "Bearer staff")
		if n := s.verified.Load() - before; n != 1 {
			t.Fatalf("verified %d times", n)
		}
	})
	t.Run("a grant removed applies to the next request", func(t *testing.T) {
		s.mu.Lock()
		s.creds["reader"].grants = nil
		s.mu.Unlock()
		if got := call(t, srv, "GET", "/admin/payments", "Bearer reader"); got.status != 403 {
			t.Fatalf("%d", got.status)
		}
	})
	t.Run("an outage is 503, never a refusal of the credential", func(t *testing.T) {
		s.mu.Lock()
		s.down = true
		s.mu.Unlock()
		defer func() { s.mu.Lock(); s.down = false; s.mu.Unlock() }()
		if got := call(t, srv, "GET", "/me", "Bearer customer"); got.status != 503 || got.challenge != "" {
			t.Fatalf("%d %q", got.status, got.challenge)
		}
	})
}

// Without Can a provider grants nothing; without CheckRecentSignIn a person
// moves no money.
func TestMissingCapabilitiesFailClosed(t *testing.T) {
	srv := serve(identityOnly{newShop()})
	defer srv.Close()
	for path, want := range map[string]int{"/me": 204, "/admin/payments": 403} {
		if got := call(t, srv, "GET", path, "Bearer staff"); got.status != want {
			t.Errorf("%s: %d, want %d", path, got.status, want)
		}
	}
	if got := call(t, srv, "POST", "/admin/refunds", "Bearer staff"); got.status != 403 {
		t.Errorf("refund without the capabilities: %d", got.status)
	}
}

func TestHostsOwnAuthConforms(t *testing.T) {
	s := newShop()
	req := func(token string) func() *http.Request {
		return func() *http.Request {
			r := httptest.NewRequest(http.MethodGet, "https://billing.example/v1/admin/payments", nil)
			r.Header.Set("Authorization", "Bearer "+token)
			return r
		}
	}
	authtest.Check(t, s, authtest.Cases{
		Scope:       billing,
		Permissions: []string{"billing:read", "billing:refund"},
		Staff:       req("staff"),
		User:        req("customer"),
		Holders:     map[string]func() *http.Request{"billing:read": req("reader")},
		Stale:       req("stale"),
		Application: req("key-1"),
		Refused:     map[string]func() *http.Request{"forged": req("forged"), "revoked": req("revoked")},
		Revoke: func() {
			s.mu.Lock()
			s.creds["staff"].revoked = true
			s.mu.Unlock()
		},
	})
}
