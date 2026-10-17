package controller

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/farzai/github-runner-railway/internal/ghapp"
	"github.com/farzai/github-runner-railway/internal/reconcile"
	"github.com/farzai/github-runner-railway/internal/webhook"
)

var (
	org  = reconcile.Scope{Kind: "org", Owner: "farzai"}
	repo = reconcile.Scope{Kind: "repo", Owner: "parsilver", Repo: "skyabove"}
	now  = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
)

type fakeGH struct {
	mu      sync.Mutex
	jobs    map[string][]ghapp.Job
	runners map[string][]reconcile.Runner
	jobsErr map[string]error
	busy    map[int64]bool
	deleted []int64
	jit     []string
	nextID  int64
}

func (f *fakeGH) QueuedJobs(_ context.Context, s reconcile.Scope) ([]ghapp.Job, error) {
	return f.jobs[s.Key()], f.jobsErr[s.Key()]
}
func (f *fakeGH) Runners(_ context.Context, s reconcile.Scope) ([]reconcile.Runner, error) {
	return f.runners[s.Key()], nil
}
func (f *fakeGH) JITConfig(_ context.Context, s reconcile.Scope, name string) (string, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.jit = append(f.jit, s.Key()+" "+name)
	return "JIT-" + name, 1000 + f.nextID, nil
}
func (f *fakeGH) DeleteRunner(_ context.Context, _ reconcile.Scope, id int64) error {
	if f.busy[id] {
		return ghapp.ErrBusy
	}
	f.deleted = append(f.deleted, id)
	return nil
}

type fakeRW struct {
	services  []reconcile.Service
	created   []string
	vars      map[string]map[string]string
	deleted   []string
	createErr error
}

func (f *fakeRW) List(context.Context) ([]reconcile.Service, error) { return f.services, nil }
func (f *fakeRW) Create(_ context.Context, name string, vars map[string]string) (string, error) {
	if f.createErr != nil {
		return "", f.createErr
	}
	f.created = append(f.created, name)
	if f.vars == nil {
		f.vars = map[string]map[string]string{}
	}
	f.vars[name] = vars
	return "id-" + name, nil
}
func (f *fakeRW) Delete(_ context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}

func newC(gh *fakeGH, rw *fakeRW) *Controller {
	c := New(Config{
		Scopes: []reconcile.Scope{org, repo}, Prefix: "rr-", MaxRunners: 10, MaxCreatePerCycle: 5,
		StartupGrace: 3 * time.Minute, IdleTTL: 2 * time.Minute, MaxAge: 6 * time.Hour, AlertAfter: 5 * time.Minute,
	}, gh, rw)
	c.now = func() time.Time { return now }
	return c
}

func TestACycleCreatesARunnerServiceWithItsJITConfigForEachQueuedJob(t *testing.T) {
	gh := &fakeGH{jobs: map[string][]ghapp.Job{repo.Key(): {{ID: 1, Repo: "parsilver/skyabove", CreatedAt: now}, {ID: 2, Repo: "parsilver/skyabove", CreatedAt: now}}}}
	rw := &fakeRW{}
	if err := newC(gh, rw).Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(rw.created) != 2 {
		t.Fatalf("created %v", rw.created)
	}
	for _, name := range rw.created {
		if !strings.HasPrefix(name, "rr-"+repo.Hash()+"-") {
			t.Errorf("name %q does not carry the scope hash", name)
		}
		v := rw.vars[name]
		if v["ACTIONS_RUNNER_INPUT_JITCONFIG"] != "JIT-"+name {
			t.Errorf("vars %v: the runner needs its own JIT config", v)
		}
	}
	if rw.created[0] == rw.created[1] {
		t.Error("two services got the same name")
	}
}

func TestNothingQueuedCreatesNothing(t *testing.T) {
	gh, rw := &fakeGH{}, &fakeRW{}
	if err := newC(gh, rw).Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(rw.created)+len(rw.deleted)+len(gh.jit) != 0 {
		t.Errorf("acted: %v %v %v", rw.created, rw.deleted, gh.jit)
	}
}

func TestAServiceThatCannotBeCreatedHasItsRunnerRemoved(t *testing.T) {
	gh := &fakeGH{jobs: map[string][]ghapp.Job{org.Key(): {{ID: 1, CreatedAt: now}}}}
	rw := &fakeRW{createErr: errors.New("railway down")}
	c := newC(gh, rw)
	if err := c.Cycle(context.Background()); err == nil {
		t.Error("want the failure reported")
	}
	if len(gh.deleted) != 1 || gh.deleted[0] != 1001 {
		t.Errorf("deleted runners %v: a registered runner with no service must not be left", gh.deleted)
	}
}

func TestIfGitHubCannotBeReadForAScopeItsServicesAreLeftAlone(t *testing.T) {
	old := reconcile.Service{ID: "s1", Name: "rr-" + org.Hash() + "-a", ScopeHash: org.Hash(), CreatedAt: now.Add(-time.Hour)}
	gh := &fakeGH{jobsErr: map[string]error{org.Key(): errors.New("github 502")}}
	rw := &fakeRW{services: []reconcile.Service{old}}
	c := newC(gh, rw)
	_ = c.Cycle(context.Background())
	if len(rw.deleted) != 0 {
		t.Errorf("deleted %v: with no view of GitHub nothing may be reaped", rw.deleted)
	}
}

func TestAReapDeletesTheRunnerFirstAndSkipsTheServiceWhenTheRunnerIsBusy(t *testing.T) {
	idle := reconcile.Service{ID: "s-idle", Name: "rr-" + org.Hash() + "-i", ScopeHash: org.Hash(), CreatedAt: now.Add(-10 * time.Minute)}
	race := reconcile.Service{ID: "s-race", Name: "rr-" + org.Hash() + "-r", ScopeHash: org.Hash(), CreatedAt: now.Add(-11 * time.Minute)}
	gh := &fakeGH{
		runners: map[string][]reconcile.Runner{org.Key(): {{ID: 5, Name: idle.Name}, {ID: 6, Name: race.Name}}},
		busy:    map[int64]bool{6: true}, // picked up a job between the listing and the delete
	}
	rw := &fakeRW{services: []reconcile.Service{idle, race}}
	if err := newC(gh, rw).Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(rw.deleted) != 1 || rw.deleted[0] != "s-idle" {
		t.Errorf("deleted %v, want only s-idle: the runner that turned busy is kept because GitHub refuses to delete it", rw.deleted)
	}
}

func TestAJobQueuedTooLongWithoutARunnerRaisesOneAlert(t *testing.T) {
	gh := &fakeGH{jobs: map[string][]ghapp.Job{org.Key(): {{ID: 9, Repo: "farzai/a", CreatedAt: now.Add(-10 * time.Minute)}}}}
	rw := &fakeRW{createErr: errors.New("railway down")}
	var alerts []string
	c := newC(gh, rw)
	c.cfg.Alert = func(msg string) { alerts = append(alerts, msg) }
	_ = c.Cycle(context.Background())
	_ = c.Cycle(context.Background())
	if len(alerts) != 1 || !strings.Contains(alerts[0], "farzai/a") {
		t.Errorf("alerts %v, want exactly one naming the repository", alerts)
	}
}

func TestNoAlertWhileARunnerIsComing(t *testing.T) {
	gh := &fakeGH{jobs: map[string][]ghapp.Job{org.Key(): {{ID: 9, Repo: "farzai/a", CreatedAt: now.Add(-10 * time.Minute)}}}}
	rw := &fakeRW{services: []reconcile.Service{{ID: "s", Name: "rr-" + org.Hash() + "-a", ScopeHash: org.Hash(), CreatedAt: now.Add(-30 * time.Second)}}}
	var alerts []string
	c := newC(gh, rw)
	c.cfg.Alert = func(msg string) { alerts = append(alerts, msg) }
	_ = c.Cycle(context.Background())
	if len(alerts) != 0 {
		t.Errorf("alerts %v", alerts)
	}
}

func TestNudgeWakesTheLoopAndManyNudgesCollapse(t *testing.T) {
	c := newC(&fakeGH{}, &fakeRW{})
	for i := 0; i < 100; i++ {
		c.Nudge()
	}
	select {
	case <-c.wake:
	default:
		t.Fatal("no wake-up")
	}
	select {
	case <-c.wake:
		t.Error("nudges must collapse into one wake-up")
	default:
	}
}

func TestOnlyAQueuedEventWakesTheLoop(t *testing.T) {
	c := newC(&fakeGH{}, &fakeRW{})
	for _, action := range []string{"in_progress", "completed", "waiting"} {
		c.OnEvent(webhook.Event{Action: action})
	}
	select {
	case <-c.wake:
		t.Error("a non-queued event woke a full scan: with many repositories each scan costs dozens of API calls")
	default:
	}
	c.OnEvent(webhook.Event{Action: "queued"})
	select {
	case <-c.wake:
	default:
		t.Error("a queued event must wake the loop")
	}
}

func TestAnUnreadableScopeKeepsItsAlertMarksSoAlertsDoNotRepeat(t *testing.T) {
	gh := &fakeGH{jobs: map[string][]ghapp.Job{org.Key(): {{ID: 9, Repo: "farzai/a", CreatedAt: now.Add(-10 * time.Minute)}}}}
	rw := &fakeRW{createErr: errors.New("railway down")}
	var alerts []string
	c := newC(gh, rw)
	c.cfg.Alert = func(msg string) { alerts = append(alerts, msg) }
	_ = c.Cycle(context.Background())
	gh.jobsErr = map[string]error{org.Key(): errors.New("github 502")}
	_ = c.Cycle(context.Background())
	gh.jobsErr = nil
	_ = c.Cycle(context.Background())
	if len(alerts) != 1 {
		t.Errorf("alerts %v, want one: a cycle that could not read GitHub must not forget what it already alerted", alerts)
	}
}
