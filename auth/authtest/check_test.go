package authtest_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-rails/helpers/auth"
	"github.com/open-rails/helpers/auth/authtest"
)

const issuer = "https://host.example"

var scope = auth.Scope{Authority: issuer, ID: "merchant-1"}

var perms = []string{"billing:read", "billing:refund"}

// host is an in-memory Authenticator; its flags break it the ways Check
// must catch.
type host struct {
	mu    sync.Mutex
	creds map[string]*cred

	anonymousAdmitted bool // no credential reads as the customer
	forgedAdmitted    bool // an unknown token reads as the customer
	unclassified      bool // refusals are bare errors
	verifiedWithError bool // a refusal also returns a Verified
	scopeIgnored      bool // Can ignores the scope
	authorityIgnored  bool // Can ignores the scope's authority
	patterns          bool // Can reads "*" as a wildcard
	everyoneHolds     bool // Can grants every person everything
	noCan             bool // Verified has no Can
	noRecentSignIn    bool // Verified has no CheckRecentSignIn
	staleAccepted     bool // every sign-in is recent
	staleForbidden    bool // a stale sign-in is ErrForbidden, not a step-up
	appAsUser         bool // the application reads as a person
	appSignedIn       bool // the application passes CheckRecentSignIn
	noInvoker         bool // Identity names no invoker
	notLive           bool // a revoked sign-in is still admitted
	knowsEverything   bool // KnownPermission is always true
}

type cred struct {
	id       auth.Identity
	grants   []string
	signedIn time.Time
	revoked  bool
}

func newHost() *host {
	user := func(id string, signedIn time.Time, grants ...string) *cred {
		return &cred{id: auth.Identity{
			Issuer: issuer, Subject: id, SubjectKind: auth.SubjectUser,
			Invoker:    auth.Invoker{Issuer: issuer, ID: id},
			Credential: auth.Credential{Kind: auth.CredentialSession, ID: "s-" + id},
		}, grants: grants, signedIn: signedIn}
	}
	now := time.Now()
	return &host{creds: map[string]*cred{
		"staff":    user("u-staff", now, perms...),
		"customer": user("u-customer", now),
		"reader":   user("u-reader", now, "billing:read"),
		"stale":    user("u-stale", now.Add(-time.Hour), perms...),
		"key": {id: auth.Identity{
			Issuer: issuer, Subject: "app-1", SubjectKind: auth.SubjectApplication,
			Invoker:    auth.Invoker{Issuer: issuer, ID: "app-1"},
			Credential: auth.Credential{Kind: auth.CredentialAPIKey, ID: "key-1"},
		}, grants: perms},
	}}
}

func (h *host) refusal(err error) error {
	if h.unclassified {
		return errors.New("no such session")
	}
	return err
}

func (h *host) Authenticate(r *http.Request) (auth.Verified, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	c, ok := h.creds[token]
	switch {
	case token == "" && h.anonymousAdmitted, !ok && token != "" && h.forgedAdmitted:
		c = h.creds["customer"]
	case !ok:
		if h.verifiedWithError {
			return h.verified(h.creds["customer"]), h.refusal(auth.ErrUnauthenticated)
		}
		return nil, h.refusal(auth.ErrUnauthenticated)
	case c.revoked && !h.notLive:
		return nil, h.refusal(errors.Join(auth.ErrUnauthenticated, auth.ErrRevoked))
	}
	return h.verified(c), nil
}

func (h *host) verified(c *cred) auth.Verified {
	id := c.id
	if h.appAsUser && id.SubjectKind == auth.SubjectApplication {
		id.SubjectKind = auth.SubjectUser
	}
	if h.noInvoker {
		id.Invoker = auth.Invoker{}
	}
	base := verified{h: h, c: c, id: id}
	switch {
	case h.noCan:
		return identityOnly{base}
	case h.noRecentSignIn:
		return canOnly{base}
	}
	return base
}

func (h *host) KnownPermission(p string) bool { return h.knowsEverything || slices.Contains(perms, p) }

type verified struct {
	h  *host
	c  *cred
	id auth.Identity
}

func (v verified) Identity() auth.Identity { return v.id }

func (v verified) Can(_ context.Context, s auth.Scope, p string) (bool, error) {
	h := v.h
	switch {
	case h.everyoneHolds && v.c.id.SubjectKind == auth.SubjectUser:
		return true, nil
	case h.patterns && strings.HasSuffix(p, "*"):
		return slices.ContainsFunc(v.c.grants, func(g string) bool { return strings.HasPrefix(g, strings.TrimSuffix(p, "*")) }), nil
	case h.scopeIgnored, h.authorityIgnored && s.ID == scope.ID, s == scope:
		return slices.Contains(v.c.grants, p), nil
	}
	return false, nil
}

func (v verified) CheckRecentSignIn(context.Context) error {
	h := v.h
	switch {
	case v.c.id.SubjectKind == auth.SubjectApplication && !h.appSignedIn:
		return auth.ErrForbidden
	case h.staleAccepted, time.Since(v.c.signedIn) < 15*time.Minute:
		return nil
	case h.staleForbidden:
		return auth.ErrForbidden
	}
	return &auth.Challenge{Err: auth.ErrStepUpRequired, MaxAge: 15 * time.Minute}
}

type canOnly struct{ v verified }

func (c canOnly) Identity() auth.Identity { return c.v.Identity() }
func (c canOnly) Can(ctx context.Context, s auth.Scope, p string) (bool, error) {
	return c.v.Can(ctx, s, p)
}

type identityOnly struct{ v verified }

func (i identityOnly) Identity() auth.Identity { return i.v.Identity() }

func (h *host) cases() authtest.Cases {
	req := func(token string) func() *http.Request {
		return func() *http.Request {
			r := httptest.NewRequest(http.MethodGet, "https://billing.example/v1/admin/payments", nil)
			r.Header.Set("Authorization", "Bearer "+token)
			return r
		}
	}
	return authtest.Cases{
		Scope:       scope,
		Permissions: perms,
		Staff:       req("staff"),
		User:        req("customer"),
		Holders:     map[string]func() *http.Request{"billing:read": req("reader")},
		Stale:       req("stale"),
		Application: req("key"),
		Refused:     map[string]func() *http.Request{"forged": req("forged")},
		Revoke: func() {
			h.mu.Lock()
			h.creds["staff"].revoked = true
			h.mu.Unlock()
		},
	}
}

// recorder captures Check's failures instead of failing the test.
type recorder struct {
	testing.TB
	failures []string
}

func (r *recorder) Errorf(format string, args ...any) { r.failures = append(r.failures, format) }
func (r *recorder) Fatal(args ...any)                 { r.failures = append(r.failures, "fatal") }
func (r *recorder) Fatalf(format string, args ...any) { r.failures = append(r.failures, format) }
func (r *recorder) Helper()                           {}

func TestCheckPassesAConformingAuthenticator(t *testing.T) {
	h := newHost()
	authtest.Check(t, h, h.cases())
}

func TestCheckCatchesABrokenAuthenticator(t *testing.T) {
	for name, c := range map[string]struct {
		breakIt func(*host)
		want    string // in the failure Check must report
	}{
		"admits anonymous":          {func(h *host) { h.anonymousAdmitted = true }, "admitted %s"},
		"admits a forged token":     {func(h *host) { h.forgedAdmitted = true }, "admitted %s"},
		"unclassified refusals":     {func(h *host) { h.unclassified = true }, "which consumers answer"},
		"a Verified with an error":  {func(h *host) { h.verifiedWithError = true }, "returned a Verified"},
		"ignores the scope":         {func(h *host) { h.scopeIgnored = true }, "exactly one scope"},
		"ignores the authority":     {func(h *host) { h.authorityIgnored = true }, "exactly one scope"},
		"grants patterns":           {func(h *host) { h.patterns = true }, "exactly one permission"},
		"every person holds all":    {func(h *host) { h.everyoneHolds = true }, "who does not hold it"},
		"no Can":                    {func(h *host) { h.noCan = true }, "has no Can"},
		"no recent sign-in check":   {func(h *host) { h.noRecentSignIn = true }, "has no CheckRecentSignIn"},
		"a stale sign-in is recent": {func(h *host) { h.staleAccepted = true }, "not auth.ErrStepUpRequired"},
		"a stale sign-in refused":   {func(h *host) { h.staleForbidden = true }, "not auth.ErrStepUpRequired"},
		"an application as person":  {func(h *host) { h.appAsUser = true }, "never a person"},
		"an application signed in":  {func(h *host) { h.appSignedIn = true }, "no sign-in of its own"},
		"no invoker":                {func(h *host) { h.noInvoker = true }, "names no Invoker"},
		"not live":                  {func(h *host) { h.notLive = true }, "admitted %s"},
		"knows every permission":    {func(h *host) { h.knowsEverything = true }, "KnownPermission(%q) is true"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHost()
			c.breakIt(h)
			r := &recorder{TB: t}
			authtest.Check(r, h, h.cases())
			if !slices.ContainsFunc(r.failures, func(f string) bool { return strings.Contains(f, c.want) }) {
				t.Fatalf("Check reported %q, not %q", r.failures, c.want)
			}
		})
	}
}

func TestCheckRefusesIncompleteCases(t *testing.T) {
	h := newHost()
	for name, edit := range map[string]func(*authtest.Cases){
		"no scope":              func(c *authtest.Cases) { c.Scope = auth.Scope{} },
		"half a scope":          func(c *authtest.Cases) { c.Scope.Authority = "" },
		"no permissions":        func(c *authtest.Cases) { c.Permissions = nil },
		"a repeated permission": func(c *authtest.Cases) { c.Permissions = []string{"billing:read", "billing:read"} },
		"no staff":              func(c *authtest.Cases) { c.Staff = nil },
		"no user":               func(c *authtest.Cases) { c.User = nil },
	} {
		t.Run(name, func(t *testing.T) {
			c := h.cases()
			edit(&c)
			r := &recorder{TB: t}
			authtest.Check(r, h, c)
			if len(r.failures) != 1 {
				t.Fatalf("drew %v", r.failures)
			}
		})
	}
}
