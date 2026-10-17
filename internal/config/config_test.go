package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func base() map[string]string {
	return map[string]string{
		"GITHUB_APP_ID": "1", "GITHUB_APP_PRIVATE_KEY": "KEY-SECRET", "GITHUB_WEBHOOK_SECRET": "WH-SECRET",
		"RUNNER_RAILWAY_TOKEN": "RW-SECRET", "RUNNER_RAILWAY_PROJECT_ID": "p", "RUNNER_RAILWAY_ENVIRONMENT_ID": "e",
		"RUNNER_IMAGE": "ghcr.io/farzai/github-runner-railway:1", "RUNNER_SCOPES": "org:farzai, repo:parsilver/skyabove",
		"RUNNER_GROUP_ID": "7",
	}
}

func TestLoadParsesScopesAndDefaults(t *testing.T) {
	c, err := Load(env(base()))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Scopes) != 2 || c.Scopes[0].Key() != "org:farzai" || c.Scopes[1].Key() != "repo:parsilver/skyabove" {
		t.Errorf("scopes %+v", c.Scopes)
	}
	if c.Label != "railway-farzai" || c.MaxRunners != 8 || c.Interval != 2*time.Minute || c.MaxAge != 6*time.Hour+10*time.Minute || c.Port != "8080" {
		t.Errorf("defaults %+v", c)
	}
}

func TestLoadRefusesWhatIsMissingOrMalformedAndNamesOnlyTheVariable(t *testing.T) {
	for _, k := range []string{"GITHUB_APP_ID", "GITHUB_APP_PRIVATE_KEY", "GITHUB_WEBHOOK_SECRET", "RUNNER_RAILWAY_TOKEN", "RUNNER_RAILWAY_PROJECT_ID", "RUNNER_RAILWAY_ENVIRONMENT_ID", "RUNNER_IMAGE", "RUNNER_SCOPES"} {
		m := base()
		delete(m, k)
		_, err := Load(env(m))
		if err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("without %s: err %v", k, err)
		}
	}
	for _, bad := range []string{"farzai", "org:", "repo:parsilver", "repo:a/b/c", "team:x"} {
		m := base()
		m["RUNNER_SCOPES"] = bad
		if _, err := Load(env(m)); err == nil {
			t.Errorf("scope %q accepted", bad)
		}
	}
	m := base()
	m["MAX_RUNNERS"] = "0"
	if _, err := Load(env(m)); err == nil {
		t.Error("MAX_RUNNERS=0 accepted")
	}
	m = base()
	m["POLL_SECONDS"] = "abc"
	if _, err := Load(env(m)); err == nil {
		t.Error("a malformed number accepted")
	}
}

func TestNoErrorOrStringEverCarriesASecret(t *testing.T) {
	m := base()
	m["MAX_RUNNERS"] = "x"
	_, err := Load(env(m))
	c, _ := Load(env(base()))
	for _, s := range []string{err.Error(), c.String()} {
		for _, secret := range []string{"KEY-SECRET", "WH-SECRET", "RW-SECRET"} {
			if strings.Contains(s, secret) {
				t.Errorf("%q leaks %s", s, secret)
			}
		}
	}
}

func TestAnOrgScopeNeedsAnExplicitNonDefaultRunnerGroup(t *testing.T) {
	m := base()
	delete(m, "RUNNER_GROUP_ID")
	if _, err := Load(env(m)); err == nil || !strings.Contains(err.Error(), "RUNNER_GROUP_ID") {
		t.Errorf("an org scope with no group: err %v", err)
	}
	m["RUNNER_GROUP_ID"] = "1"
	if _, err := Load(env(m)); err == nil {
		t.Error("the Default group (1) accepted: a dedicated group is required")
	}
	m["RUNNER_GROUP_ID"] = "7"
	if c, err := Load(env(m)); err != nil || c.RunnerGroupID != 7 {
		t.Errorf("group 7: %v %+v", err, c)
	}
	m = base()
	delete(m, "RUNNER_GROUP_ID")
	m["RUNNER_SCOPES"] = "repo:parsilver/skyabove"
	if _, err := Load(env(m)); err != nil {
		t.Errorf("repo scopes use the default group by necessity and need none: %v", err)
	}
}

// Railway injects its own RAILWAY_PROJECT_ID and RAILWAY_ENVIRONMENT_ID (and RAILWAY_*) into every service
// for the project the service runs in. A controller that read the runner project from those names would
// manage its OWN project, so its settings use other names, and it refuses to start if the runner project
// is the project it runs in.
func TestTheRunnerProjectComesFromVariablesRailwayDoesNotInject(t *testing.T) {
	m := base()
	m["RAILWAY_PROJECT_ID"] = "the-controllers-own-project" // what Railway injects
	m["RAILWAY_ENVIRONMENT_ID"] = "the-controllers-own-environment"
	c, err := Load(env(m))
	if err != nil {
		t.Fatal(err)
	}
	if c.RailwayProject != "p" || c.RailwayEnv != "e" || c.RailwayToken != "RW-SECRET" {
		t.Errorf("project %q env %q: the runner project must come from RUNNER_RAILWAY_*, never from what Railway injects", c.RailwayProject, c.RailwayEnv)
	}
}

func TestTheControllerRefusesToManageTheProjectItRunsIn(t *testing.T) {
	m := base()
	m["RAILWAY_PROJECT_ID"] = "p" // the controller's own project is the runner project
	if _, err := Load(env(m)); err == nil || !strings.Contains(err.Error(), "RUNNER_RAILWAY_PROJECT_ID") {
		t.Errorf("err %v: runner services created beside the controller share its private network", err)
	}
}
