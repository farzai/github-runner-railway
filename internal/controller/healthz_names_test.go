package controller

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// /healthz is public, so it must not name a scope it cannot read: a private repository's name would
// leak to anyone who calls it. It reports how many scopes are unreadable; the names stay in the
// controller's logs and in the failing alert, which only the operator sees.
func TestHealthzCountsUnreadableScopesWithoutNamingThem(t *testing.T) {
	gh := &fakeGH{jobsErr: map[string]error{
		repo.Key(): notFound("acme/app"),
		org.Key():  errors.New("403 rate limited"),
	}}
	c, alerts := alerting(gh, &fakeRW{})
	var logs bytes.Buffer
	c.cfg.Log = slog.New(slog.NewTextHandler(&logs, nil))
	for i := 0; i < c.cfg.FailAlertAfter; i++ {
		_ = c.Cycle(context.Background())
	}

	rec := httptest.NewRecorder()
	c.HealthHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if raw := rec.Body.String(); strings.Contains(raw, "acme") {
		t.Fatalf("/healthz body %s names a scope, want only the count", raw)
	}
	if _, body := health(t, c); body["unreadable_scopes"] != float64(2) {
		t.Fatalf("unreadable_scopes %v, want 2", body["unreadable_scopes"])
	}

	if !strings.Contains(logs.String(), "scope not found on GitHub; skipped") || !strings.Contains(logs.String(), "scope="+repo.Key()) {
		t.Fatalf("logs %q, want the not-found warn line naming %s", logs.String(), repo.Key())
	}
	if len(*alerts) == 0 || !strings.Contains(strings.Join(*alerts, "\n"), "scopes unreadable that long: "+org.Key()) {
		t.Fatalf("alerts %q, want the failing alert naming %s", *alerts, org.Key())
	}
}
