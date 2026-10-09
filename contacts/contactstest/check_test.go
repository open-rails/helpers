package contactstest_test

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/open-rails/helpers/contacts"
	"github.com/open-rails/helpers/contacts/contactstest"
)

// directory is an in-memory Source; its flags break it the ways Check must
// catch.
type directory struct {
	mu     sync.RWMutex
	people []contacts.Contact
	frozen []contacts.Contact // a copy taken at construction, read when stale

	stale          bool // reads the copy, not the directory
	everyone       bool // Contacts returns everyone held
	unknownErr     bool // Contacts fails on an id it does not hold
	emptyMatchAll  bool // an empty query matches everyone
	ignoreLimit    bool
	perField       bool                           // a contact matching on two fields appears twice
	match          func(field, query string) bool // nil: contains, ignoring case
	wildcardsMatch bool                           // % and _ are SQL LIKE wildcards
}

func newDirectory() *directory {
	people := []contacts.Contact{
		{ID: "11111111-1111-4111-8111-111111111111", Email: "Ada.Lovelace@example.com", Name: "Ada Lovelace", Username: "ada"},
		{ID: "22222222-2222-4222-8222-222222222222", Email: "grace@example.org", Name: "Grace Hopper", Username: "grace_h"},
		{ID: "33333333-3333-4333-8333-333333333333", Email: "alan@example.net", Name: "Alan Turing", Username: "aturing"},
	}
	return &directory{people: people, frozen: append([]contacts.Contact{}, people...)}
}

func (d *directory) read() []contacts.Contact {
	if d.stale {
		return d.frozen
	}
	return d.people
}

func (d *directory) Contacts(_ context.Context, ids []string) (map[string]contacts.Contact, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := map[string]contacts.Contact{}
	for _, c := range d.read() {
		if d.everyone {
			out[c.ID] = c
		}
	}
	for _, id := range ids {
		found := false
		for _, c := range d.read() {
			if c.ID == id {
				out[id], found = c, true
			}
		}
		if !found && d.unknownErr {
			return nil, context.DeadlineExceeded
		}
	}
	return out, nil
}

func (d *directory) SearchContacts(_ context.Context, query string, limit int) ([]contacts.Contact, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if (query == "" && !d.emptyMatchAll) || (limit < 1 && !d.ignoreLimit) {
		return nil, nil
	}
	match := d.match
	if match == nil {
		match = func(field, query string) bool {
			return strings.Contains(strings.ToLower(field), strings.ToLower(query))
		}
	}
	if d.wildcardsMatch {
		like := regexp.MustCompile(`(?i)` + strings.NewReplacer("%", ".*", "_", ".").Replace(regexp.QuoteMeta(query)))
		match = func(field, _ string) bool { return like.MatchString(field) }
	}
	var out []contacts.Contact
	for _, c := range d.read() {
		for _, field := range []string{c.Email, c.Username, c.Name} {
			if match(field, query) {
				out = append(out, c)
				if !d.perField {
					break
				}
			}
		}
	}
	if !d.ignoreLimit && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// change renames the last person, as a host's account update would.
func (d *directory) change(c contacts.Contact) contacts.Contact {
	d.mu.Lock()
	defer d.mu.Unlock()
	c.Email, c.Name, c.Username = "turing@example.ac.uk", "A. M. Turing", "amt"
	for i := range d.people {
		if d.people[i].ID == c.ID {
			d.people[i] = c
		}
	}
	return c
}

func (d *directory) fixtures() contactstest.Fixtures {
	return contactstest.Fixtures{
		Contacts: append([]contacts.Contact{}, d.people...),
		Unknown:  []string{"44444444-4444-4444-8444-444444444444"},
		Change:   d.change,
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

func TestCheckPassesAConformingSource(t *testing.T) {
	d := newDirectory()
	contactstest.Check(t, d, d.fixtures())
}

func TestCheckCatchesABrokenSource(t *testing.T) {
	lower := func(f func(string, string) bool) func(string, string) bool {
		return func(field, query string) bool { return f(strings.ToLower(field), strings.ToLower(query)) }
	}
	for name, c := range map[string]struct {
		breakIt func(*directory)
		want    string // in the failure Check must report
	}{
		"stale copy":           {func(d *directory) { d.stale = true }, "not the current"},
		"returns everyone":     {func(d *directory) { d.everyone = true }, "not asked for"},
		"fails on unknown ids": {func(d *directory) { d.unknownErr = true }, "Contacts(%s) failed"},
		"empty query matches":  {func(d *directory) { d.emptyMatchAll = true }, "not none"},
		"ignores the limit":    {func(d *directory) { d.ignoreLimit = true }, "at least %d match"},
		"repeats a contact":    {func(d *directory) { d.perField = true }, "twice"},
		"LIKE wildcards":       {func(d *directory) { d.wildcardsMatch = true }, "does not contain it"},
		"case-sensitive":       {func(d *directory) { d.match = strings.Contains }, "did not find"},
		"prefix only":          {func(d *directory) { d.match = lower(strings.HasPrefix) }, "did not find"},
		"email only": {func(d *directory) {
			d.match = func(field, query string) bool {
				return strings.Contains(field, "@") && lower(strings.Contains)(field, query)
			}
		}, "did not find"},
	} {
		t.Run(name, func(t *testing.T) {
			d := newDirectory()
			c.breakIt(d)
			r := &recorder{TB: t}
			contactstest.Check(r, d, d.fixtures())
			if !slices.ContainsFunc(r.failures, func(f string) bool { return strings.Contains(f, c.want) }) {
				t.Fatalf("Check reported %q, not %q", r.failures, c.want)
			}
		})
	}
}

func TestCheckRefusesTooFewFixtures(t *testing.T) {
	d := newDirectory()
	f := d.fixtures()
	f.Contacts = f.Contacts[:1]
	r := &recorder{TB: t}
	contactstest.Check(r, d, f)
	if len(r.failures) != 1 {
		t.Fatalf("one fixture drew %v", r.failures)
	}
}
