// Package authtest checks a host's auth.Authenticator in the host's own CI,
// against the contract the libraries that take it enforce.
package authtest

import (
	"errors"
	"net/http"
	"slices"
	"sort"
	"sync"
	"testing"

	"github.com/open-rails/helpers/auth"
)

// Cases are credentials the host's auth holds when Check runs. Each is a
// function returning a fresh request, since a sender proof is spent once.
type Cases struct {
	// Scope is where Permissions are held: the scope the host gives the
	// library (OpenRails' Routes.Scope).
	Scope auth.Scope
	// Permissions are the permissions the host gives the library, at least
	// one.
	Permissions []string
	// Staff is a person signed in recently, holding every one of
	// Permissions in Scope.
	Staff func() *http.Request
	// User is another person, signed in, holding none of them in Scope.
	User func() *http.Request
	// Holders are people each holding only the permission they are keyed
	// by. Nil skips the case.
	Holders map[string]func() *http.Request
	// Stale is a person holding every one of Permissions whose sign-in is
	// too old for an action that moves money. Nil skips the case.
	Stale func() *http.Request
	// Application is an application's credential (client credentials, an
	// API key) holding every one of Permissions in Scope. Nil skips the
	// case.
	Application func() *http.Request
	// Refused are credentials Authenticate must refuse, by name: an expired
	// token, a forged one, a signed-out session, a banned or deleted
	// account's.
	Refused map[string]func() *http.Request
	// Revoke ends the sign-in behind Staff's credential, as signing out
	// does. Check calls it last; Authenticate must then refuse Staff. Nil
	// skips the case.
	Revoke func()
}

const unknownPermission = "authtest:unknown:permission"

// Check fails t when a admits what it must refuse: an anonymous request or
// a Refused credential, a permission to someone who does not hold it, in a
// scope it was not granted in, or for a pattern; a stale sign-in as recent;
// an application as a person or as signed in. It fails when a refuses Staff,
// User, a Holder or Application, so a check cannot pass vacuously, and when
// an error classifies as anything but the contract says (auth.Refuse), so
// every consumer answers it the same.
func Check(t testing.TB, a auth.Authenticator, c Cases) {
	t.Helper()
	if a == nil || c.Scope.Authority == "" || c.Scope.ID == "" || len(c.Permissions) == 0 || c.Staff == nil || c.User == nil {
		t.Fatal("authtest: Check needs an Authenticator, a Scope, Permissions, Staff and User")
		return
	}
	held := map[string]bool{}
	for _, p := range c.Permissions {
		if p == "" || held[p] {
			t.Fatalf("authtest: Permissions %q has an empty or repeated permission", c.Permissions)
			return
		}
		held[p] = true
	}
	ch := checker{t: t, a: a, scope: c.Scope, permission: c.Permissions[0]}

	ch.refused("an anonymous request", func() *http.Request {
		r := c.Staff()
		r.Header.Del("Authorization")
		r.Header.Del("Cookie")
		r.Header.Del("DPoP")
		return r
	}, http.StatusUnauthorized)
	for _, name := range sortedKeys(c.Refused) {
		ch.refused("the "+name+" credential", c.Refused[name], http.StatusUnauthorized, http.StatusForbidden)
	}

	staff, ok := ch.person("Staff", c.Staff)
	if ok {
		ch.holds("Staff", staff, c.Permissions)
		ch.notGranted("Staff", staff)
		if rs, ok := staff.(auth.RecentSignInChecker); !ok {
			t.Errorf("authtest: Staff's Verified has no CheckRecentSignIn: a person must prove a recent sign-in to move money")
		} else if err := rs.CheckRecentSignIn(t.Context()); err != nil {
			t.Errorf("authtest: CheckRecentSignIn refused Staff, signed in recently: %v", err)
		}
	}
	if user, ok := ch.person("User", c.User); ok {
		if staff != nil && user.Identity().Subject == staff.Identity().Subject {
			t.Errorf("authtest: User and Staff are the same subject %q", staff.Identity().Subject)
		}
		ch.lacks("User", user, c.Permissions)
	}
	for _, own := range sortedKeys(c.Holders) {
		if !held[own] {
			t.Errorf("authtest: Holders names %s, which Permissions does not", own)
			continue
		}
		if v, ok := ch.person("the holder of "+own, c.Holders[own]); ok {
			ch.holds("the holder of "+own, v, []string{own})
			ch.lacks("the holder of "+own, v, slices.DeleteFunc(slices.Clone(c.Permissions), func(p string) bool { return p == own }))
		}
	}
	if c.Stale != nil {
		if v, ok := ch.person("Stale", c.Stale); ok {
			ch.holds("Stale", v, c.Permissions)
			ch.stale(v, c.Stale)
		}
	}
	if c.Application != nil {
		if v, ok := ch.admitted("Application", c.Application); ok {
			if id := v.Identity(); id.SubjectKind != auth.SubjectApplication {
				t.Errorf("authtest: Application's Identity is SubjectKind %q; an application is never a person", id.SubjectKind)
			}
			ch.holds("Application", v, c.Permissions)
			ch.notGranted("Application", v)
			if rs, ok := v.(auth.RecentSignInChecker); ok {
				if err := rs.CheckRecentSignIn(t.Context()); !errors.Is(err, auth.ErrForbidden) {
					t.Errorf("authtest: CheckRecentSignIn answered %v for Application; a credential with no sign-in of its own is auth.ErrForbidden", err)
				}
			}
		}
	}
	if catalog, ok := a.(auth.PermissionCatalog); ok {
		for _, p := range c.Permissions {
			if !catalog.KnownPermission(p) {
				t.Errorf("authtest: KnownPermission(%q) is false for a permission Staff holds", p)
			}
		}
		if catalog.KnownPermission(unknownPermission) {
			t.Errorf("authtest: KnownPermission(%q) is true", unknownPermission)
		}
	}

	errs := make([]error, 8)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Go(func() {
			req := c.Staff
			if i%2 == 1 {
				req = c.User
			}
			_, errs[i] = a.Authenticate(req())
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Errorf("authtest: a concurrent Authenticate failed: %v", err)
			break
		}
	}

	if c.Revoke != nil && staff != nil {
		c.Revoke()
		ch.refused("Staff after Revoke", c.Staff, http.StatusUnauthorized)
	}
}

type checker struct {
	t     testing.TB
	a     auth.Authenticator
	scope auth.Scope
	// permission is one every holder of all of them holds in scope.
	permission string
}

// admitted requires Authenticate to admit req, with a whole identity.
func (c checker) admitted(name string, req func() *http.Request) (auth.Verified, bool) {
	c.t.Helper()
	v, err := c.a.Authenticate(req())
	switch {
	case err != nil:
		c.t.Errorf("authtest: Authenticate refused %s: %v", name, err)
		return nil, false
	case v == nil:
		c.t.Errorf("authtest: Authenticate admitted %s with no Verified", name)
		return nil, false
	}
	id := v.Identity()
	switch {
	case id.Issuer == "" || id.Subject == "" || id.Credential.Kind == "":
		c.t.Errorf("authtest: %s's Identity names no Issuer, Subject or Credential kind: %+v", name, id)
	case id.SubjectKind != auth.SubjectUser && id.SubjectKind != auth.SubjectApplication:
		c.t.Errorf("authtest: %s's Identity is SubjectKind %q, neither a user nor an application", name, id.SubjectKind)
	case id.Invoker.Issuer == "" || id.Invoker.ID == "":
		c.t.Errorf("authtest: %s's Identity names no Invoker; a subject acting itself is its own", name)
	default:
		return v, true
	}
	return nil, false
}

// person requires a person acting itself.
func (c checker) person(name string, req func() *http.Request) (auth.Verified, bool) {
	c.t.Helper()
	v, ok := c.admitted(name, req)
	if !ok {
		return nil, false
	}
	if id := v.Identity(); id.SubjectKind != auth.SubjectUser || !id.SelfInvoked() {
		c.t.Errorf("authtest: %s's Identity is SubjectKind %q, invoked by %+v; a person signed in is a user acting itself", name, id.SubjectKind, id.Invoker)
		return nil, false
	}
	return v, true
}

// refused requires Authenticate to refuse req, as one of statuses.
func (c checker) refused(name string, req func() *http.Request, statuses ...int) {
	c.t.Helper()
	r := req()
	v, err := c.a.Authenticate(r)
	switch status := auth.Refuse(r, err).Status; {
	case err == nil:
		c.t.Errorf("authtest: Authenticate admitted %s", name)
	case v != nil:
		c.t.Errorf("authtest: Authenticate refused %s but returned a Verified", name)
	case !slices.Contains(statuses, status):
		c.t.Errorf("authtest: Authenticate refused %s with %v, which consumers answer %d, not %v", name, err, status, statuses)
	}
}

func (c checker) can(v auth.Verified, scope auth.Scope, permission string) (bool, error) {
	pc, ok := v.(auth.PermissionChecker)
	if !ok {
		return false, nil
	}
	return pc.Can(c.t.Context(), scope, permission)
}

// holds requires v to hold each of permissions in the scope.
func (c checker) holds(name string, v auth.Verified, permissions []string) {
	c.t.Helper()
	if _, ok := v.(auth.PermissionChecker); !ok {
		c.t.Errorf("authtest: %s's Verified has no Can", name)
		return
	}
	for _, p := range permissions {
		if ok, err := c.can(v, c.scope, p); !ok || err != nil {
			c.t.Errorf("authtest: Can(%+v, %q) refused %s: %v", c.scope, p, name, err)
		}
	}
}

// lacks requires v to hold none of permissions in the scope.
func (c checker) lacks(name string, v auth.Verified, permissions []string) {
	c.t.Helper()
	for _, p := range permissions {
		if ok, _ := c.can(v, c.scope, p); ok {
			c.t.Errorf("authtest: Can(%+v, %q) granted %s, who does not hold it", c.scope, p, name)
		}
	}
}

// notGranted requires v, holding the permissions in the scope, to hold
// nothing outside it: another scope, another authority's scope of the same
// id, no scope, a pattern, an unknown permission.
func (c checker) notGranted(name string, v auth.Verified) {
	c.t.Helper()
	p := c.permission
	for _, probe := range []struct {
		scope      auth.Scope
		permission string
	}{
		{auth.Scope{Authority: c.scope.Authority, ID: "authtest-other-" + c.scope.ID}, p},
		{auth.Scope{Authority: "https://authtest.invalid", ID: c.scope.ID}, p},
		{auth.Scope{}, p},
		{c.scope, ""},
		{c.scope, "*"},
		{c.scope, p + "*"},
		{c.scope, unknownPermission},
	} {
		if ok, _ := c.can(v, probe.scope, probe.permission); ok {
			c.t.Errorf("authtest: Can(%+v, %q) granted %s: a grant is exactly one permission in exactly one scope", probe.scope, probe.permission, name)
		}
	}
}

// stale requires CheckRecentSignIn to ask v's person to sign in again, as
// a step-up every consumer answers 401.
func (c checker) stale(v auth.Verified, req func() *http.Request) {
	c.t.Helper()
	rs, ok := v.(auth.RecentSignInChecker)
	if !ok {
		c.t.Errorf("authtest: Stale's Verified has no CheckRecentSignIn")
		return
	}
	err := rs.CheckRecentSignIn(c.t.Context())
	if !errors.Is(err, auth.ErrStepUpRequired) {
		c.t.Errorf("authtest: CheckRecentSignIn answered %v for Stale, not auth.ErrStepUpRequired", err)
		return
	}
	if refusal := auth.Refuse(req(), err); refusal.Status != http.StatusUnauthorized {
		c.t.Errorf("authtest: Stale's step-up %v answers %d, not 401", err, refusal.Status)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
