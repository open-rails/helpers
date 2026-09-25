package deps

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBackoffIsCappedFullJitter(t *testing.T) {
	for attempt := 0; attempt < 64; attempt++ {
		ceil := min(30*time.Second, (500*time.Millisecond)<<min(attempt, 20))
		for range 50 {
			if d := Backoff(attempt, 500*time.Millisecond, 30*time.Second); d < 0 || d > ceil {
				t.Fatalf("attempt %d: %v outside [0,%v]", attempt, d, ceil)
			}
		}
	}
}

func TestNilDependencyIsDownAndSwitchUsesFallback(t *testing.T) {
	var d *Dependency
	if d.Up() || d.Report(context.DeadlineExceeded) {
		t.Fatal("nil dependency must read as down and ignore reports")
	}
	sw := NewSwitch[string](nil, "primary", func() string { return "fallback" })
	if b, primary := sw.Get(); primary || b != "fallback" {
		t.Fatalf("got %q primary=%v", b, primary)
	}
}

func TestApplicationErrorsDoNotMarkDown(t *testing.T) {
	sup := New(WithTiming(fastTiming))
	dep := sup.Add("pg", Required, func(context.Context) error { return nil }, nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sup.Start(ctx)
	eventually(t, "up", dep.Up)
	if dep.Report(errors.New("duplicate key")) || !dep.Up() {
		t.Fatal("application error marked dependency down")
	}
}
