package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/farzai/github-runner-railway/internal/ghapp"
	"github.com/farzai/github-runner-railway/internal/reconcile"
)

func alerting(gh *fakeGH, rw *fakeRW) (*Controller, *[]string) {
	var alerts []string
	c := New(Config{
		Scopes: []reconcile.Scope{org, repo}, Prefix: "rr-", MaxRunners: 10, MaxCreatePerCycle: 5,
		StartupGrace: 3 * time.Minute, IdleTTL: 2 * time.Minute, MaxAge: 6 * time.Hour, AlertAfter: 5 * time.Minute,
		Interval: time.Minute, FailAlertAfter: 3, Alert: func(m string) { alerts = append(alerts, m) },
	}, gh, rw)
	c.now = func() time.Time { return now }
	return c, &alerts
}

func health(t *testing.T, c *Controller) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	c.HealthHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	return rec.Code, body
}

// A controller whose every cycle fails used to answer 200 forever, because a failed cycle still
// stamped LastCycle: after FailAlertAfter failures in a row it alerts once and answers 503, and the
// first good cycle sends one recovery message and answers 200 again.
func TestEveryCycleFailingAlertsOnceAnd503sThenRecovers(t *testing.T) {
	boom := errors.New("github down")
	gh := &fakeGH{jobsErr: map[string]error{org.Key(): boom, repo.Key(): boom}}
	c, alerts := alerting(gh, &fakeRW{})
	for i := 1; i <= 5; i++ {
		_ = c.Cycle(context.Background())
		code, body := health(t, c)
		switch {
		case i < 3 && code != http.StatusOK:
			t.Fatalf("cycle %d: %d, want 200 before the threshold", i, code)
		case i >= 3 && code != http.StatusServiceUnavailable:
			t.Fatalf("cycle %d: %d, want 503 at and after the threshold", i, code)
		}
		if body["consecutive_failures"] != float64(i) {
			t.Fatalf("cycle %d: consecutive_failures %v", i, body["consecutive_failures"])
		}
	}
	if len(*alerts) != 1 || !strings.Contains((*alerts)[0], "3 cycles") {
		t.Fatalf("alerts %q, want exactly one after the third failure", *alerts)
	}
	gh.jobsErr = nil
	_ = c.Cycle(context.Background())
	code, body := health(t, c)
	if code != http.StatusOK || body["consecutive_failures"] != float64(0) || body["last_success"] == nil {
		t.Fatalf("after recovery: %d %v", code, body)
	}
	if len(*alerts) != 2 || !strings.Contains((*alerts)[1], "recovered") {
		t.Fatalf("alerts %q, want one recovery message", *alerts)
	}
	_ = c.Cycle(context.Background())
	if len(*alerts) != 2 {
		t.Fatalf("alerts %q: a healthy cycle must not alert", *alerts)
	}
}

// When one scope fails every cycle and the other works, the alert names the failing scope and the
// working one still gets its runners.
func TestOneUnreadableScopeIsNamedAndTheOtherKeepsWorking(t *testing.T) {
	gh := &fakeGH{jobsErr: map[string]error{org.Key(): errors.New("403 rate limited")}}
	rw := &fakeRW{}
	c, alerts := alerting(gh, rw)
	for i := 0; i < 4; i++ {
		_ = c.Cycle(context.Background())
	}
	if len(*alerts) != 1 || !strings.Contains((*alerts)[0], org.Key()) || strings.Contains((*alerts)[0], repo.Key()) {
		t.Fatalf("alerts %q, want one naming only %s", *alerts, org.Key())
	}
	_, body := health(t, c)
	if got, _ := body["unreadable_scopes"].([]any); len(got) != 1 || got[0] != org.Key() {
		t.Fatalf("unreadable_scopes %v", body["unreadable_scopes"])
	}
	gh.jobs = map[string][]ghapp.Job{repo.Key(): {{ID: 9, Repo: "acme/app", CreatedAt: now}}}
	_ = c.Cycle(context.Background())
	if len(rw.created) != 1 {
		t.Fatalf("created %v: the readable scope must keep getting runners", rw.created)
	}
}

// Alerts are sent after the cycle releases its lock: a slow alert webhook must not stall /healthz,
// which reads the status under the same lock.
func TestAlertsAreSentWithoutHoldingTheLock(t *testing.T) {
	boom := errors.New("github down")
	gh := &fakeGH{jobsErr: map[string]error{org.Key(): boom, repo.Key(): boom}}
	c, _ := alerting(gh, &fakeRW{})
	sent := make(chan struct{}, 4)
	c.cfg.Alert = func(string) { _ = c.Status(); sent <- struct{}{} }
	done := make(chan struct{})
	go func() {
		for i := 0; i < 3; i++ {
			_ = c.Cycle(context.Background())
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the cycle deadlocked: the alert ran while the status lock was held")
	}
	if len(sent) != 1 {
		t.Fatalf("alerts %d, want 1", len(sent))
	}
}
