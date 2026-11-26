// Package config reads the controller's settings from the environment. The App key, the
// webhook secret and the Railway token come only from variables; nothing here ever prints one.
package config

import (
	"encoding/json"
	"fmt"
	"regexp"
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
	Pools                            []reconcile.Pool
	RunnerGroupID                    int
	MaxCreatePerCycle                int // per pool, per cycle
	Interval, StartupGrace, IdleTTL  time.Duration
	MaxAge, AlertAfter               time.Duration
	FailAlertAfter                   int // failed cycles in a row before an alert and a 503
	AlertURL                         string
	Port                             string
}

// String is the config without its secrets.
func (c Config) String() string {
	pools := make([]string, 0, len(c.Pools))
	for _, p := range c.Pools {
		keys := make([]string, 0, len(p.Scopes))
		for _, s := range p.Scopes {
			keys = append(keys, s.Key())
		}
		pools = append(pools, fmt.Sprintf("[label=%s prefix=%s region=%s image=%s max=%d scopes=%s]", p.Label, p.Prefix, p.Region, p.Image, p.MaxRunners, strings.Join(keys, ",")))
	}
	return fmt.Sprintf("pools=%s interval=%s", strings.Join(pools, " "), c.Interval)
}

// Labels is every pool's label, in pool order.
func (c Config) Labels() []string { return c.each(func(p reconcile.Pool) string { return p.Label }) }

// Prefixes is every pool's service-name prefix, in pool order.
func (c Config) Prefixes() []string { return c.each(func(p reconcile.Pool) string { return p.Prefix }) }

func (c Config) each(field func(reconcile.Pool) string) []string {
	out := make([]string, len(c.Pools))
	for i, p := range c.Pools {
		out[i] = field(p)
	}
	return out
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
		{"RUNNER_RAILWAY_TOKEN", &c.RailwayToken}, {"RUNNER_RAILWAY_PROJECT_ID", &c.RailwayProject}, {"RUNNER_RAILWAY_ENVIRONMENT_ID", &c.RailwayEnv},
	} {
		if err := need(f.name, f.into); err != nil {
			return Config{}, err
		}
	}
	// Railway injects RAILWAY_PROJECT_ID into the controller for the project the controller runs in; the
	// runner project is another one, and a controller that managed its own project would put runner
	// services (running untrusted jobs) on its private network.
	if own := strings.TrimSpace(get("RAILWAY_PROJECT_ID")); own != "" && own == c.RailwayProject {
		return Config{}, fmt.Errorf("config: RUNNER_RAILWAY_PROJECT_ID is the project the controller runs in; the runners belong in their own project")
	}
	var err error
	num := func(name string, def int) int {
		n, nerr := number(get, name, def)
		if nerr != nil {
			err = nerr
		}
		return n
	}
	c.Port = text(get, "PORT", "8080")
	c.AlertURL = strings.TrimSpace(get("ALERT_WEBHOOK_URL"))
	c.RunnerGroupID = num("RUNNER_GROUP_ID", 0)
	c.MaxCreatePerCycle = num("MAX_CREATE_PER_CYCLE", 4)
	c.Interval = time.Duration(num("POLL_SECONDS", 120)) * time.Second
	c.StartupGrace = time.Duration(num("STARTUP_GRACE_SECONDS", 300)) * time.Second
	c.IdleTTL = time.Duration(num("IDLE_TTL_SECONDS", 120)) * time.Second
	c.MaxAge = time.Duration(num("MAX_JOB_MINUTES", 360)+10) * time.Minute
	c.AlertAfter = time.Duration(num("ALERT_QUEUED_MINUTES", 5)) * time.Minute
	c.FailAlertAfter = num("ALERT_FAILED_CYCLES", 3)
	if err != nil {
		return Config{}, err
	}
	if c.Pools, err = loadPools(get); err != nil {
		return Config{}, err
	}
	for _, p := range c.Pools {
		for _, sc := range p.Scopes {
			if sc.Kind == "org" && (c.RunnerGroupID == 0 || c.RunnerGroupID == 1) {
				return Config{}, fmt.Errorf("config: RUNNER_GROUP_ID must name a dedicated runner group (not the Default group, 1) for an org scope")
			}
		}
	}
	if c.FailAlertAfter < 1 {
		return Config{}, fmt.Errorf("config: ALERT_FAILED_CYCLES must be at least 1")
	}
	if c.MaxCreatePerCycle < 1 || c.Interval < 10*time.Second {
		return Config{}, fmt.Errorf("config: MAX_CREATE_PER_CYCLE must be at least 1 and POLL_SECONDS at least 10")
	}
	return c, nil
}

// text reads a variable, def when it is unset or blank.
func text(get func(string) string, name, def string) string {
	if v := strings.TrimSpace(get(name)); v != "" {
		return v
	}
	return def
}

// number reads a non-negative whole number, def when the variable is unset.
func number(get func(string) string, name string, def int) (int, error) {
	v := strings.TrimSpace(get(name))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def, fmt.Errorf("config: %s is not a non-negative number", name)
	}
	return n, nil
}

// singlePoolVars are the variables of a one-pool setup, which RUNNER_POOLS replaces.
var singlePoolVars = []string{"RUNNER_IMAGE", "RUNNER_LABEL", "RUNNER_SERVICE_PREFIX", "MAX_RUNNERS", "RUNNER_SCOPES"}

// poolNames is how errors name each field of a pool: the single-pool variable, or the field of
// RUNNER_POOLS it came from.
type poolNames struct{ label, image, prefix, region, max, scopes string }

func singleNames() poolNames {
	return poolNames{"RUNNER_LABEL", "RUNNER_IMAGE", "RUNNER_SERVICE_PREFIX", "RUNNER_REGION", "MAX_RUNNERS", "RUNNER_SCOPES"}
}

func pooledNames(i int) poolNames {
	f := func(field string) string { return fmt.Sprintf("RUNNER_POOLS[%d].%s", i, field) }
	return poolNames{f("label"), f("image"), f("prefix"), f("region"), f("max_runners"), f("scopes")}
}

// loadPools reads RUNNER_POOLS, or the single-pool variables as one pool when it is unset. A
// pool with no region of its own takes RUNNER_REGION.
func loadPools(get func(string) string) ([]reconcile.Pool, error) {
	region := strings.TrimSpace(get("RUNNER_REGION"))
	if err := checkRegion(region, "RUNNER_REGION"); err != nil {
		return nil, err
	}
	raw := strings.TrimSpace(get("RUNNER_POOLS"))
	if raw == "" {
		p, err := singlePool(get, region)
		if err != nil {
			return nil, err
		}
		return []reconcile.Pool{p}, nil
	}
	for _, name := range singlePoolVars {
		if strings.TrimSpace(get(name)) != "" {
			return nil, fmt.Errorf("config: %s cannot be set together with RUNNER_POOLS; put it in the pool instead", name)
		}
	}
	return parsePools(raw, region)
}

// singlePool is the one pool of the single-pool variables, with today's defaults.
func singlePool(get func(string) string, region string) (reconcile.Pool, error) {
	p := reconcile.Pool{Image: text(get, "RUNNER_IMAGE", ""), Label: text(get, "RUNNER_LABEL", "railway"), Prefix: text(get, "RUNNER_SERVICE_PREFIX", "rr-"), Region: region}
	if p.Image == "" {
		return p, fmt.Errorf("config: RUNNER_IMAGE is not set")
	}
	raw := text(get, "RUNNER_SCOPES", "")
	if raw == "" {
		return p, fmt.Errorf("config: RUNNER_SCOPES is not set")
	}
	var err error
	if p.Scopes, err = parseScopes(strings.Split(raw, ","), "RUNNER_SCOPES"); err != nil {
		return p, err
	}
	if p.MaxRunners, err = number(get, "MAX_RUNNERS", 8); err != nil {
		return p, err
	}
	return p, checkPool(p, singleNames())
}

// poolJSON is one entry of RUNNER_POOLS.
type poolJSON struct {
	Label      string   `json:"label"`
	Image      string   `json:"image"`
	Prefix     string   `json:"prefix"`
	Region     string   `json:"region"`
	MaxRunners int      `json:"max_runners"`
	Scopes     []string `json:"scopes"`
}

// parsePools reads RUNNER_POOLS strictly: one JSON array, no unknown field, every pool checked
// on its own and against the others.
func parsePools(raw, region string) ([]reconcile.Pool, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var in []poolJSON
	if err := dec.Decode(&in); err != nil || dec.More() {
		return nil, fmt.Errorf("config: RUNNER_POOLS is not a JSON array of pools (label, image, prefix, max_runners, scopes, and optional region)")
	}
	if len(in) == 0 {
		return nil, fmt.Errorf("config: RUNNER_POOLS has no pool")
	}
	pools := make([]reconcile.Pool, 0, len(in))
	for i, pj := range in {
		names := pooledNames(i)
		p, err := pj.pool(region, names)
		if err == nil {
			err = checkApart(p, pools, names)
		}
		if err != nil {
			return nil, err
		}
		pools = append(pools, p)
	}
	return pools, nil
}

// pool is the entry as a checked pool; with no region of its own it takes region.
func (pj poolJSON) pool(region string, names poolNames) (reconcile.Pool, error) {
	p := reconcile.Pool{Label: strings.TrimSpace(pj.Label), Image: strings.TrimSpace(pj.Image), Prefix: strings.TrimSpace(pj.Prefix),
		Region: strings.TrimSpace(pj.Region), MaxRunners: pj.MaxRunners}
	if p.Region == "" {
		p.Region = region
	}
	for _, f := range []struct{ v, name string }{{p.Label, names.label}, {p.Image, names.image}, {p.Prefix, names.prefix}} {
		if f.v == "" {
			return p, fmt.Errorf("config: %s is not set", f.name)
		}
	}
	if len(pj.Scopes) == 0 {
		return p, fmt.Errorf("config: %s is empty", names.scopes)
	}
	var err error
	if p.Scopes, err = parseScopes(pj.Scopes, names.scopes); err != nil {
		return p, err
	}
	return p, checkPool(p, names)
}

// checkPool checks what a pool must be on its own.
func checkPool(p reconcile.Pool, names poolNames) error {
	if p.Label == "" || reserved[strings.ToLower(p.Label)] {
		return fmt.Errorf("config: %s must be a label of your own, not self-hosted, linux or x64 (every runner has those)", names.label)
	}
	// One letter, one letter or digit, a dash: never a prefix of another valid prefix, so two pools
	// (or two controllers) sharing the runner project cannot list or reap each other's services.
	if !servicePrefix.MatchString(p.Prefix) {
		return fmt.Errorf("config: %s must be two lowercase letters or digits (the first a letter) and a dash, e.g. rp-", names.prefix)
	}
	if err := checkRegion(p.Region, names.region); err != nil {
		return err
	}
	if p.MaxRunners < 1 {
		return fmt.Errorf("config: %s must be at least 1", names.max)
	}
	return nil
}

// checkApart refuses a pool that shares a prefix or a label with an earlier one: a prefix would
// let one pool reap the other's services, and a label would leave a job two pools to go to.
func checkApart(p reconcile.Pool, earlier []reconcile.Pool, names poolNames) error {
	for _, e := range earlier {
		if e.Prefix == p.Prefix {
			return fmt.Errorf("config: %s %s is another pool's prefix; each pool needs its own", names.prefix, p.Prefix)
		}
		if strings.EqualFold(e.Label, p.Label) {
			return fmt.Errorf("config: %s %s is another pool's label (labels compare without case); each pool needs its own", names.label, p.Label)
		}
	}
	return nil
}

// reserved is the labels every runner has, which cannot tell one pool from another.
var reserved = map[string]bool{"self-hosted": true, "linux": true, "x64": true}

// checkRegion accepts a Railway region id, e.g. us-east4-eqdc4a, or nothing. Railway itself
// refuses an unknown one when the first runner is created; this only catches what cannot be a
// region id at all.
func checkRegion(r, name string) error {
	if r != "" && !region.MatchString(r) {
		return fmt.Errorf("config: %s must be a Railway region id of lowercase letters, digits and dashes, e.g. us-east4-eqdc4a", name)
	}
	return nil
}

var servicePrefix = regexp.MustCompile(`^[a-z][a-z0-9]-$`)

var region = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

func parseScopes(entries []string, name string) ([]reconcile.Scope, error) {
	out := make([]reconcile.Scope, 0, len(entries))
	for _, e := range entries {
		s, err := parseScope(strings.TrimSpace(e), name)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func parseScope(s, name string) (reconcile.Scope, error) {
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
	return reconcile.Scope{}, fmt.Errorf("config: %s entry %q is not org:<name> or repo:<owner>/<name>", name, s)
}
