package controller

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/farzai/github-runner-railway/internal/ghapp"
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
	for _, want := range []string{"unlisted=1", "job=310", "repo=acme/app"} {
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
