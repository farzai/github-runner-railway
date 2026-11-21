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
	"sync/atomic"
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
	mu      sync.Mutex             // held for a whole cycle; guards the cycle state below, not status
	alerted map[int64]string       // job id -> scope key
	status  atomic.Pointer[Status] // the last cycle's outcome, replaced whole and never changed after

	failures    int                  // failed cycles in a row
	lastSuccess time.Time            // the end of the last cycle with no error
	scopeFails  map[string]int       // scope key -> cycles in a row it could not be read, a 404 aside
	missing     map[string]int       // scope key -> cycles in a row GitHub answered 404 for it
	goneAlerted map[string]bool      // scope keys whose missing alert was sent and not yet cleared
	failAlerted bool                 // the failing state has been alerted and not yet recovered
	lowWarn     map[string]time.Time // account -> the reset of the window already warned about
	pending     []string             // alerts of this cycle, sent once the lock is released
}

// RateLimiter is the optional part of GH that reports GitHub's rate limit per installation.
type RateLimiter interface {
	RateLimits() map[string]ghapp.RateLimit
}

// Status is the last cycle, for /healthz.
type Status struct {
	LastCycle           time.Time
	LastError           string
	Services            int
	Queued              int
	ConsecutiveFailures int
	LastSuccess         time.Time
	UnreadableScopes    int  // how many scopes the last cycle could not read; /healthz is public, so no names
	UnlistedJobs        int  // queued jobs the last cycle found only because GitHub's status filter left their run out
	UnlistedReadFailed  int  // repositories whose unfiltered run list the last cycle could not read; their listed jobs were still served
	Failing             bool // ConsecutiveFailures reached FailAlertAfter
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
		scopeFails: map[string]int{}, missing: map[string]int{}, goneAlerted: map[string]bool{}, lowWarn: map[string]time.Time{}}
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

// Status is the state after the last cycle. It never waits for a running cycle, so /healthz
// answers while a cycle is busy with GitHub and Railway.
func (c *Controller) Status() Status {
	if st := c.status.Load(); st != nil {
		return *st
	}
	return Status{}
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
// are neither counted nor reaped, since a guess about them could kill a busy runner. A scope
// GitHub answers 404 for (a deleted or renamed repo) is listed as unreadable, warned about and
// in time alerted, but does not fail the cycle unless no scope at all could be read; any other
// read error does.
func (c *Controller) Cycle(ctx context.Context) (err error) {
	// Alerts go out after the lock is released: a slow alert webhook must not hold up the next cycle.
	var alerts []string
	defer func() {
		for _, a := range alerts {
			c.cfg.Alert(a)
		}
	}()
	c.mu.Lock()
	defer func() {
		alerts, c.pending = c.pending, nil
		c.mu.Unlock()
	}()
	now := c.now()
	began, ghBefore, rwBefore := time.Now(), calls(c.gh), calls(c.rw)
	created, reaped := 0, 0
	var gap unlistedGap // set once the scopes are read; the log line below reads it when the cycle ends
	defer func() {
		c.cfg.Log.Info("cycle", "duration", time.Since(began).Round(time.Millisecond).String(), "github_calls", calls(c.gh)-ghBefore,
			"railway_calls", calls(c.rw)-rwBefore, "created", created, "reaped", reaped, "unlisted", gap.jobs,
			"unlisted_read_failed", gap.readFailed, "failed", err != nil)
	}()
	rd := c.readScopes(ctx)
	gap = unlistedGap{jobs: c.warnUnlisted(rd.jobs), readFailed: rd.unlistedReadFailed}
	errs, queued, runners, jobsOf, readable := rd.errs, rd.queued, rd.runners, rd.jobs, rd.readable
	all, err := c.rw.List(ctx)
	if err != nil {
		errs = append(errs, err)
		c.record(now, errs, 0, 0, gap)
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
		deleted, err := c.reap(ctx, r)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if deleted {
			reaped++
		}
	}
	createdBy := map[string]int{}
	for _, cr := range plan.Create {
		if err := c.create(ctx, scopeByKey(readable, cr.Scope)); err != nil {
			errs = append(errs, err)
			continue
		}
		createdBy[cr.Scope]++
		created++
	}
	c.alertLate(now, jobsOf, services, runners, createdBy)
	c.warnLowLimits()
	total := 0
	for _, n := range queued {
		total += n
	}
	c.record(now, errs, len(services)+len(plan.Create)-len(plan.Reap), total, gap)
	return errors.Join(errs...)
}

// scopeReads is what reading every scope in a cycle found.
type scopeReads struct {
	readable []reconcile.Scope
	queued   map[string]int
	runners  map[string][]reconcile.Runner
	jobs     map[string][]ghapp.Job
	errs     []error // the errors that fail the cycle

	unlistedReadFailed int // repositories whose fallback read failed; it does not fail the cycle
}

// unlistedGap is how the last cycle saw GitHub's status-filter gap: the jobs found only through
// the unfiltered run list, and the repositories whose unfiltered list could not be read.
type unlistedGap struct{ jobs, readFailed int }

// warnUnlisted logs each queued job found only because GitHub's status filter left its run out, so
// the gap on GitHub's side stays visible, and returns how many there were.
func (c *Controller) warnUnlisted(jobs map[string][]ghapp.Job) int {
	n := 0
	for _, js := range jobs {
		for _, j := range js {
			if j.Unlisted {
				n++
				c.cfg.Log.Warn("queued job missing from GitHub's status-filtered run list", "job", j.ID, "repo", j.Repo)
			}
		}
	}
	return n
}

// warnUnlistedRead logs each repository whose fallback read failed (repository and status only) and
// returns how many there were. The scope's listed jobs are still served: the fallback only adds jobs.
func (c *Controller) warnUnlistedRead(e *ghapp.UnlistedReadError) int {
	for _, f := range e.Failed {
		c.cfg.Log.Warn("unfiltered run list unreadable; serving the status-filtered jobs only", "repo", f.Repo, "status", f.Status)
	}
	return len(e.Failed)
}

// readScopes reads every scope, records how each went, and raises or clears the missing-scope alerts.
func (c *Controller) readScopes(ctx context.Context) scopeReads {
	rd := scopeReads{queued: map[string]int{}, runners: map[string][]reconcile.Runner{}, jobs: map[string][]ghapp.Job{}}
	for _, s := range c.cfg.Scopes {
		jobs, rs, err := c.readScope(ctx, s)
		var ue *ghapp.UnlistedReadError
		if errors.As(err, &ue) {
			rd.unlistedReadFailed += c.warnUnlistedRead(ue)
			err = nil
		}
		if fail := c.noteRead(s.Key(), err); fail != nil {
			rd.errs = append(rd.errs, fmt.Errorf("%s: %w", s.Key(), fail))
		}
		if err != nil {
			continue
		}
		rd.readable = append(rd.readable, s)
		rd.queued[s.Key()], rd.runners[s.Key()], rd.jobs[s.Key()] = len(jobs), rs, jobs
	}
	if fail := c.noneReadable(); fail != nil {
		rd.errs = append(rd.errs, fail)
		return rd
	}
	c.alertMissing(rd.readable)
	return rd
}

// noneReadable fails the cycle when every scope answered 404: all of them gone at once is far more
// likely the App uninstalled or its installation gone than repos deleted, and nothing at all can be
// served. The failing alert then names the scopes in its last error. When some scopes failed for
// another reason, those errors already fail the cycle and the 404 scopes are not blamed for it.
func (c *Controller) noneReadable() error {
	var gone []string
	for _, s := range c.cfg.Scopes {
		if c.missing[s.Key()] == 0 {
			return nil
		}
		gone = append(gone, s.Key())
	}
	if len(gone) == 0 {
		return nil
	}
	return fmt.Errorf("every scope not found on GitHub: %s (is the GitHub App still installed?)", strings.Join(gone, ", "))
}

// alertMissing raises one alert for a scope GitHub has answered 404 for FailAlertAfter cycles in a
// row, so a deleted repo left in RUNNER_SCOPES gets removed, and one message once it reads again.
func (c *Controller) alertMissing(readable []reconcile.Scope) {
	for _, s := range readable {
		if c.goneAlerted[s.Key()] {
			delete(c.goneAlerted, s.Key())
			c.pending = append(c.pending, "controller: scope "+s.Key()+" is readable again")
		}
	}
	for _, s := range c.cfg.Scopes {
		if n := c.missing[s.Key()]; n >= c.cfg.FailAlertAfter && !c.goneAlerted[s.Key()] {
			c.goneAlerted[s.Key()] = true
			c.pending = append(c.pending, fmt.Sprintf("controller: scope %s not found on GitHub for %d cycles in a row; "+
				"remove it from RUNNER_SCOPES, or check the GitHub App is installed on it", s.Key(), n))
		}
	}
}

// noteRead records how reading the scope went and returns the error that fails the cycle, if any.
// A 404 (a deleted repo, a typo in RUNNER_SCOPES, an App no longer installed) counts the scope
// missing and is warned about every cycle, but on its own is not an outage: it does not fail the
// cycle (noneReadable decides when every scope is gone) and does not count toward the failing
// alert's scopes unreadable that long.
func (c *Controller) noteRead(key string, err error) error {
	switch {
	case err == nil:
		delete(c.scopeFails, key)
		delete(c.missing, key)
		return nil
	case errors.Is(err, ghapp.ErrNotFound):
		delete(c.scopeFails, key)
		c.missing[key]++
		c.cfg.Log.Warn("scope not found on GitHub; skipped", "scope", key, "err", err.Error())
		return nil
	default:
		c.scopeFails[key]++
		delete(c.missing, key)
		return err
	}
}

// readScope reads the scope's queued jobs and its runners. When only the fallback read failed
// (an *ghapp.UnlistedReadError), the jobs and runners come with that error.
func (c *Controller) readScope(ctx context.Context, s reconcile.Scope) ([]ghapp.Job, []reconcile.Runner, error) {
	jobs, jobsErr := c.gh.QueuedJobs(ctx, s)
	var ue *ghapp.UnlistedReadError
	if jobsErr != nil && !errors.As(jobsErr, &ue) {
		return nil, nil, jobsErr
	}
	rs, err := c.gh.Runners(ctx, s)
	if err != nil {
		return nil, nil, err
	}
	return jobs, rs, jobsErr
}

// record keeps the cycle's outcome for /healthz and raises the failing and recovered alerts: one
// alert when FailAlertAfter cycles in a row have failed, naming the scopes unreadable for that long,
// and one message when a cycle succeeds again. A failed cycle still stamps LastCycle (the loop is
// alive); ConsecutiveFailures is what tells a failing controller from a working one.
func (c *Controller) record(now time.Time, errs []error, services, queued int, gap unlistedGap) {
	if len(errs) > 0 {
		c.failures++
	} else {
		c.failures = 0
		c.lastSuccess = now
	}
	unreadable, stuck := c.unreadableScopes()
	st := &Status{LastCycle: now, Services: services, Queued: queued, ConsecutiveFailures: c.failures,
		LastSuccess: c.lastSuccess, UnreadableScopes: unreadable, UnlistedJobs: gap.jobs,
		UnlistedReadFailed: gap.readFailed, Failing: c.failures >= c.cfg.FailAlertAfter}
	if len(errs) > 0 {
		st.LastError = errors.Join(errs...).Error()
	}
	c.status.Store(st)
	switch {
	case c.failures >= c.cfg.FailAlertAfter && !c.failAlerted:
		c.failAlerted = true
		msg := fmt.Sprintf("controller: %d cycles in a row failed", c.failures)
		if len(stuck) > 0 {
			msg += "; scopes unreadable that long: " + strings.Join(stuck, ", ")
		}
		c.pending = append(c.pending, msg+"; last error: "+truncate(st.LastError, 300))
	case c.failures == 0 && c.failAlerted:
		c.failAlerted = false
		c.pending = append(c.pending, "controller: recovered, the last cycle succeeded")
	}
}

// unreadableScopes counts the scopes the last cycle could not read (a 404 included), and names the
// ones that failed for FailAlertAfter cycles or more for another reason, for the operator's alert.
func (c *Controller) unreadableScopes() (unreadable int, stuck []string) {
	for _, s := range c.cfg.Scopes {
		n := c.scopeFails[s.Key()]
		if n > 0 || c.missing[s.Key()] > 0 {
			unreadable++
		}
		if n >= c.cfg.FailAlertAfter {
			stuck = append(stuck, s.Key())
		}
	}
	return unreadable, stuck
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// reap removes the service (and its runner first); deleted is false when the runner turned busy
// and the service was kept.
func (c *Controller) reap(ctx context.Context, r reconcile.Reap) (deleted bool, err error) {
	if r.RunnerID != 0 {
		sc := scopeByKey(c.cfg.Scopes, r.Scope)
		if err := c.gh.DeleteRunner(ctx, sc, r.RunnerID); err != nil {
			if errors.Is(err, ghapp.ErrBusy) && !r.Force {
				c.cfg.Log.Info("runner turned busy; kept", "service", r.Service.Name)
				return false, nil
			}
			if !errors.Is(err, ghapp.ErrBusy) {
				return false, fmt.Errorf("delete runner %s: %w", r.Service.Name, err)
			}
		}
	}
	c.cfg.Log.Info("reaping", "service", r.Service.Name, "reason", r.Reason)
	if err := c.rw.Delete(ctx, r.Service.ID); err != nil {
		return false, err
	}
	return true, nil
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
			c.pending = append(c.pending, fmt.Sprintf("job %d in %s has been queued %s with no runner coming", j.ID, j.Repo, now.Sub(j.CreatedAt).Round(time.Second)))
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
		body := map[string]any{"last_cycle": st.LastCycle, "services": st.Services, "queued": st.Queued,
			"error": st.LastError != "", "consecutive_failures": st.ConsecutiveFailures, "last_success": lastSuccess,
			"unreadable_scopes": st.UnreadableScopes, "unlisted_jobs": st.UnlistedJobs,
			"unlisted_read_failures": st.UnlistedReadFailed}
		if rl, ok := c.gh.(RateLimiter); ok {
			body["github_rate_limits"] = rl.RateLimits()
		}
		_ = json.NewEncoder(w).Encode(body)
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

// warnLowLimits logs once per rate-limit window an installation with less than a tenth of its
// GitHub requests left.
func (c *Controller) warnLowLimits() {
	rl, ok := c.gh.(RateLimiter)
	if !ok {
		return
	}
	for account, l := range rl.RateLimits() {
		if l.Limit <= 0 || l.Remaining*10 >= l.Limit || c.lowWarn[account].Equal(l.Reset) {
			continue
		}
		c.lowWarn[account] = l.Reset
		c.cfg.Log.Warn("github rate limit low", "account", account, "remaining", l.Remaining, "limit", l.Limit, "reset", l.Reset)
	}
}

// CallCounter is the optional part of GH and RW that counts the API calls made so far.
type CallCounter interface {
	Calls() int64
}

func calls(x any) int64 {
	if cc, ok := x.(CallCounter); ok {
		return cc.Calls()
	}
	return 0
}
