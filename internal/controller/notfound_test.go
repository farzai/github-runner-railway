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
	"github.com/farzai/github-runner-railway/internal/reconcile"
)

// A deleted repository in RUNNER_SCOPES answers 404. It is listed as unreadable and warned about
// once a cycle, but the cycle succeeds: /healthz stays 200 and the other scope still gets its runners.
// The only alert is the one saying the scope has been missing for FailAlertAfter cycles.
func TestAScopeThatIsNotFoundIsWarnedAboutButTheCycleSucceeds(t *testing.T) {
	gone := fmt.Errorf("%w: GET /repos/acme/app/actions/runs: HTTP 404", ghapp.ErrNotFound)
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
	if body["unreadable_scopes"] != float64(1) {
		t.Fatalf("unreadable_scopes %v, want 1", body["unreadable_scopes"])
	}
	if len(rw.created) == 0 {
		t.Fatalf("created %v: the readable scope must get runners", rw.created)
	}
	if len(*alerts) != 1 || !strings.Contains((*alerts)[0], repo.Key()) || !strings.Contains((*alerts)[0], "not found") {
		t.Fatalf("alerts %q, want only the missing-scope alert for %s", *alerts, repo.Key())
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

// The runners listing answering 404 is treated the same as the jobs listing: warned about, not failed.
func TestAScopeWhoseRunnersAreNotFoundDoesNotFailTheCycle(t *testing.T) {
	gh := &fakeGH{runsErr: map[string]error{repo.Key(): fmt.Errorf("%w: GET /repos/acme/app/actions/runners: HTTP 404", ghapp.ErrNotFound)}}
	c, _ := alerting(gh, &fakeRW{})
	var logs bytes.Buffer
	c.cfg.Log = slog.New(slog.NewTextHandler(&logs, nil))
	if err := c.Cycle(context.Background()); err != nil {
		t.Fatalf("cycle: %v, want success", err)
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 {
		t.Fatalf("%d warn lines, want one:\n%s", n, logs.String())
	}
	code, body := health(t, c)
	if code != http.StatusOK || body["error"] != false {
		t.Fatalf("health %d %v, want 200", code, body)
	}
	if body["unreadable_scopes"] != float64(1) {
		t.Fatalf("unreadable_scopes %v after one cycle, want 1", body["unreadable_scopes"])
	}
}

// When an outage fails the cycles, the failing alert names the scopes unreadable because of it, not a
// deleted repository that answers 404 throughout; that one stays listed on /healthz and gets its own
// missing-scope alert.
func TestTheFailingAlertDoesNotBlameANotFoundScope(t *testing.T) {
	gh := &fakeGH{jobsErr: map[string]error{
		org.Key():  errors.New("github down"),
		repo.Key(): fmt.Errorf("%w: GET /repos/acme/app/actions/runs: HTTP 404", ghapp.ErrNotFound),
	}}
	c, alerts := alerting(gh, &fakeRW{})
	for i := 0; i < c.cfg.FailAlertAfter; i++ {
		_ = c.Cycle(context.Background())
	}
	var failing []string
	for _, a := range *alerts {
		if strings.Contains(a, "cycles in a row failed") {
			failing = append(failing, a)
		}
	}
	if len(failing) != 1 || !strings.Contains(failing[0], "scopes unreadable that long: "+org.Key()+";") || strings.Contains(failing[0], repo.Key()) {
		t.Fatalf("alerts %q, want one failing alert naming only %s", *alerts, org.Key())
	}
	_, body := health(t, c)
	if body["unreadable_scopes"] != float64(2) {
		t.Fatalf("unreadable_scopes %v, want both scopes", body["unreadable_scopes"])
	}
}

func notFound(s string) error {
	return fmt.Errorf("%w: GET /repos/%s/actions/runs: HTTP 404", ghapp.ErrNotFound, s)
}

// When every scope answers 404 (the App uninstalled, or its installation gone), nothing at all can
// be served: the cycle fails, /healthz goes 503 and the failing alert fires after FailAlertAfter.
// The scopes are named in the error, and no per-scope missing alert is sent on top of it.
func TestEveryScopeNotFoundFailsTheCycleAndAlerts(t *testing.T) {
	gh := &fakeGH{jobsErr: map[string]error{org.Key(): notFound("acme"), repo.Key(): notFound("acme/app")}}
	c, alerts := alerting(gh, &fakeRW{})
	for i := 0; i < c.cfg.FailAlertAfter; i++ {
		if err := c.Cycle(context.Background()); err == nil {
			t.Fatalf("cycle %d succeeded, want it to fail when no scope is readable", i+1)
		}
	}
	code, body := health(t, c)
	if code != http.StatusServiceUnavailable || body["error"] != true {
		t.Fatalf("health %d %v, want 503 with an error", code, body)
	}
	if len(*alerts) != 1 || !strings.Contains((*alerts)[0], "cycles in a row failed") ||
		!strings.Contains((*alerts)[0], org.Key()) || !strings.Contains((*alerts)[0], repo.Key()) {
		t.Fatalf("alerts %q, want one failing alert naming both scopes", *alerts)
	}
	gh.jobsErr = nil // the App is installed again
	if err := c.Cycle(context.Background()); err != nil {
		t.Fatalf("cycle after the scopes came back: %v, want success", err)
	}
	if len(*alerts) != 2 || !strings.Contains((*alerts)[1], "recovered") {
		t.Fatalf("alerts %q, want the recovered message next", *alerts)
	}
}

// One scope answering 404 among readable ones raises one missing-scope alert after FailAlertAfter
// cycles, never repeats it, and sends one message when the scope reads again.
func TestAMissingScopeIsAlertedOnceAndClearedWhenItReadsAgain(t *testing.T) {
	gh := &fakeGH{jobsErr: map[string]error{repo.Key(): notFound("acme/app")}}
	c, alerts := alerting(gh, &fakeRW{})
	for i := 1; i < c.cfg.FailAlertAfter; i++ {
		_ = c.Cycle(context.Background())
	}
	if len(*alerts) != 0 {
		t.Fatalf("alerts %q after %d cycles, want none yet", *alerts, c.cfg.FailAlertAfter-1)
	}
	for i := 0; i < 3; i++ {
		if err := c.Cycle(context.Background()); err != nil {
			t.Fatalf("cycle: %v, want success", err)
		}
	}
	want := fmt.Sprintf("controller: scope %s not found on GitHub for %d cycles in a row", repo.Key(), c.cfg.FailAlertAfter)
	if len(*alerts) != 1 || !strings.HasPrefix((*alerts)[0], want) {
		t.Fatalf("alerts %q, want one starting %q", *alerts, want)
	}
	delete(gh.jobsErr, repo.Key())
	_ = c.Cycle(context.Background())
	_ = c.Cycle(context.Background())
	if len(*alerts) != 2 || (*alerts)[1] != "controller: scope "+repo.Key()+" is readable again" {
		t.Fatalf("alerts %q, want one readable-again message", *alerts)
	}
	if _, body := health(t, c); body["unreadable_scopes"] != float64(0) {
		t.Fatalf("unreadable_scopes %v, want none", body["unreadable_scopes"])
	}
}

// A pool of one scope that answers 404 is every scope gone: the cycle fails. A pool with no scopes
// has nothing missing and does not fail.
func TestASingleScopePoolThatIsNotFoundFailsAndAnEmptyPoolDoesNot(t *testing.T) {
	gh := &fakeGH{jobsErr: map[string]error{repo.Key(): notFound("acme/app")}}
	c, _ := alerting(gh, &fakeRW{})
	c.cfg.Scopes = []reconcile.Scope{repo}
	if err := c.Cycle(context.Background()); err == nil || !strings.Contains(err.Error(), repo.Key()) {
		t.Fatalf("cycle: %v, want a failure naming %s", err, repo.Key())
	}
	c.cfg.Scopes = nil
	if err := c.Cycle(context.Background()); err != nil {
		t.Fatalf("cycle with no scopes: %v, want success", err)
	}
}
