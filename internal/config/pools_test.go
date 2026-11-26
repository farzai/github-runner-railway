package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/farzai/github-runner-railway/internal/reconcile"
)

// pooled is base() with the pools in RUNNER_POOLS instead of the single-pool variables.
func pooled(pools string) map[string]string {
	m := base()
	for _, k := range []string{"RUNNER_IMAGE", "RUNNER_SCOPES"} {
		delete(m, k)
	}
	m["RUNNER_POOLS"] = pools
	return m
}

const twoPools = `[
 {"label":"railway","image":"ghcr.io/acme/runner:1","prefix":"rr-","max_runners":4,"scopes":["org:acme","repo:acme/app"]},
 {"label":"railway-pg","image":"ghcr.io/acme/runner:pg-1","prefix":"rp-","region":"europe-west4","max_runners":2,"scopes":["repo:acme/app"]}
]`

// The variables of a single-pool setup keep working: they are one pool, with today's defaults.
func TestTheSinglePoolVariablesLoadAsOnePool(t *testing.T) {
	m := base()
	m["RUNNER_REGION"] = "us-east4-eqdc4a"
	c, err := Load(env(m))
	if err != nil {
		t.Fatal(err)
	}
	want := []reconcile.Pool{{Label: "railway", Image: "ghcr.io/acme/github-runner-railway:1", Prefix: "rr-", Region: "us-east4-eqdc4a", MaxRunners: 8,
		Scopes: []reconcile.Scope{{Kind: "org", Owner: "acme"}, {Kind: "repo", Owner: "acme", Repo: "app"}}}}
	if !reflect.DeepEqual(c.Pools, want) {
		t.Errorf("pools %+v, want %+v", c.Pools, want)
	}
	m["RUNNER_LABEL"], m["RUNNER_SERVICE_PREFIX"], m["MAX_RUNNERS"] = "railway-pg", "rp-", "3"
	if c, err = Load(env(m)); err != nil || c.Pools[0].Label != "railway-pg" || c.Pools[0].Prefix != "rp-" || c.Pools[0].MaxRunners != 3 {
		t.Errorf("err %v pools %+v", err, c.Pools)
	}
}

func TestRunnerPoolsLoadsEveryPoolAndARegionFallsBackToRunnerRegion(t *testing.T) {
	m := pooled(twoPools)
	m["RUNNER_REGION"] = "us-east4-eqdc4a"
	c, err := Load(env(m))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Pools) != 2 {
		t.Fatalf("pools %+v", c.Pools)
	}
	a, b := c.Pools[0], c.Pools[1]
	if a.Label != "railway" || a.Image != "ghcr.io/acme/runner:1" || a.Prefix != "rr-" || a.MaxRunners != 4 || len(a.Scopes) != 2 || a.Region != "us-east4-eqdc4a" {
		t.Errorf("first pool %+v", a)
	}
	if b.Label != "railway-pg" || b.Prefix != "rp-" || b.MaxRunners != 2 || b.Region != "europe-west4" || len(b.Scopes) != 1 || b.Scopes[0].Key() != "repo:acme/app" {
		t.Errorf("second pool %+v", b)
	}
	if got := c.Labels(); !reflect.DeepEqual(got, []string{"railway", "railway-pg"}) {
		t.Errorf("labels %v", got)
	}
	if got := c.Prefixes(); !reflect.DeepEqual(got, []string{"rr-", "rp-"}) {
		t.Errorf("prefixes %v", got)
	}
	for _, s := range []string{"label=railway-pg", "prefix=rp-", "image=ghcr.io/acme/runner:pg-1", "region=europe-west4", "max=2", "scopes=repo:acme/app"} {
		if !strings.Contains(c.String(), s) {
			t.Errorf("String() %q lacks %q", c.String(), s)
		}
	}
}

func TestRunnerPoolsIsRefusedWhenMalformedNamingTheVariable(t *testing.T) {
	ok := `{"label":"railway","image":"i","prefix":"rr-","max_runners":1,"scopes":["repo:acme/app"]}`
	for name, pools := range map[string]string{
		"not JSON":          `[{`,
		"not a list":        ok,
		"empty list":        `[]`,
		"trailing data":     `[` + ok + `] []`,
		"unknown field":     `[{"label":"railway","image":"i","prefix":"rr-","max_runners":1,"scopes":["repo:acme/app"],"maxrunners":2}]`,
		"no label":          `[{"image":"i","prefix":"rr-","max_runners":1,"scopes":["repo:acme/app"]}]`,
		"no image":          `[{"label":"railway","prefix":"rr-","max_runners":1,"scopes":["repo:acme/app"]}]`,
		"no prefix":         `[{"label":"railway","image":"i","max_runners":1,"scopes":["repo:acme/app"]}]`,
		"no max":            `[{"label":"railway","image":"i","prefix":"rr-","scopes":["repo:acme/app"]}]`,
		"negative max":      `[{"label":"railway","image":"i","prefix":"rr-","max_runners":-1,"scopes":["repo:acme/app"]}]`,
		"no scopes":         `[{"label":"railway","image":"i","prefix":"rr-","max_runners":1,"scopes":[]}]`,
		"bad scope":         `[{"label":"railway","image":"i","prefix":"rr-","max_runners":1,"scopes":["acme"]}]`,
		"bad prefix":        `[{"label":"railway","image":"i","prefix":"rr-pg-","max_runners":1,"scopes":["repo:acme/app"]}]`,
		"bad region":        `[{"label":"railway","image":"i","prefix":"rr-","region":"US East","max_runners":1,"scopes":["repo:acme/app"]}]`,
		"same prefix":       `[` + ok + `,{"label":"railway-pg","image":"i","prefix":"rr-","max_runners":1,"scopes":["repo:acme/app"]}]`,
		"same label":        `[` + ok + `,{"label":"Railway","image":"i","prefix":"rp-","max_runners":1,"scopes":["repo:acme/app"]}]`,
		"reserved label":    `[{"label":"linux","image":"i","prefix":"rr-","max_runners":1,"scopes":["repo:acme/app"]}]`,
		"reserved label 2":  `[{"label":"Self-Hosted","image":"i","prefix":"rr-","max_runners":1,"scopes":["repo:acme/app"]}]`,
		"reserved label 3":  `[{"label":"X64","image":"i","prefix":"rr-","max_runners":1,"scopes":["repo:acme/app"]}]`,
		"blank label":       `[{"label":" ","image":"i","prefix":"rr-","max_runners":1,"scopes":["repo:acme/app"]}]`,
		"org with no group": `[{"label":"railway","image":"i","prefix":"rr-","max_runners":1,"scopes":["org:other"]}]`,
	} {
		m := pooled(pools)
		if name == "org with no group" {
			delete(m, "RUNNER_GROUP_ID")
		}
		_, err := Load(env(m))
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if want := "RUNNER_POOLS"; name != "org with no group" && !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err %q does not name %s", name, err, want)
		}
	}
}

// Two answers to the same question are refused rather than one silently winning.
func TestRunnerPoolsCannotBeMixedWithTheSinglePoolVariables(t *testing.T) {
	for _, k := range []string{"RUNNER_IMAGE", "RUNNER_LABEL", "RUNNER_SERVICE_PREFIX", "MAX_RUNNERS", "RUNNER_SCOPES"} {
		m := pooled(twoPools)
		m[k] = "x"
		if _, err := Load(env(m)); err == nil || !strings.Contains(err.Error(), k) || !strings.Contains(err.Error(), "RUNNER_POOLS") {
			t.Errorf("%s with RUNNER_POOLS: err %v", k, err)
		}
	}
	m := pooled(twoPools)
	m["RUNNER_REGION"] = "us-east4-eqdc4a" // the default region of every pool, not a single-pool variable
	if _, err := Load(env(m)); err != nil {
		t.Errorf("RUNNER_REGION with RUNNER_POOLS: %v", err)
	}
	m["RUNNER_REGION"] = "US East"
	if _, err := Load(env(m)); err == nil || !strings.Contains(err.Error(), "RUNNER_REGION") {
		t.Errorf("a bad RUNNER_REGION with RUNNER_POOLS: err %v", err)
	}
}

func TestWithoutRunnerPoolsTheImageAndScopesAreStillRequired(t *testing.T) {
	for _, k := range []string{"RUNNER_IMAGE", "RUNNER_SCOPES"} {
		m := base()
		delete(m, k)
		if _, err := Load(env(m)); err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("without %s: err %v", k, err)
		}
	}
}

func TestCreatesPerCycleAndThePollPeriodHaveAFloor(t *testing.T) {
	for k, v := range map[string]string{"MAX_CREATE_PER_CYCLE": "0", "POLL_SECONDS": "9"} {
		m := base()
		m[k] = v
		if _, err := Load(env(m)); err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("%s=%s: err %v", k, v, err)
		}
	}
	m := base()
	m["POLL_SECONDS"] = "10"
	if _, err := Load(env(m)); err != nil {
		t.Errorf("POLL_SECONDS=10: %v", err)
	}
}

// Each lower bound is exact: the bound itself is accepted, one below it refused.
func TestEachNumberIsAcceptedFromItsLowerBound(t *testing.T) {
	m := base()
	m["ALERT_FAILED_CYCLES"], m["STARTUP_GRACE_SECONDS"], m["MAX_RUNNERS"] = "1", "0", "1"
	if c, err := Load(env(m)); err != nil || c.FailAlertAfter != 1 || c.StartupGrace != 0 || c.Pools[0].MaxRunners != 1 {
		t.Errorf("err %v config %+v", err, c)
	}
	for _, v := range []string{"-1", "abc", "1.5"} {
		m := base()
		m["STARTUP_GRACE_SECONDS"] = v
		if _, err := Load(env(m)); err == nil || !strings.Contains(err.Error(), "STARTUP_GRACE_SECONDS") {
			t.Errorf("STARTUP_GRACE_SECONDS=%s: err %v", v, err)
		}
	}
	one := `[{"label":"railway","image":"i","prefix":"rr-","max_runners":1,"scopes":["repo:acme/app"]}]`
	if c, err := Load(env(pooled(one))); err != nil || c.Pools[0].MaxRunners != 1 {
		t.Errorf("max_runners 1: err %v", err)
	}
}
