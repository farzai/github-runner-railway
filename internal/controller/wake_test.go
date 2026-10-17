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
	// a job is queued, so the nudged cycle finds work and no retry follows
	gh := &countingGH{fakeGH: fakeGH{jobs: map[string][]ghapp.Job{repo.Key(): {{ID: 1, Repo: "acme/app", CreatedAt: now}}}}}
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

type laggingGH struct {
	fakeGH
	reads    atomic.Int32
	listFrom int32 // the job shows up from this read on
}

func (g *laggingGH) QueuedJobs(_ context.Context, s reconcile.Scope) ([]ghapp.Job, error) {
	if g.reads.Add(1) >= g.listFrom {
		return []ghapp.Job{{ID: 1, Repo: "acme/app", CreatedAt: now}}, nil
	}
	return nil, nil
}

// GitHub may not list a job yet when the debounced cycle reads it. A webhook-driven cycle that found
// nothing queued checks once more after MinGap instead of leaving the job to the next poll.
func TestAWebhookCycleThatFindsNothingChecksOnceMoreAfterTheGap(t *testing.T) {
	gh := &laggingGH{listFrom: 3} // read 1: start-up cycle, read 2: the webhook cycle, read 3: the retry
	rw := &fakeRW{}
	c := New(Config{Scopes: []reconcile.Scope{repo}, Prefix: "rr-", MaxRunners: 1, MaxCreatePerCycle: 1,
		StartupGrace: time.Minute, IdleTTL: time.Minute, MaxAge: time.Hour,
		Interval: time.Hour, MinGap: 200 * time.Millisecond, Debounce: 20 * time.Millisecond}, gh, rw)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	for gh.reads.Load() < 1 {
		time.Sleep(5 * time.Millisecond)
	}
	c.Nudge()
	time.Sleep(time.Second)
	if gh.reads.Load() != 3 {
		t.Fatalf("reads %d, want 3: start-up, the webhook cycle and one retry", gh.reads.Load())
	}
	if c.Status().Services == 0 {
		t.Fatal("the job listed on the retry got no runner")
	}
}
