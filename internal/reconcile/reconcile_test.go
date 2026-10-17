package reconcile

import (
	"testing"
	"time"
)

var now = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

var (
	org  = Scope{Kind: "org", Owner: "farzai"}
	repo = Scope{Kind: "repo", Owner: "parsilver", Repo: "skyabove"}
)

func params() Params {
	return Params{
		MaxRunners: 10, MaxCreatePerCycle: 5, StartupGrace: 3 * time.Minute, IdleTTL: 2 * time.Minute, MaxAge: 6 * time.Hour,
		Scopes: []Scope{org, repo},
	}
}

func svc(name string, s Scope, age time.Duration) Service {
	return Service{ID: "id-" + name, Name: name, ScopeHash: s.Hash(), CreatedAt: now.Add(-age)}
}

func plan(in Input) Plan {
	if in.Params.Scopes == nil {
		in.Params = params()
	}
	in.Now = now
	return Make(in)
}

func createsFor(p Plan, s Scope) int {
	n := 0
	for _, c := range p.Create {
		if c.Scope == s.Key() {
			n++
		}
	}
	return n
}

func reaped(p Plan, name string) (Reap, bool) {
	for _, r := range p.Reap {
		if r.Service.Name == name {
			return r, true
		}
	}
	return Reap{}, false
}

func TestOneRunnerPerQueuedJob(t *testing.T) {
	p := plan(Input{Queued: map[string]int{org.Key(): 3}})
	if createsFor(p, org) != 3 {
		t.Errorf("creates %+v, want 3 for the org", p.Create)
	}
}

func TestNothingQueuedNothingCreated(t *testing.T) {
	if p := plan(Input{}); len(p.Create) != 0 || len(p.Reap) != 0 {
		t.Errorf("plan %+v", p)
	}
}

func TestARunnerThatIsStartingOrIdleCoversAQueuedJob(t *testing.T) {
	in := Input{
		Queued:   map[string]int{org.Key(): 3},
		Services: []Service{svc("rr-a", org, 20*time.Second), svc("rr-b", org, 20*time.Second)},
		Runners:  map[string][]Runner{org.Key(): {{ID: 2, Name: "rr-b"}}}, // rr-a is still registering, rr-b is idle
	}
	if got := createsFor(plan(in), org); got != 1 {
		t.Errorf("creates %d, want 1: two runners already cover two of three jobs", got)
	}
}

func TestABusyRunnerDoesNotCoverAQueuedJob(t *testing.T) {
	in := Input{
		Queued:   map[string]int{org.Key(): 1},
		Services: []Service{svc("rr-a", org, time.Minute)},
		Runners:  map[string][]Runner{org.Key(): {{ID: 1, Name: "rr-a", Busy: true}}},
	}
	p := plan(in)
	if createsFor(p, org) != 1 {
		t.Errorf("creates %+v, want 1: the only runner is busy with another job", p.Create)
	}
	if _, gone := reaped(p, "rr-a"); gone {
		t.Error("a busy runner must never be reaped")
	}
}

func TestScopesAreCountedApart(t *testing.T) {
	in := Input{
		Queued:   map[string]int{org.Key(): 1, repo.Key(): 2},
		Services: []Service{svc("rr-a", org, time.Second)},
	}
	p := plan(in)
	if createsFor(p, org) != 0 || createsFor(p, repo) != 2 {
		t.Errorf("creates %+v: the org's runner cannot serve the repo's jobs", p.Create)
	}
}

func TestTheTotalIsCappedAndScopesShareTheRoomInTurn(t *testing.T) {
	pr := params()
	pr.MaxRunners = 4
	pr.MaxCreatePerCycle = 10
	in := Input{Params: pr, Queued: map[string]int{org.Key(): 10, repo.Key(): 10}, Services: []Service{svc("rr-a", org, time.Minute)}}
	p := plan(in)
	if len(p.Create) != 3 {
		t.Fatalf("creates %d, want 3: four allowed, one already there", len(p.Create))
	}
	if createsFor(p, org) == 0 || createsFor(p, repo) == 0 {
		t.Errorf("creates %+v: neither scope may starve the other", p.Create)
	}
}

func TestCreationPerCycleIsBounded(t *testing.T) {
	pr := params()
	pr.MaxCreatePerCycle = 2
	if p := plan(Input{Params: pr, Queued: map[string]int{org.Key(): 9}}); len(p.Create) != 2 {
		t.Errorf("creates %d, want 2: the Railway API allows 1,000 calls an hour", len(p.Create))
	}
}

func TestADeadServiceIsReapedNotCounted(t *testing.T) {
	in := Input{
		Queued:   map[string]int{org.Key(): 1},
		Services: []Service{svc("rr-old", org, 10*time.Minute), {ID: "id-x", Name: "rr-x", ScopeHash: org.Hash(), CreatedAt: now.Add(-time.Minute), Exited: true}},
	}
	p := plan(in)
	for _, n := range []string{"rr-old", "rr-x"} {
		r, ok := reaped(p, n)
		if !ok || r.RunnerID != 0 {
			t.Errorf("%s: reap %+v ok=%v, want reaped (never registered / container exited)", n, r, ok)
		}
	}
	if createsFor(p, org) != 1 {
		t.Errorf("creates %+v: dead services cover nothing", p.Create)
	}
}

func TestIdleRunnersBeyondTheDemandAreReapedAfterTheirTTLOldestFirst(t *testing.T) {
	in := Input{
		Queued:   map[string]int{org.Key(): 1},
		Services: []Service{svc("rr-a", org, 5*time.Minute), svc("rr-b", org, 4*time.Minute), svc("rr-c", org, 30*time.Second)},
		Runners:  map[string][]Runner{org.Key(): {{ID: 1, Name: "rr-a"}, {ID: 2, Name: "rr-b"}, {ID: 3, Name: "rr-c"}}},
	}
	p := plan(in)
	if r, ok := reaped(p, "rr-a"); !ok || r.RunnerID != 1 {
		t.Errorf("rr-a: %+v ok=%v, want reaped with its runner id (surplus, past the TTL)", r, ok)
	}
	if r, ok := reaped(p, "rr-b"); !ok || r.RunnerID != 2 {
		t.Errorf("rr-b: %+v ok=%v, want reaped too: three idle for one job leaves two surplus", r, ok)
	}
	if _, ok := reaped(p, "rr-c"); ok {
		t.Error("rr-c is the one kept for the queued job (and is inside its TTL)")
	}
	if len(p.Create) != 0 {
		t.Errorf("creates %+v", p.Create)
	}
}

func TestAServiceBeyondTheMaxAgeIsReapedEvenWhenBusy(t *testing.T) {
	in := Input{
		Services: []Service{svc("rr-a", org, 7*time.Hour)},
		Runners:  map[string][]Runner{org.Key(): {{ID: 1, Name: "rr-a", Busy: true}}},
	}
	r, ok := reaped(plan(in), "rr-a")
	if !ok || !r.Force {
		t.Errorf("reap %+v ok=%v, want a forced reap past the job timeout", r, ok)
	}
}

func TestAnExitedServiceOfAnUnknownScopeIsReapedAtOnce(t *testing.T) {
	stray := Service{ID: "id-s", Name: "rr-s", ScopeHash: "ffffff", CreatedAt: now.Add(-10 * time.Minute), Exited: true}
	if _, ok := reaped(plan(Input{Services: []Service{stray}}), "rr-s"); !ok {
		t.Error("an exited stray must go")
	}
}

func TestScopeHashIsStableAndShort(t *testing.T) {
	if org.Hash() != org.Hash() || len(org.Hash()) != 6 || org.Hash() == repo.Hash() {
		t.Errorf("hash %q %q", org.Hash(), repo.Hash())
	}
}

func TestARunnerThatIsOfflinePastTheGraceIsDeadAndReapedWithItsRunner(t *testing.T) {
	in := Input{
		Queued:   map[string]int{org.Key(): 2},
		Services: []Service{svc("rr-hung", org, 10*time.Minute), svc("rr-new", org, 30*time.Second)},
		Runners:  map[string][]Runner{org.Key(): {{ID: 7, Name: "rr-hung", Offline: true}, {ID: 8, Name: "rr-new", Offline: true}}},
	}
	p := plan(in)
	r, ok := reaped(p, "rr-hung")
	if !ok || r.RunnerID != 7 {
		t.Errorf("rr-hung: %+v ok=%v, want reaped with its runner: a JIT runner is registered at once and a hung container never goes online", r, ok)
	}
	if _, ok := reaped(p, "rr-new"); ok {
		t.Error("an offline runner inside the startup grace is still starting")
	}
	if createsFor(p, org) != 1 {
		t.Errorf("creates %+v: rr-new covers one of the two queued jobs, the hung one covers none", p.Create)
	}
}

func TestABusyRunnerThatTurnedOfflineIsLeftToTheMaxAge(t *testing.T) {
	in := Input{
		Services: []Service{svc("rr-a", org, 20*time.Minute)},
		Runners:  map[string][]Runner{org.Key(): {{ID: 1, Name: "rr-a", Busy: true, Offline: true}}},
	}
	if _, ok := reaped(plan(in), "rr-a"); ok {
		t.Error("a runner mid-job is not reaped before the job timeout")
	}
}

func TestAServiceOfAScopeNoLongerConfiguredIsKeptUntilItsJobCouldHaveEnded(t *testing.T) {
	stray := Service{ID: "id-s", Name: "rr-s", ScopeHash: "ffffff", CreatedAt: now.Add(-30 * time.Minute)}
	old := Service{ID: "id-o", Name: "rr-o", ScopeHash: "ffffff", CreatedAt: now.Add(-7 * time.Hour)}
	p := plan(Input{Services: []Service{stray, old}})
	if _, ok := reaped(p, "rr-s"); ok {
		t.Error("a service of a removed scope may be mid-job: it goes only past the job timeout")
	}
	if _, ok := reaped(p, "rr-o"); !ok {
		t.Error("past the job timeout it goes")
	}
}
