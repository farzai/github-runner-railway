// Package controller runs the loop: look at GitHub and Railway, ask reconcile for a plan,
// apply it. GitHub and Railway are the truth, read afresh each cycle; nothing is remembered
// between cycles except which late jobs were already alerted.
package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
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
	MinGap            time.Duration // at least this long between a wake-up and the next cycle
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
}

// Status is the last cycle, for /healthz.
type Status struct {
	LastCycle time.Time
	LastError string
	Services  int
	Queued    int
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
	if cfg.Interval <= 0 {
		cfg.Interval = 2 * time.Minute
	}
	return &Controller{cfg: cfg, gh: gh, rw: rw, now: time.Now, wake: make(chan struct{}, 1), alerted: map[int64]string{}}
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

// Run cycles until ctx ends: on every interval and on every nudge.
func (c *Controller) Run(ctx context.Context) {
	t := time.NewTicker(c.cfg.Interval)
	defer t.Stop()
	for {
		if err := c.Cycle(ctx); err != nil {
			c.cfg.Log.Error("cycle failed", "err", err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-c.wake:
			time.Sleep(c.cfg.MinGap) // let a burst of events land in one cycle, and bound the scan rate
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
			continue
		}
		rs, err := c.gh.Runners(ctx, s)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", s.Key(), err))
			continue
		}
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

func (c *Controller) record(now time.Time, errs []error, services, queued int) {
	c.status = Status{LastCycle: now, Services: services, Queued: queued}
	if len(errs) > 0 {
		c.status.LastError = errors.Join(errs...).Error()
	}
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
			if !registered && now.Sub(sv.CreatedAt) <= c.cfg.StartupGrace {
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
