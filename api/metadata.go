package api

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

// Metadata is a PUBLIC field. Doujins' HTTP client spreads it straight onto
// the top-level error object it hands the UI (compat/golden/parsers.json), so
// anything put here is both shown to end users and able to shadow a top-level
// field. It is therefore an allowlist, not a free map: a key survives only if
// it is registered, its name is not reserved, its value is a scalar, and the
// value carries no sign of an internal detail. Everything else is dropped —
// silently on the wire, because a metadata mistake must never turn into a
// second failure, and loudly in tests via MetadataRejections.
//
// The enforcement lives here rather than in a review rule: NewEnvelope runs it
// on every path to the wire, so no writer can opt out.

const (
	maxMetadataKeys      = 8
	maxMetadataKeyLen    = 64
	maxMetadataStringLen = 200
	maxMetadataBytes     = 1024
)

// reservedMetadataKeys would collide with an envelope field or with a field
// Doujins' client sets on the flattened object it hands the UI.
var reservedMetadataKeys = map[string]bool{
	"body": true, "code": true, "data": true, "error": true, "message": true,
	"metadata": true, "name": true, "object": true, "param": true,
	"request_id": true, "status": true, "type": true,
}

// The built-in allowlist: retry and quota hints, and the safe shape of a
// refusal. A domain adds its own with RegisterMetadataKey.
var defaultMetadataKeys = []string{
	"allowed_types", "field", "limit", "max_bytes", "moderation_reason",
	"remaining", "reset_at", "resource", "retry_after", "retry_after_seconds",
}

var (
	metadataMu   sync.RWMutex
	metadataKeys = func() map[string]bool {
		m := make(map[string]bool, len(defaultMetadataKeys))
		for _, k := range defaultMetadataKeys {
			m[k] = true
		}
		return m
	}()
)

// RegisterMetadataKey allowlists a metadata key for this process. A reserved
// name is refused: it would shadow an envelope field downstream. Call it from
// a package initializer, next to the codes the domain defines.
func RegisterMetadataKey(keys ...string) {
	metadataMu.Lock()
	defer metadataMu.Unlock()
	for _, k := range keys {
		if k == "" || reservedMetadataKeys[k] || len(k) > maxMetadataKeyLen || !safeKey(k) {
			continue
		}
		metadataKeys[k] = true
	}
}

// AllowedMetadataKeys lists every currently allowlisted key, sorted.
func AllowedMetadataKeys() []string {
	metadataMu.RLock()
	defer metadataMu.RUnlock()
	out := make([]string, 0, len(metadataKeys))
	for k := range metadataKeys {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// MetadataRejections reports, per key, why SanitizeMetadata would drop it. It
// exists so a test can assert a leak is refused instead of hoping it was.
func MetadataRejections(m map[string]any) map[string]string {
	out := map[string]string{}
	_ = sanitize(m, out)
	return out
}

// SanitizeMetadata returns the subset of m that may go on the wire, or nil.
func SanitizeMetadata(m map[string]any) map[string]any {
	return sanitize(m, nil)
}

func sanitize(m map[string]any, why map[string]string) map[string]any {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make(map[string]any, len(keys))
	for _, k := range keys {
		if reason := rejectKey(k); reason != "" {
			note(why, k, reason)
			continue
		}
		if len(out) >= maxMetadataKeys {
			note(why, k, "over the key budget")
			continue
		}
		v, reason := safeValue(m[k])
		if reason != "" {
			note(why, k, reason)
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	// A run of allowlisted scalars can still add up; the whole map is bounded.
	for {
		encoded, err := json.Marshal(out)
		if err != nil {
			return nil
		}
		if len(encoded) <= maxMetadataBytes {
			break
		}
		last := ""
		for _, k := range keys {
			if _, ok := out[k]; ok {
				last = k
			}
		}
		if last == "" {
			return nil
		}
		delete(out, last)
		note(why, last, "over the total size budget")
		if len(out) == 0 {
			return nil
		}
	}
	return out
}

func note(why map[string]string, k, reason string) {
	if why != nil {
		why[k] = reason
	}
}

func rejectKey(k string) string {
	switch {
	case k == "":
		return "empty key"
	case len(k) > maxMetadataKeyLen:
		return "key too long"
	case reservedMetadataKeys[k]:
		return "reserved key: would shadow an envelope field downstream"
	case !safeKey(k):
		return "key is not lower_snake_case"
	}
	metadataMu.RLock()
	allowed := metadataKeys[k]
	metadataMu.RUnlock()
	if !allowed {
		return "key is not allowlisted (RegisterMetadataKey)"
	}
	return ""
}

func safeKey(k string) bool {
	if k == "" || k[0] < 'a' || k[0] > 'z' {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' {
			continue
		}
		return false
	}
	return true
}

// safeValue admits scalars only. A nested map or slice is how an internal
// struct gets onto the wire by accident, so there is no recursion here.
func safeValue(v any) (any, string) {
	switch t := v.(type) {
	case nil:
		return nil, "nil value"
	case bool:
		return t, ""
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return t, ""
	case json.Number:
		return t, ""
	case string:
		if !utf8.ValidString(t) {
			return nil, "value is not valid UTF-8"
		}
		if utf8.RuneCountInString(t) > maxMetadataStringLen {
			return nil, "string value too long"
		}
		if reason := looksInternal(t); reason != "" {
			return nil, reason
		}
		return t, ""
	default:
		return nil, "value is not a scalar"
	}
}

// internalMarkers are the shapes a leaked internal actually takes in this
// fleet: driver text, constraint names, Go stack frames, bearer/JWT material
// and provider payloads. Each one is a real thing that has reached a log line
// here; the list is meant to grow when a new one does.
var internalMarkers = []struct {
	needle string
	reason string
}{
	{"pq:", "postgres driver text"},
	{"sqlstate", "postgres driver text"},
	{"constraint \"", "sql constraint name"},
	{"violates", "sql constraint text"},
	{"duplicate key value", "sql constraint text"},
	{"goroutine ", "stack trace"},
	{".go:", "stack frame"},
	{"panic:", "stack trace"},
	{"bearer ", "credential"},
	{"eyj", "jwt"},
	{"authorization:", "credential"},
	{"password", "credential"},
	{"secret", "credential"},
	{"api_key", "credential"},
	{"apikey", "credential"},
	{"private_key", "credential"},
	{"://", "url — may carry credentials or an internal host"},
	{"@", "email address or credential"},
}

func looksInternal(s string) string {
	trimmed := strings.TrimSpace(s)
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		return "value looks like an internal detail (serialized payload)"
	}
	if strings.ContainsAny(s, "\n\r\t") {
		return "value looks like an internal detail (multi-line log or trace)"
	}
	lower := strings.ToLower(s)
	for _, m := range internalMarkers {
		if strings.Contains(lower, m.needle) {
			return "value looks like an internal detail (" + m.reason + ")"
		}
	}
	return ""
}
