package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/farzai/github-runner-railway/internal/ghapp"
)

// A deleted repository in RUNNER_SCOPES answers 404. It is listed as unreadable and warned about
// once a cycle, but the cycle succeeds: /healthz stays 200 and the other scope still gets its runners.
func TestAScopeThatIsNotFoundIsWarnedAboutButTheCycleSucceeds(t *testing.T) {
	gone := fmt.Errorf("ghapp: GET /repos/acme/app/actions/runs: HTTP 404: %w", ghapp.ErrNotFound)
	gh := &fakeGH{
		jobsErr: map[string]error{repo.Key(): gone},
		jobs:    map[string][]ghapp.Job{org.Key(): {{ID: 9, Repo: "acme/web", CreatedAt: now}}},
	}
	rw := &fakeRW{}
	c, alerts := alerting(gh, rw)
	var logs bytes.Buffer
	c.cfg.Log = slog.New(slog.NewTextHandler(&logs, nil))
	cycles := c.cfg.FailAlertAfter + 1
	for i := 0; i < cycles; i++ {
		if err := c.Cycle(context.Background()); err != nil {
			t.Fatalf("cycle %d: %v, want success", i+1, err)
		}
	}
	code, body := health(t, c)
	if code != http.StatusOK || body["consecutive_failures"] != float64(0) || body["error"] != false {
		t.Fatalf("health %d %v, want 200 with no failures", code, body)
	}
	if got, _ := body["unreadable_scopes"].([]any); len(got) != 1 || got[0] != repo.Key() {
		t.Fatalf("unreadable_scopes %v, want [%s]", body["unreadable_scopes"], repo.Key())
	}
	if len(rw.created) == 0 {
		t.Fatalf("created %v: the readable scope must get runners", rw.created)
	}
	if len(*alerts) != 0 {
		t.Fatalf("alerts %q, want none", *alerts)
	}
	warns := 0
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Contains(l, "level=WARN") && strings.Contains(l, repo.Key()) {
			warns++
		}
	}
	if warns != cycles {
		t.Fatalf("%d warn lines naming %s in %d cycles, want one a cycle:\n%s", warns, repo.Key(), cycles, logs.String())
	}
}

// Only a 404 is spared: any other read error still fails the cycle.
func TestAReadErrorOtherThanNotFoundStillFailsTheCycle(t *testing.T) {
	gh := &fakeGH{jobsErr: map[string]error{repo.Key(): errors.New("ghapp: GET /repos/acme/app/actions/runs: HTTP 502")}}
	c, _ := alerting(gh, &fakeRW{})
	if err := c.Cycle(context.Background()); err == nil {
		t.Fatal("cycle succeeded, want it to fail on a 502")
	}
	if _, body := health(t, c); body["consecutive_failures"] != float64(1) || body["error"] != true {
		t.Fatalf("health %v, want one failure", body)
	}
}
