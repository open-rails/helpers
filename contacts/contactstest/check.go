// Package contactstest checks a host's contacts.Source in the host's own CI.
package contactstest

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/open-rails/helpers/contacts"
)

// Fixtures are what the host's directory holds when Check runs.
type Fixtures struct {
	// Contacts are at least two people the directory holds, exactly as it
	// holds them, each with a distinct ID, an Email, a Name and a Username.
	// Their values should be distinctive: a search for one, or for it without
	// its first and last character, must find it among the first 100.
	Contacts []contacts.Contact
	// Unknown are ids in the directory's own format that it does not hold:
	// never issued, or deleted. Nil skips the case.
	Unknown []string
	// Change changes the email, name and username of the contact it is given
	// in the directory and returns the contact as it now is; the next reads
	// must return the new values. Nil skips the case.
	Change func(contacts.Contact) contacts.Contact
}

const searchLimit = 100

// Check fails t when src returns a contact it was not asked for or does not
// hold, a value other than the directory's current one, an error for an
// unknown id, or a search result that does not contain the query (a pattern
// read as a wildcard), appears twice or exceeds the limit. It also fails when
// src misses a contact it must return, so a check cannot pass vacuously.
func Check(t testing.TB, src contacts.Source, f Fixtures) {
	t.Helper()
	if src == nil || len(f.Contacts) < 2 {
		t.Fatal("contactstest: Check needs a Source and at least two Contacts")
		return
	}
	c := &checker{t: t, ctx: t.Context(), src: src, held: map[string]contacts.Contact{}}
	ids := make([]string, 0, len(f.Contacts))
	for _, want := range f.Contacts {
		if _, dup := c.held[want.ID]; dup || want.ID == "" || !strings.Contains(want.Email, "@") || want.Name == "" || want.Username == "" {
			t.Fatalf("contactstest: fixture %+v needs a distinct ID, an Email, a Name and a Username", want)
			return
		}
		c.held[want.ID] = want
		ids = append(ids, want.ID)
	}
	const malformed = "contactstest: not an id"

	asked := append(append(append([]string{}, ids...), f.Unknown...), malformed, ids[0])
	c.contacts("fixtures, unknown, malformed and repeated ids", asked, ids)
	c.contacts("unknown and malformed ids", append([]string{malformed}, f.Unknown...), nil)
	c.contacts("no ids", nil, nil)

	for _, want := range f.Contacts {
		for _, v := range []string{want.Email, want.Username, want.Name} {
			c.find(strings.ToUpper(v), want)
			c.find(inner(strings.ToLower(v)), want)
		}
	}
	for _, q := range []string{"%", "_", "*", ".*", `\`} {
		c.search(q, searchLimit)
	}
	for _, limit := range []int{1, 2} {
		if got := c.search("@", limit); len(got) != limit {
			t.Errorf("contactstest: SearchContacts(%q, %d) returned %d contacts; at least %d match", "@", limit, len(got), len(f.Contacts))
		}
	}
	for _, q := range []struct {
		query string
		limit int
	}{{"", searchLimit}, {"@", 0}, {"@", -1}} {
		if got := c.search(q.query, q.limit); len(got) != 0 {
			t.Errorf("contactstest: SearchContacts(%q, %d) returned %d contacts, not none", q.query, q.limit, len(got))
		}
	}

	errs := make([]error, 8)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Go(func() {
			if i%2 == 0 {
				_, errs[i] = src.Contacts(c.ctx, ids)
			} else {
				_, errs[i] = src.SearchContacts(c.ctx, "@", 2)
			}
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Errorf("contactstest: a concurrent read failed: %v", err)
		}
	}

	if f.Change != nil {
		old := f.Contacts[len(f.Contacts)-1]
		now := f.Change(old)
		if now.ID != old.ID || now.Email == old.Email || now.Name == old.Name || now.Username == old.Username || !strings.Contains(now.Email, "@") {
			t.Errorf("contactstest: Change returned %+v for %+v; it must keep the ID and change the Email, Name and Username", now, old)
			return
		}
		c.held[now.ID] = now
		c.contacts("a changed contact", []string{now.ID}, []string{now.ID})
		c.find(strings.ToUpper(now.Email), now)
		c.search(old.Email, searchLimit)
		c.search(old.Username, searchLimit)
	}
}

type checker struct {
	t    testing.TB
	ctx  context.Context
	src  contacts.Source
	held map[string]contacts.Contact
}

// contacts asks for ids and requires exactly the held contacts of want.
func (c *checker) contacts(what string, ids, want []string) {
	c.t.Helper()
	got, err := c.src.Contacts(c.ctx, ids)
	if err != nil {
		c.t.Errorf("contactstest: Contacts(%s) failed: %v", what, err)
		return
	}
	for _, id := range want {
		if _, ok := got[id]; !ok {
			c.t.Errorf("contactstest: Contacts(%s) did not return %s", what, id)
		}
	}
	for id, have := range got {
		switch held, ok := c.held[id]; {
		case !ok || !slices.Contains(want, id):
			c.t.Errorf("contactstest: Contacts(%s) returned %q, which it was not asked for or does not hold", what, id)
		case have != held:
			c.t.Errorf("contactstest: Contacts(%s) returned %+v, not the current %+v", what, have, held)
		}
	}
}

// find requires a search for query to return want.
func (c *checker) find(query string, want contacts.Contact) {
	c.t.Helper()
	for _, have := range c.search(query, searchLimit) {
		if have.ID == want.ID {
			return
		}
	}
	c.t.Errorf("contactstest: SearchContacts(%q) did not find %s", query, want.ID)
}

// search requires every result to contain query, once, with its current
// values, and no more results than limit.
func (c *checker) search(query string, limit int) []contacts.Contact {
	c.t.Helper()
	got, err := c.src.SearchContacts(c.ctx, query, limit)
	if err != nil {
		c.t.Errorf("contactstest: SearchContacts(%q, %d) failed: %v", query, limit, err)
		return nil
	}
	if len(got) > max(limit, 0) {
		c.t.Errorf("contactstest: SearchContacts(%q, %d) returned %d contacts", query, limit, len(got))
	}
	seen := map[string]bool{}
	for _, have := range got {
		if seen[have.ID] {
			c.t.Errorf("contactstest: SearchContacts(%q) returned %s twice", query, have.ID)
		}
		seen[have.ID] = true
		if !containsFold(have.Email, query) && !containsFold(have.Username, query) && !containsFold(have.Name, query) {
			c.t.Errorf("contactstest: SearchContacts(%q) returned %+v, which does not contain it", query, have)
		}
		if held, ok := c.held[have.ID]; ok && have != held {
			c.t.Errorf("contactstest: SearchContacts(%q) returned %+v, not the current %+v", query, have, held)
		}
	}
	return got
}

// inner is s without its first and last rune: finding it takes a contains
// match, not a prefix or whole-value one.
func inner(s string) string {
	r := []rune(s)
	if len(r) < 3 {
		return s
	}
	return string(r[1 : len(r)-1])
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}
