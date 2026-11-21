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
// like any other, and the gap stays visible: the cycle line and a warning name it, and /healthz
// counts it in the last cycle and, once per job, since the controller started.
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
	if body["unlisted_jobs"] != float64(1) || body["unlisted_jobs_total"] != float64(1) {
		t.Fatalf("healthz %v, want unlisted_jobs 1 and unlisted_jobs_total 1", body)
	}

	_ = c.Cycle(context.Background()) // still waiting: the same job again is not counted again
	if _, body = health(t, c); body["unlisted_jobs"] != float64(1) || body["unlisted_jobs_total"] != float64(1) {
		t.Fatalf("healthz %v after the same job again, want 1 and 1", body)
	}

	gh.jobs[repo.Key()] = []ghapp.Job{{ID: 2, Repo: "acme/app", CreatedAt: now}, {ID: 311, Repo: "acme/app", CreatedAt: now, Unlisted: true}, {ID: 312, Repo: "acme/app", CreatedAt: now, Unlisted: true}}
	_ = c.Cycle(context.Background())
	if _, body = health(t, c); body["unlisted_jobs"] != float64(2) || body["unlisted_jobs_total"] != float64(3) {
		t.Fatalf("healthz %v after two new unlisted jobs, want 2 and 3", body)
	}

	gh.jobs[repo.Key()] = nil
	_ = c.Cycle(context.Background())
	if _, body = health(t, c); body["unlisted_jobs"] != float64(0) || body["unlisted_jobs_total"] != float64(3) {
		t.Fatalf("healthz %v once served, want 0 and 3", body)
	}
}
