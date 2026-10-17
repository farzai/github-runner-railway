// Package controller runs the loop: look at GitHub and Railway, ask reconcile for a plan,
// apply it. GitHub and Railway are the truth, read afresh each cycle; nothing is remembered
// between cycles except which late jobs were already alerted.
package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/farzai/github-runner-railway/internal/ghapp"
	"github.com/farzai/github-runner-railway/internal/reconcile"
	"github.com/farzai/github-runner-railway/internal/webhook"
)

// GH is the GitHub side.
type GH interface {
	QueuedJobs(ctx context.Context, s reconcile.Scope) ([]ghapp.Job, error)
	Runners(ctx context.Context, s reconcile.Scope) ([]reconcile.Runner, error)
	JITConfig(ctx context.Context, s reconcile.Scope, name string) (string, int64, error)
	DeleteRunner(ctx context.Context, s reconcile.Scope, id int64) error
}

// RW is the Railway side.
type RW interface {
	List(ctx context.Context) ([]reconcile.Service, error)
	Create(ctx context.Context, name string, vars map[string]string) (string, error)
	Delete(ctx context.Context, id string) error
}

// Config is the controller's knobs.
type Config struct {
	Scopes            []reconcile.Scope
	Prefix            string
	MaxRunners        int
	MaxCreatePerCycle int
	StartupGrace      time.Duration
	IdleTTL           time.Duration
	MaxAge            time.Duration
	AlertAfter        time.Duration // a job queued this long with no runner coming is an alert
	Interval          time.Duration // the polling period; a webhook wakes a cycle sooner
	MinGap            time.Duration // a cycle never starts sooner than this after the previous one started
	Debounce          time.Duration // after a wake-up, wait this long so a burst of events is one cycle
	FailAlertAfter    int           // this many failed cycles in a row raise one alert and turn /healthz 503
	Alert             func(msg string)
	Log               *slog.Logger
}

// Controller applies plans.
type Controller struct {
	cfg     Config
	gh      GH
	rw      RW
	now     func() time.Time
	wake    chan struct{}
	mu      sync.Mutex
	alerted map[int64]string // job id -> scope key
	status  Status

	failures    int            // failed cycles in a row
	lastSuccess time.Time      // the end of the last cycle with no error
	scopeFails  map[string]int // scope key -> cycles in a row it could not be read
	failAlerted bool           // the failing state has been alerted and not yet recovered
}

// Status is the last cycle, for /healthz.
type Status struct {
	LastCycle           time.Time
	LastError           string
	Services            int
	Queued              int
	ConsecutiveFailures int
	LastSuccess         time.Time
	UnreadableScopes    []string // scopes that could not be read in the last cycle
	Failing             bool     // ConsecutiveFailures reached FailAlertAfter
}

// New builds a Controller.
func New(cfg Config, gh GH, rw RW) *Controller {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Alert == nil {
		cfg.Alert = func(string) {}
	}
	if cfg.MinGap <= 0 {
		cfg.MinGap = 20 * time.Second
	}
	if cfg.Debounce <= 0 {
		cfg.Debounce = 3 * time.Second
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 2 * time.Minute
	}
	if cfg.FailAlertAfter <= 0 {
		cfg.FailAlertAfter = 3
	}
	return &Controller{cfg: cfg, gh: gh, rw: rw, now: time.Now, wake: make(chan struct{}, 1), alerted: map[int64]string{},
		scopeFails: map[string]int{}}
}

// OnEvent is the webhook handler. Only a queued job can need a new runner; the rest wait for
// the next poll, since every wake-up costs a scan of GitHub.
func (c *Controller) OnEvent(e webhook.Event) {
	if e.Action == "queued" {
		c.Nudge()
	}
}

// Nudge asks for a cycle soon; many nudges are one.
func (c *Controller) Nudge() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Status is the state after the last cycle.
func (c *Controller) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

// Run cycles until ctx ends: on every interval and on every nudge. A nudge waits out the debounce
// (and the rest of MinGap since the previous cycle started); nudges that arrive meanwhile are the
// same cycle. GitHub may not list a just-queued job yet, so a nudged cycle that found nothing
// queued checks once more after MinGap. The interval restarts after every cycle, so a tick never
// follows a nudged cycle straight away.
func (c *Controller) Run(ctx context.Context) {
	t := time.NewTicker(c.cfg.Interval)
	defer t.Stop()
	var retry <-chan time.Time
	nudged := false
	for {
		lastStart := time.Now()
		if err := c.Cycle(ctx); err != nil {
			c.cfg.Log.Error("cycle failed", "err", err.Error())
		}
		t.Reset(c.cfg.Interval)
		retry = nil
		if nudged && c.Status().Queued == 0 {
			retry = time.After(c.cfg.MinGap)
		}
		nudged = false
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-retry:
		case <-c.wake:
			nudged = true
			select {
			case <-ctx.Done():
				return
			case <-time.After(wakeDelay(time.Now(), lastStart, c.cfg.Debounce, c.cfg.MinGap)):
			}
			select { // a nudge that came in during the wait is this cycle
			case <-c.wake:
			default:
			}
		}
	}
}

func scopeByKey(ss []reconcile.Scope, key string) reconcile.Scope {
	for _, s := range ss {
		if s.Key() == key {
			return s
		}
	}
	return reconcile.Scope{}
}

// Cycle is one pass. A scope GitHub cannot be read for is left alone entirely: its services
// are neither counted nor reaped, since a guess about them could kill a busy runner.
func (c *Controller) Cycle(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	var errs []error
	queued := map[string]int{}
	runners := map[string][]reconcile.Runner{}
	jobsOf := map[string][]ghapp.Job{}
	var readable []reconcile.Scope
	for _, s := range c.cfg.Scopes {
		jobs, err := c.gh.QueuedJobs(ctx, s)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", s.Key(), err))
			c.scopeFails[s.Key()]++
			continue
		}
		rs, err := c.gh.Runners(ctx, s)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", s.Key(), err))
			c.scopeFails[s.Key()]++
			continue
		}
		delete(c.scopeFails, s.Key())
		readable = append(readable, s)
		queued[s.Key()], runners[s.Key()], jobsOf[s.Key()] = len(jobs), rs, jobs
	}
	all, err := c.rw.List(ctx)
	if err != nil {
		errs = append(errs, err)
		c.record(now, errs, 0, 0)
		return errors.Join(errs...)
	}
	ok := map[string]bool{}
	for _, s := range readable {
		ok[s.Hash()] = true
	}
	var services []reconcile.Service
	for _, sv := range all {
		known := false
		for _, s := range c.cfg.Scopes {
			known = known || s.Hash() == sv.ScopeHash
		}
		if !known || ok[sv.ScopeHash] { // a service of an unreadable scope is not looked at
			services = append(services, sv)
		}
	}
	plan := reconcile.Make(reconcile.Input{
		Now: now, Queued: queued, Services: services, Runners: runners,
		Params: reconcile.Params{MaxRunners: c.cfg.MaxRunners, MaxCreatePerCycle: c.cfg.MaxCreatePerCycle, StartupGrace: c.cfg.StartupGrace,
			IdleTTL: c.cfg.IdleTTL, MaxAge: c.cfg.MaxAge, Scopes: readable},
	})
	for _, r := range plan.Reap {
		if err := c.reap(ctx, r); err != nil {
			errs = append(errs, err)
		}
	}
	created := map[string]int{}
	for _, cr := range plan.Create {
		if err := c.create(ctx, scopeByKey(readable, cr.Scope)); err != nil {
			errs = append(errs, err)
			continue
		}
		created[cr.Scope]++
	}
	c.alertLate(now, jobsOf, services, runners, created)
	total := 0
	for _, n := range queued {
		total += n
	}
	c.record(now, errs, len(services)+len(plan.Create)-len(plan.Reap), total)
	return errors.Join(errs...)
}

// record keeps the cycle's outcome for /healthz and raises the failing and recovered alerts: one
// alert when FailAlertAfter cycles in a row have failed, naming the scopes unreadable for that long,
// and one message when a cycle succeeds again. A failed cycle still stamps LastCycle (the loop is
// alive); ConsecutiveFailures is what tells a failing controller from a working one.
func (c *Controller) record(now time.Time, errs []error, services, queued int) {
	if len(errs) > 0 {
		c.failures++
	} else {
		c.failures = 0
		c.lastSuccess = now
	}
	var unreadable, stuck []string
	for _, s := range c.cfg.Scopes {
		if n := c.scopeFails[s.Key()]; n > 0 {
			unreadable = append(unreadable, s.Key())
			if n >= c.cfg.FailAlertAfter {
				stuck = append(stuck, s.Key())
			}
		}
	}
	c.status = Status{LastCycle: now, Services: services, Queued: queued, ConsecutiveFailures: c.failures,
		LastSuccess: c.lastSuccess, UnreadableScopes: unreadable, Failing: c.failures >= c.cfg.FailAlertAfter}
	if len(errs) > 0 {
		c.status.LastError = errors.Join(errs...).Error()
	}
	switch {
	case c.failures >= c.cfg.FailAlertAfter && !c.failAlerted:
		c.failAlerted = true
		msg := fmt.Sprintf("controller: %d cycles in a row failed", c.failures)
		if len(stuck) > 0 {
			msg += "; scopes unreadable that long: " + strings.Join(stuck, ", ")
		}
		c.cfg.Alert(msg + "; last error: " + truncate(c.status.LastError, 300))
	case c.failures == 0 && c.failAlerted:
		c.failAlerted = false
		c.cfg.Alert("controller: recovered, the last cycle succeeded")
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func (c *Controller) reap(ctx context.Context, r reconcile.Reap) error {
	if r.RunnerID != 0 {
		sc := scopeByKey(c.cfg.Scopes, r.Scope)
		if err := c.gh.DeleteRunner(ctx, sc, r.RunnerID); err != nil {
			if errors.Is(err, ghapp.ErrBusy) && !r.Force {
				c.cfg.Log.Info("runner turned busy; kept", "service", r.Service.Name)
				return nil
			}
			if !errors.Is(err, ghapp.ErrBusy) {
				return fmt.Errorf("delete runner %s: %w", r.Service.Name, err)
			}
		}
	}
	c.cfg.Log.Info("reaping", "service", r.Service.Name, "reason", r.Reason)
	return c.rw.Delete(ctx, r.Service.ID)
}

func (c *Controller) create(ctx context.Context, s reconcile.Scope) error {
	var b [3]byte
	_, _ = rand.Read(b[:])
	name := c.cfg.Prefix + s.Hash() + "-" + hex.EncodeToString(b[:])
	jit, runnerID, err := c.gh.JITConfig(ctx, s, name)
	if err != nil {
		return fmt.Errorf("jit config %s: %w", s.Key(), err)
	}
	if _, err := c.rw.Create(ctx, name, map[string]string{"ACTIONS_RUNNER_INPUT_JITCONFIG": jit}); err != nil {
		if derr := c.gh.DeleteRunner(ctx, s, runnerID); derr != nil {
			c.cfg.Log.Error("could not remove the runner of a failed service", "runner", name)
		}
		return fmt.Errorf("create service %s: %w", name, err)
	}
	c.cfg.Log.Info("created", "service", name, "scope", s.Key())
	return nil
}

// alertLate raises one alert per job that has waited past AlertAfter with no runner coming
// for its scope (a service created this cycle or still starting counts as coming).
func (c *Controller) alertLate(now time.Time, jobs map[string][]ghapp.Job, services []reconcile.Service, runners map[string][]reconcile.Runner, created map[string]int) {
	live := map[int64]bool{}
	for key, js := range jobs {
		sc := scopeByKey(c.cfg.Scopes, key)
		coming := created[key]
		for _, sv := range services {
			if sv.ScopeHash != sc.Hash() || sv.Exited {
				continue
			}
			registered := false
			for _, r := range runners[key] {
				registered = registered || r.Name == sv.Name
			}
			if !registered && now.Sub(sv.CreatedAt) <= reconcile.RegisterLag {
				coming++
			}
		}
		for _, j := range js {
			live[j.ID] = true
			if _, done := c.alerted[j.ID]; coming > 0 || done || j.CreatedAt.IsZero() || now.Sub(j.CreatedAt) < c.cfg.AlertAfter {
				continue
			}
			c.alerted[j.ID] = key
			c.cfg.Alert(fmt.Sprintf("job %d in %s has been queued %s with no runner coming", j.ID, j.Repo, now.Sub(j.CreatedAt).Round(time.Second)))
		}
	}
	for id, key := range c.alerted {
		if _, readable := jobs[key]; readable && !live[id] { // a scope that could not be read keeps its marks
			delete(c.alerted, id)
		}
	}
}

// HealthHandler is /healthz: 503 until a cycle has run, when the last one is older than three
// intervals, or while FailAlertAfter or more cycles in a row have failed.
func (c *Controller) HealthHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		st := c.Status()
		w.Header().Set("Content-Type", "application/json")
		if st.LastCycle.IsZero() || c.now().Sub(st.LastCycle) > 3*c.cfg.Interval || st.Failing {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		var lastSuccess any
		if !st.LastSuccess.IsZero() {
			lastSuccess = st.LastSuccess
		}
		unreadable := st.UnreadableScopes
		if unreadable == nil {
			unreadable = []string{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"last_cycle": st.LastCycle, "services": st.Services, "queued": st.Queued,
			"error": st.LastError != "", "consecutive_failures": st.ConsecutiveFailures, "last_success": lastSuccess,
			"unreadable_scopes": unreadable})
	})
}

// wakeDelay is how long to wait after a wake-up: the debounce, or longer when the previous cycle
// started less than minGap ago.
func wakeDelay(now, lastStart time.Time, debounce, minGap time.Duration) time.Duration {
	if lastStart.IsZero() {
		return debounce
	}
	if rest := lastStart.Add(minGap).Sub(now); rest > debounce {
		return rest
	}
	return debounce
}
