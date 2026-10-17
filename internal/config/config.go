// Package config reads the controller's settings from the environment. The App key, the
// webhook secret and the Railway token come only from variables; nothing here ever prints one.
package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/farzai/github-runner-railway/internal/reconcile"
)

// Config is everything the controller needs.
type Config struct {
	AppID, PrivateKey, WebhookSecret string
	RailwayToken                     string
	RailwayProject, RailwayEnv       string
	Image, Label                     string
	Scopes                           []reconcile.Scope
	RunnerGroupID                    int
	MaxRunners, MaxCreatePerCycle    int
	Interval, StartupGrace, IdleTTL  time.Duration
	MaxAge, AlertAfter               time.Duration
	AlertURL                         string
	Port                             string
}

// String is the config without its secrets.
func (c Config) String() string {
	keys := make([]string, 0, len(c.Scopes))
	for _, s := range c.Scopes {
		keys = append(keys, s.Key())
	}
	return fmt.Sprintf("scopes=%s label=%s image=%s max=%d interval=%s", strings.Join(keys, ","), c.Label, c.Image, c.MaxRunners, c.Interval)
}

// Load reads and checks the environment through get.
func Load(get func(string) string) (Config, error) {
	var c Config
	need := func(name string, into *string) error {
		*into = strings.TrimSpace(get(name))
		if *into == "" {
			return fmt.Errorf("config: %s is not set", name)
		}
		return nil
	}
	for _, f := range []struct {
		name string
		into *string
	}{
		{"GITHUB_APP_ID", &c.AppID}, {"GITHUB_APP_PRIVATE_KEY", &c.PrivateKey}, {"GITHUB_WEBHOOK_SECRET", &c.WebhookSecret},
		{"RAILWAY_API_TOKEN", &c.RailwayToken}, {"RAILWAY_PROJECT_ID", &c.RailwayProject}, {"RAILWAY_ENVIRONMENT_ID", &c.RailwayEnv},
		{"RUNNER_IMAGE", &c.Image},
	} {
		if err := need(f.name, f.into); err != nil {
			return Config{}, err
		}
	}
	var raw string
	if err := need("RUNNER_SCOPES", &raw); err != nil {
		return Config{}, err
	}
	for _, part := range strings.Split(raw, ",") {
		s, err := parseScope(strings.TrimSpace(part))
		if err != nil {
			return Config{}, err
		}
		c.Scopes = append(c.Scopes, s)
	}
	str := func(name, def string) string {
		if v := strings.TrimSpace(get(name)); v != "" {
			return v
		}
		return def
	}
	var err error
	num := func(name string, def int) int {
		v := strings.TrimSpace(get(name))
		if v == "" {
			return def
		}
		n, perr := strconv.Atoi(v)
		if perr != nil || n < 0 {
			err = fmt.Errorf("config: %s is not a non-negative number", name)
			return def
		}
		return n
	}
	c.Label = str("RUNNER_LABEL", "railway-farzai")
	c.Port = str("PORT", "8080")
	c.AlertURL = strings.TrimSpace(get("ALERT_WEBHOOK_URL"))
	c.RunnerGroupID = num("RUNNER_GROUP_ID", 0)
	c.MaxRunners = num("MAX_RUNNERS", 8)
	c.MaxCreatePerCycle = num("MAX_CREATE_PER_CYCLE", 4)
	c.Interval = time.Duration(num("POLL_SECONDS", 120)) * time.Second
	c.StartupGrace = time.Duration(num("STARTUP_GRACE_SECONDS", 300)) * time.Second
	c.IdleTTL = time.Duration(num("IDLE_TTL_SECONDS", 120)) * time.Second
	c.MaxAge = time.Duration(num("MAX_JOB_MINUTES", 360)+10) * time.Minute
	c.AlertAfter = time.Duration(num("ALERT_QUEUED_MINUTES", 5)) * time.Minute
	if err != nil {
		return Config{}, err
	}
	for _, sc := range c.Scopes {
		if sc.Kind == "org" && (c.RunnerGroupID == 0 || c.RunnerGroupID == 1) {
			return Config{}, fmt.Errorf("config: RUNNER_GROUP_ID must name a dedicated runner group (not the Default group, 1) for an org scope")
		}
	}
	if c.MaxRunners < 1 || c.MaxCreatePerCycle < 1 || c.Interval < 10*time.Second {
		return Config{}, fmt.Errorf("config: MAX_RUNNERS and MAX_CREATE_PER_CYCLE must be at least 1 and POLL_SECONDS at least 10")
	}
	return c, nil
}

func parseScope(s string) (reconcile.Scope, error) {
	kind, rest, ok := strings.Cut(s, ":")
	switch {
	case ok && kind == "org" && rest != "" && !strings.Contains(rest, "/"):
		return reconcile.Scope{Kind: "org", Owner: rest}, nil
	case ok && kind == "repo":
		owner, repo, ok := strings.Cut(rest, "/")
		if ok && owner != "" && repo != "" && !strings.Contains(repo, "/") {
			return reconcile.Scope{Kind: "repo", Owner: owner, Repo: repo}, nil
		}
	}
	return reconcile.Scope{}, fmt.Errorf("config: RUNNER_SCOPES entry %q is not org:<name> or repo:<owner>/<name>", s)
}
