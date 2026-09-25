package deps

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGaugeFuncIsExported(t *testing.T) {
	sup := New()
	sup.GaugeFunc("app_peer_jwks_age_seconds", "Seconds since the peer's keys were last fetched.", func() []Sample {
		return []Sample{{Labels: []Label{{"issuer", `https://a.example "x"`}}, Value: 42.5}, {Value: 1}}
	})
	rec := httptest.NewRecorder()
	sup.Metrics(rec, httptest.NewRequest("GET", MetricsPath, nil))
	body := rec.Body.String()
	for _, want := range []string{
		"# TYPE app_peer_jwks_age_seconds gauge",
		`app_peer_jwks_age_seconds{issuer="https://a.example \"x\""} 42.5`,
		"app_peer_jwks_age_seconds 1\n",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics lack %q:\n%s", want, body)
		}
	}
}
