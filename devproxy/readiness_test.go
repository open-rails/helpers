package devproxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReadinessTokenIsCandidateSpecific(t *testing.T) {
	for _, tc := range []struct{ path, configured, sent, want string }{
		{"/readyz", "candidate-a", "candidate-a", "candidate-a"},
		{"/readyz", "candidate-a", "candidate-b", ""},
		{"/readyz", "candidate-a", "", ""},
		{"/readyz", "", "candidate-a", ""},
		{"/account", "candidate-a", "candidate-a", ""},
	} {
		r := httptest.NewRequest(http.MethodGet, tc.path, nil)
		r.Header.Set("X-Devproxy-Ready-Token", tc.sent)
		w := httptest.NewRecorder()
		WithReadinessToken(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }), tc.configured).ServeHTTP(w, r)
		if got := w.Header().Get("X-Devproxy-Ready-Token"); got != tc.want || w.Code != http.StatusServiceUnavailable {
			t.Fatalf("path=%s sent=%q: header=%q status=%d", tc.path, tc.sent, got, w.Code)
		}
	}
}
