package deps

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGaugeFuncEscapesLabelValues(t *testing.T) {
	sup := New()
	if err := sup.GaugeFunc("app_peer_jwks_age_seconds", "Age.\nSecond line \\ here.", func() []Sample {
		return []Sample{
			{Labels: []Label{{"issuer", "https://a.example/\"x\"\\y\nz ü"}}, Value: 1},
			{Labels: []Label{{"bad-label", "v"}}, Value: 2}, // invalid label name: dropped
		}
	}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	sup.Metrics(rec, httptest.NewRequest("GET", MetricsPath, nil))
	body := rec.Body.String()
	for _, want := range []string{
		`# HELP app_peer_jwks_age_seconds Age.\nSecond line \\ here.` + "\n",
		`app_peer_jwks_age_seconds{issuer="https://a.example/\"x\"\\y\nz ü"} 1` + "\n",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics lack %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "bad-label") {
		t.Fatalf("a sample with an invalid label name was exported:\n%s", body)
	}
}

func TestGaugeFuncValidatesNames(t *testing.T) {
	sup := New()
	sup.Counter("app_things_total", "Things.")
	for _, name := range []string{"", "1bad", "bad-name", "app_ready", "app_dependency_up", "app_things_total"} {
		if err := sup.GaugeFunc(name, "x", func() []Sample { return nil }); err == nil {
			t.Fatalf("GaugeFunc(%q) accepted", name)
		}
	}
	if err := sup.GaugeFunc("app_ok", "x", func() []Sample { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := sup.GaugeFunc("app_ok", "x", func() []Sample { return nil }); err == nil {
		t.Fatal("duplicate gauge accepted")
	}
}
