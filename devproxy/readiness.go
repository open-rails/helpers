package devproxy

import (
	"crypto/subtle"
	"net/http"
)

// WithReadinessToken proves that a readiness response came from this candidate.
// A supervisor supplies a random token only to its child. Normal readiness
// requests work unchanged; absent or incorrect tokens are never reflected.
func WithReadinessToken(next http.Handler, token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/readyz" && token != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Devproxy-Ready-Token")), []byte(token)) == 1 {
			w.Header().Set("X-Devproxy-Ready-Token", token)
		}
		next.ServeHTTP(w, r)
	})
}
