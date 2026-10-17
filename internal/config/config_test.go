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
		"RAILWAY_API_TOKEN": "RW-SECRET", "RAILWAY_PROJECT_ID": "p", "RAILWAY_ENVIRONMENT_ID": "e",
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
	for _, k := range []string{"GITHUB_APP_ID", "GITHUB_APP_PRIVATE_KEY", "GITHUB_WEBHOOK_SECRET", "RAILWAY_API_TOKEN", "RAILWAY_PROJECT_ID", "RAILWAY_ENVIRONMENT_ID", "RUNNER_IMAGE", "RUNNER_SCOPES"} {
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
