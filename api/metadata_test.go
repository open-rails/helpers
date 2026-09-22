package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/open-rails/helpers/api"
)

// Every entry here is a real leak shape: a constraint name, a provider body, a
// stack frame, a token. The point of the table is that refusing them is code,
// not a review habit — a writer cannot opt out of SanitizeMetadata.
func TestMetadataRefusesInternalDetail(t *testing.T) {
	api.RegisterMetadataKey("provider_response", "detail", "stack", "token", "operator_email", "callback_url", "note")
	cases := map[string]any{
		"detail":            `pq: duplicate key value violates unique constraint "users_email_key"`,
		"provider_response": `{"nmi":{"responsetext":"DECLINE","authcode":""}}`,
		"stack":             "goroutine 42 [running]:\nmain.handle(0x0)\n\t/src/api/handler.go:88 +0x1f",
		"token":             "Bearer eyJhbGciOiJIUzI1NiJ9.e30.sig",
		"operator_email":    "ops@doujins.example",
		"callback_url":      "https://user:pw@internal.svc/hook",
	}
	for k, v := range cases {
		t.Run(k, func(t *testing.T) {
			got := api.SanitizeMetadata(map[string]any{k: v})
			if got != nil {
				t.Fatalf("%s survived sanitization: %#v", k, got)
			}
			if why := api.MetadataRejections(map[string]any{k: v}); why[k] == "" {
				t.Fatalf("%s was dropped without a reason", k)
			}
		})
	}
}

func TestMetadataAllowlist(t *testing.T) {
	in := map[string]any{
		"retry_after":  30,
		"limit":        100,
		"remaining":    0,
		"undocumented": "anything",
	}
	got := api.SanitizeMetadata(in)
	if _, ok := got["undocumented"]; ok {
		t.Error("an unregistered key must not reach the wire")
	}
	for _, k := range []string{"retry_after", "limit", "remaining"} {
		if _, ok := got[k]; !ok {
			t.Errorf("allowlisted key %q was dropped", k)
		}
	}
	api.RegisterMetadataKey("undocumented")
	if got := api.SanitizeMetadata(in); got["undocumented"] != "anything" {
		t.Error("a registered key must survive")
	}
}

// Doujins' client spreads metadata straight onto the top-level error object it
// hands the UI, so a metadata key named like an envelope field would shadow it.
// Registration refuses those names outright.
func TestMetadataReservedKeysCannotBeRegistered(t *testing.T) {
	reserved := []string{"object", "error", "message", "code", "type", "param", "request_id", "status", "data", "body"}
	api.RegisterMetadataKey(reserved...)
	allowed := strings.Join(api.AllowedMetadataKeys(), ",")
	for _, k := range reserved {
		if strings.Contains(","+allowed+",", ","+k+",") {
			t.Errorf("reserved key %q was allowlisted", k)
		}
		if got := api.SanitizeMetadata(map[string]any{k: "x"}); got != nil {
			t.Errorf("reserved key %q reached the wire", k)
		}
	}
}

func TestMetadataBounds(t *testing.T) {
	api.RegisterMetadataKey("note")
	t.Run("nested values are refused", func(t *testing.T) {
		api.RegisterMetadataKey("nested")
		if got := api.SanitizeMetadata(map[string]any{"nested": map[string]any{"inner": 1}}); got != nil {
			t.Errorf("a nested map reached the wire: %#v", got)
		}
	})
	t.Run("long strings are refused", func(t *testing.T) {
		if got := api.SanitizeMetadata(map[string]any{"note": strings.Repeat("a", 500)}); got != nil {
			t.Errorf("an oversized string reached the wire: %#v", got)
		}
	})
	t.Run("the whole map is bounded", func(t *testing.T) {
		in := map[string]any{}
		for _, k := range api.AllowedMetadataKeys() {
			in[k] = strings.Repeat("b", 200)
		}
		got := api.SanitizeMetadata(in)
		b, _ := json.Marshal(got)
		if len(b) > 1024 {
			t.Errorf("metadata is %d bytes, over the budget: %s", len(b), b)
		}
		if len(got) > 8 {
			t.Errorf("metadata carries %d keys, over the budget", len(got))
		}
	})
}

// Sanitization is on the path to the wire, not a helper a writer may forget.
func TestSanitizationCannotBeBypassed(t *testing.T) {
	api.RegisterMetadataKey("detail")
	leak := map[string]any{"detail": `pq: violates unique constraint "x"`, "retry_after": 5}

	env := api.NewEnvelope(api.ErrorObject{Type: api.TypeAPI, Message: "x", Metadata: leak})
	if _, ok := env.Error.Metadata["detail"]; ok {
		t.Error("NewEnvelope let a leak through")
	}
	e := api.E(http.StatusConflict, api.CodeResourceConflict, "x").WithMetadata(leak)
	if _, ok := e.Metadata["detail"]; ok {
		t.Error("WithMetadata let a leak through")
	}
	_, viaErr := api.EnvelopeFor(e)
	b, _ := json.Marshal(viaErr)
	if strings.Contains(string(b), "constraint") {
		t.Errorf("a constraint name reached the wire: %s", b)
	}
	if !strings.Contains(string(b), `"retry_after":5`) {
		t.Errorf("the safe key was lost with the unsafe one: %s", b)
	}
}
