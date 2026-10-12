package deps

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/helpers/cache"
)

func TestCounterFuncExportsCacheStats(t *testing.T) {
	c, err := cache.New(cache.Options{Name: "deps_test_products", RefreshAfter: time.Minute, MaxStale: time.Hour},
		func(_ context.Context, keys []string) (map[string]int, error) { return map[string]int{keys[0]: 1}, nil })
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Get(context.Background(), []string{"a"})
	c.Get(context.Background(), []string{"a"})

	sup := New()
	if err := sup.CounterFunc("app_cache_events_total", "Cache events by cache name.", func() []Sample {
		var out []Sample
		for _, s := range cache.Snapshot() {
			for event, n := range map[string]uint64{"hit": s.Hits, "miss": s.Misses, "stale": s.StaleServes, "load": s.Loads, "load_failure": s.LoadFailures} {
				out = append(out, Sample{Labels: []Label{{"cache", s.Name}, {"event", event}}, Value: float64(n)})
			}
		}
		return out
	}); err != nil {
		t.Fatal(err)
	}
	if err := sup.GaugeFunc("app_cache_events_total", "x", func() []Sample { return nil }); err == nil {
		t.Fatal("gauge reused a counter's name")
	}
	rec := httptest.NewRecorder()
	sup.Metrics(rec, httptest.NewRequest("GET", MetricsPath, nil))
	body := rec.Body.String()
	for _, want := range []string{
		"# TYPE app_cache_events_total counter\n",
		`app_cache_events_total{cache="deps_test_products",event="hit"} 1` + "\n",
		`app_cache_events_total{cache="deps_test_products",event="miss"} 1` + "\n",
		`app_cache_events_total{cache="deps_test_products",event="load"} 1` + "\n",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics lack %q:\n%s", want, body)
		}
	}
}
