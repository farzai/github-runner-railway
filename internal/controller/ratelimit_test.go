package controller

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/farzai/github-runner-railway/internal/ghapp"
)

type limitedGH struct {
	fakeGH
	limits map[string]ghapp.RateLimit
}

func (g *limitedGH) RateLimits() map[string]ghapp.RateLimit { return g.limits }

// Below a tenth of its limit an installation is logged once per reset window, not every cycle.
func TestALowGitHubRateLimitIsLoggedOncePerWindow(t *testing.T) {
	reset := now.Add(30 * time.Minute)
	gh := &limitedGH{limits: map[string]ghapp.RateLimit{"acme": {Limit: 5000, Remaining: 400, Reset: reset}, "other": {Limit: 5000, Remaining: 4000, Reset: reset}}}
	var buf bytes.Buffer
	c := newC(&gh.fakeGH, &fakeRW{})
	c.gh = gh
	c.cfg.Log = slog.New(slog.NewTextHandler(&buf, nil))
	for i := 0; i < 3; i++ {
		_ = c.Cycle(context.Background())
	}
	if n := strings.Count(buf.String(), "github rate limit low"); n != 1 || !strings.Contains(buf.String(), "account=acme") {
		t.Fatalf("log %q: want one warning for acme", buf.String())
	}
	gh.limits["acme"] = ghapp.RateLimit{Limit: 5000, Remaining: 300, Reset: reset.Add(time.Hour)}
	_ = c.Cycle(context.Background())
	if n := strings.Count(buf.String(), "github rate limit low"); n != 2 {
		t.Fatalf("a new window must warn again: %d warnings", n)
	}
}

func TestHealthzShowsTheRateLimitPerAccount(t *testing.T) {
	gh := &limitedGH{limits: map[string]ghapp.RateLimit{"acme": {Limit: 5000, Remaining: 4321}}}
	c := newC(&gh.fakeGH, &fakeRW{})
	c.gh = gh
	_ = c.Cycle(context.Background())
	_, body := health(t, c)
	rl, _ := body["github_rate_limits"].(map[string]any)
	acme, _ := rl["acme"].(map[string]any)
	if acme["remaining"] != float64(4321) || acme["limit"] != float64(5000) {
		t.Fatalf("github_rate_limits %v", body["github_rate_limits"])
	}
}
