package controller

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/farzai/github-runner-railway/internal/ghapp"
	"github.com/farzai/github-runner-railway/internal/reconcile"
)

// The wait after a queued-job webhook: a short debounce so one push's burst of events lands in one
// cycle, but never sooner than MinGap after the previous cycle *started*, which bounds the scan rate.
func TestTheWaitAfterANudgeIsTheDebounceUnlessTheLastCycleStartedTooRecently(t *testing.T) {
	start := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		now  time.Time
		want time.Duration
	}{
		{"long after the last cycle", start.Add(5 * time.Minute), 3 * time.Second},
		{"just after a cycle started", start.Add(2 * time.Second), 18 * time.Second},
		{"the gap almost over", start.Add(19 * time.Second), 3 * time.Second},
		{"no cycle yet", start, 3 * time.Second},
	}
	for _, tc := range cases {
		last := start
		if tc.name == "no cycle yet" {
			last = time.Time{}
		}
		if got := wakeDelay(tc.now, last, 3*time.Second, 20*time.Second); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

type countingGH struct {
	fakeGH
	reads atomic.Int32
}

func (g *countingGH) QueuedJobs(ctx context.Context, s reconcile.Scope) ([]ghapp.Job, error) {
	g.reads.Add(1)
	return g.fakeGH.QueuedJobs(ctx, s)
}

// Ten nudges inside the debounce window are one cycle.
func TestABurstOfNudgesInsideTheDebounceIsOneCycle(t *testing.T) {
	gh := &countingGH{}
	c := New(Config{Scopes: []reconcile.Scope{repo}, Prefix: "rr-", MaxRunners: 1, MaxCreatePerCycle: 1,
		StartupGrace: time.Minute, IdleTTL: time.Minute, MaxAge: time.Hour,
		Interval: time.Hour, MinGap: time.Millisecond, Debounce: 150 * time.Millisecond}, gh, &fakeRW{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	deadline := time.Now().Add(2 * time.Second)
	for gh.reads.Load() < 1 && time.Now().Before(deadline) { // the first cycle at start
		time.Sleep(5 * time.Millisecond)
	}
	for i := 0; i < 10; i++ {
		c.Nudge()
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(600 * time.Millisecond)
	if got := gh.reads.Load(); got != 2 {
		t.Fatalf("cycles %d, want 2: the start-up cycle and one for the burst", got)
	}
}
