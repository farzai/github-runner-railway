package controller

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/farzai/github-runner-railway/internal/ghapp"
	"github.com/farzai/github-runner-railway/internal/reconcile"
)

type countedGH struct {
	fakeGH
	n int64
}

func (g *countedGH) Calls() int64 { return g.n }
func (g *countedGH) QueuedJobs(ctx context.Context, s reconcile.Scope) ([]ghapp.Job, error) {
	g.n += 3
	return g.fakeGH.QueuedJobs(ctx, s)
}

type countedRW struct {
	fakeRW
	n int64
}

func (r *countedRW) Calls() int64 { return r.n }

// Every cycle logs one summary line: how long it took, the API calls it made to GitHub and
// Railway, and what it created and reaped, so the cycle rate and its cost can be read from logs.
func TestEveryCycleLogsOneSummaryLine(t *testing.T) {
	gh := &countedGH{fakeGH: fakeGH{jobs: map[string][]ghapp.Job{repo.Key(): {{ID: 1, Repo: "acme/app", CreatedAt: now}}}}}
	rw := &countedRW{n: 10}
	var buf bytes.Buffer
	c := newC(&gh.fakeGH, &rw.fakeRW)
	c.gh, c.rw = gh, rw
	c.cfg.Log = slog.New(slog.NewTextHandler(&buf, nil))
	_ = c.Cycle(context.Background())
	out := buf.String()
	if strings.Count(out, "msg=cycle ") != 1 {
		t.Fatalf("log %q: want one cycle line", out)
	}
	for _, want := range []string{"created=1", "reaped=0", "github_calls=6", "railway_calls=0", "duration="} {
		if !strings.Contains(out, want) {
			t.Errorf("cycle line %q lacks %s", out, want)
		}
	}
}

// A reap that finds the runner busy keeps the service: it is not counted as reaped.
func TestAKeptBusyRunnerIsNotCountedAsReaped(t *testing.T) {
	gh := &fakeGH{runners: map[string][]reconcile.Runner{repo.Key(): {{ID: 7, Name: "rr-" + repo.Hash() + "-aaaaaa"}}}, busy: map[int64]bool{7: true}}
	rw := &fakeRW{services: []reconcile.Service{{ID: "s1", Name: "rr-" + repo.Hash() + "-aaaaaa", ScopeHash: repo.Hash(), CreatedAt: now.Add(-time.Hour)}}}
	var buf bytes.Buffer
	c := newC(gh, rw)
	c.cfg.Log = slog.New(slog.NewTextHandler(&buf, nil))
	_ = c.Cycle(context.Background())
	if !strings.Contains(buf.String(), "runner turned busy; kept") {
		t.Fatalf("log %q: the idle surplus runner should have been tried and found busy", buf.String())
	}
	if len(rw.deleted) != 0 || !strings.Contains(buf.String(), "reaped=0") {
		t.Fatalf("deleted %v, log %q: want nothing deleted and reaped=0", rw.deleted, buf.String())
	}
}
