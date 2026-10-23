package river

import (
	"testing"
	"time"

	riverqueue "github.com/riverqueue/river"
)

func TestPeriodicIntervalStartupDeadline(t *testing.T) {
	now := time.Now()
	p := &periodicInterval{every: 4 * time.Hour, last: now.Add(-time.Hour), startup: true,
		constructor: func() (riverqueue.JobArgs, *riverqueue.InsertOpts) { return firstArgs{}, nil }}
	if got := p.Next(now); !got.Equal(p.last.Add(p.every)) {
		t.Fatalf("restart moved the existing deadline: %s", got)
	}
	if args, _ := p.construct(); args != nil {
		t.Fatal("recent job ran on startup")
	}
	// River may call a regular constructor up to 100ms before the deadline.
	// It sets scheduled_at itself; only the startup call is suppressed.
	if args, _ := p.construct(); args == nil {
		t.Fatal("regular enqueue was suppressed")
	}
	p.last, p.startup = now.Add(-5*time.Hour), true
	if args, _ := p.construct(); args == nil {
		t.Fatal("overdue job did not catch up")
	}
	if next := p.Next(time.Now()); next.Before(now.Add(4 * time.Hour)) {
		t.Fatal("catchup generated an extra missed-period run")
	}
}

func TestPeriodicCheckpointRetention(t *testing.T) {
	for _, existing := range []time.Duration{0, time.Minute, -1, 9 * 24 * time.Hour} {
		cfg := &riverqueue.Config{Schema: "public", CompletedJobRetentionPeriod: existing, DiscardedJobRetentionPeriod: existing}
		h := &periodicCheckpoint{jobs: []*periodicInterval{{every: 8 * 24 * time.Hour}}}
		if err := h.bind(nil, cfg); err != nil {
			t.Fatal(err)
		}
		for _, got := range []time.Duration{cfg.CompletedJobRetentionPeriod, cfg.DiscardedJobRetentionPeriod} {
			if got != -1 && got < 8*24*time.Hour {
				t.Fatalf("checkpoint can expire before its interval: %s", got)
			}
		}
	}
}
