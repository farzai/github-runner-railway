package controller

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/farzai/github-runner-railway/internal/ghapp"
	"github.com/farzai/github-runner-railway/internal/reconcile"
)

// A job GitHub's status filter left out (found only through the unfiltered runs list) gets a runner
// like any other, and the gap stays visible: the cycle line counts it, a warning names it, and
// /healthz counts the last cycle's.
func TestAnUnlistedJobGetsARunnerAndIsCountedOnHealthzAndInTheLog(t *testing.T) {
	gh := &fakeGH{jobs: map[string][]ghapp.Job{
		repo.Key(): {{ID: 310, Repo: "acme/app", CreatedAt: now, Unlisted: true}, {ID: 2, Repo: "acme/app", CreatedAt: now}},
	}}
	rw := &fakeRW{}
	var buf bytes.Buffer
	c := newC(gh, rw)
	c.cfg.Log = slog.New(slog.NewTextHandler(&buf, nil))
	if err := c.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(rw.created) != 2 {
		t.Fatalf("created %v, want a runner for each job, the unlisted one included", rw.created)
	}
	out := buf.String()
	for _, want := range []string{"unlisted=1", "job=310", "repo=acme/app", "failed=false"} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q lacks %s", out, want)
		}
	}
	_, body := health(t, c)
	if body["unlisted_jobs"] != float64(1) {
		t.Fatalf("healthz %v, want unlisted_jobs 1", body)
	}

	gh.jobs[repo.Key()] = nil
	_ = c.Cycle(context.Background())
	if _, body = health(t, c); body["unlisted_jobs"] != float64(0) {
		t.Fatalf("healthz %v once served, want 0", body)
	}
}

type failingListRW struct{ fakeRW }

func (*failingListRW) List(context.Context) ([]reconcile.Service, error) {
	return nil, errors.New("railway down")
}

// A cycle that fails on Railway still reports the unlisted jobs it found on GitHub.
func TestACycleThatFailsOnRailwayStillCountsUnlistedJobs(t *testing.T) {
	gh := &fakeGH{jobs: map[string][]ghapp.Job{repo.Key(): {{ID: 310, Repo: "acme/app", CreatedAt: now, Unlisted: true}}}}
	rw := &failingListRW{}
	var buf bytes.Buffer
	c := newC(gh, &rw.fakeRW)
	c.rw = rw
	c.cfg.Log = slog.New(slog.NewTextHandler(&buf, nil))
	if err := c.Cycle(context.Background()); err == nil {
		t.Fatal("want the Railway failure reported")
	}
	if !strings.Contains(buf.String(), "failed=true") || !strings.Contains(buf.String(), "unlisted=1") {
		t.Errorf("log %q: want the cycle line to say it failed and count the unlisted job", buf.String())
	}
	_, body := health(t, c)
	if body["unlisted_jobs"] != float64(1) || body["error"] != true || body["services"] != float64(0) || body["queued"] != float64(0) {
		t.Fatalf("healthz %v, want unlisted_jobs 1, the error, and no services or queue claimed from a failed read", body)
	}
}

// A failed fallback read (the unfiltered run list answering 500 or 403) does not fail the scope: the
// jobs the filtered lists found still get their runners, each failure is logged at warn with its
// repository and status, and the cycle line and /healthz count them. The next clean cycle clears it.
func TestAFailedFallbackReadKeepsTheListedJobsAndIsCounted(t *testing.T) {
	gh := &fakeGH{
		jobs: map[string][]ghapp.Job{repo.Key(): {{ID: 2, Repo: "acme/app", CreatedAt: now}}},
		jobsErr: map[string]error{repo.Key(): &ghapp.UnlistedReadError{Failed: []ghapp.UnlistedReadFailure{
			{Repo: "acme/app", Status: 500}, {Repo: "acme/lib", Status: 403}}}},
	}
	rw := &fakeRW{}
	var buf bytes.Buffer
	c := newC(gh, rw)
	c.cfg.Log = slog.New(slog.NewTextHandler(&buf, nil))
	if err := c.Cycle(context.Background()); err != nil {
		t.Fatalf("err %v, want a failed fallback read not to fail the cycle", err)
	}
	if len(rw.created) != 1 {
		t.Fatalf("created %v, want a runner for the listed job", rw.created)
	}
	out := buf.String()
	for _, want := range []string{"unlisted_read_failed=2", "failed=false", "repo=acme/app status=500", "repo=acme/lib status=403"} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q lacks %s", out, want)
		}
	}
	code, body := health(t, c)
	if code != 200 || body["unlisted_read_failures"] != float64(2) || body["unreadable_scopes"] != float64(0) || body["consecutive_failures"] != float64(0) {
		t.Fatalf("healthz %d %v, want 200, unlisted_read_failures 2 and the scope readable", code, body)
	}

	gh.jobsErr = nil
	_ = c.Cycle(context.Background())
	if _, body = health(t, c); body["unlisted_read_failures"] != float64(0) {
		t.Fatalf("healthz %v after a clean cycle, want 0", body)
	}
}

// /healthz counts the services left after the cycle: a reaped one is gone.
func TestHealthzServicesLeaveOutAReapedService(t *testing.T) {
	name := "rr-" + repo.Hash() + "-aaaaaa"
	gh := &fakeGH{runners: map[string][]reconcile.Runner{repo.Key(): {{ID: 7, Name: name}}}}
	rw := &fakeRW{services: []reconcile.Service{{ID: "s1", Name: name, ScopeHash: repo.Hash(), CreatedAt: now.Add(-time.Hour)}}}
	c := newC(gh, rw)
	if err := c.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, body := health(t, c); len(rw.deleted) != 1 || body["services"] != float64(0) {
		t.Fatalf("deleted %v healthz %v, want the idle service reaped and services 0", rw.deleted, body)
	}
}
