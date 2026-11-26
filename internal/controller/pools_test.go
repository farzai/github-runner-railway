package controller

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/farzai/github-runner-railway/internal/ghapp"
	"github.com/farzai/github-runner-railway/internal/reconcile"
	"github.com/farzai/github-runner-railway/internal/webhook"
)

// Two pools on one controller: the base image under rr- and the Postgres image under rp-.
// "other" is a repository only the pg pool serves.
var other = reconcile.Scope{Kind: "repo", Owner: "acme", Repo: "db"}

func basePool() reconcile.Pool {
	return reconcile.Pool{Label: "railway", Image: "ghcr.io/acme/runner:1", Prefix: "rr-", MaxRunners: 4, Scopes: []reconcile.Scope{repo}}
}

func pgPool() reconcile.Pool {
	return reconcile.Pool{Label: "railway-pg", Image: "ghcr.io/acme/runner:pg-1", Prefix: "rp-", Region: "europe-west4", MaxRunners: 4, Scopes: []reconcile.Scope{repo, other}}
}

func newPooled(gh *fakeGH, rw *fakeRW, pools ...reconcile.Pool) *Controller {
	c := New(Config{
		Pools: pools, MaxCreatePerCycle: 5,
		StartupGrace: 3 * time.Minute, IdleTTL: 2 * time.Minute, MaxAge: 6 * time.Hour, AlertAfter: 5 * time.Minute,
	}, gh, rw)
	c.now = func() time.Time { return now }
	return c
}

func job(id int64, label string, at time.Time) ghapp.Job {
	return ghapp.Job{ID: id, Repo: "acme/app", Label: label, CreatedAt: at}
}

func TestOneCycleServesEveryPoolFromItsOwnImageAndPrefixWithOneScanPerScope(t *testing.T) {
	gh := &fakeGH{jobs: map[string][]ghapp.Job{repo.Key(): {job(1, "railway", now), job(2, "railway-pg", now)}}}
	rw := &fakeRW{}
	if err := newPooled(gh, rw, basePool(), pgPool()).Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(rw.created) != 2 {
		t.Fatalf("created %v, want one service per pool", rw.created)
	}
	byPrefix := map[string]string{}
	for _, name := range rw.created {
		byPrefix[name[:3]] = rw.images[name]
		if !strings.HasPrefix(name[3:], repo.Hash()+"-") {
			t.Errorf("name %q does not carry the scope hash after its prefix", name)
		}
	}
	if byPrefix["rr-"] != "ghcr.io/acme/runner:1" || byPrefix["rp-"] != "ghcr.io/acme/runner:pg-1" {
		t.Errorf("images by prefix %v: each pool's service must come from its own image", byPrefix)
	}
	for _, name := range rw.created {
		want := map[string]string{"rr-": "railway", "rp-": "railway-pg"}[name[:3]]
		if gh.labels[name] != want {
			t.Errorf("runner %s registered with label %q, want %q", name, gh.labels[name], want)
		}
		wantRegion := map[string]string{"rr-": "", "rp-": "europe-west4"}[name[:3]]
		if rw.regions[name] != wantRegion {
			t.Errorf("service %s in region %q, want %q", name, rw.regions[name], wantRegion)
		}
	}
	// repo is in both pools and read once; other is read once too
	if gh.reads[repo.Key()] != 1 || gh.reads[other.Key()] != 1 || gh.runnerReads[repo.Key()] != 1 || gh.runnerReads[other.Key()] != 1 {
		t.Errorf("job reads %v, runner reads %v: want one of each per scope per cycle", gh.reads, gh.runnerReads)
	}
}

func TestAPoolAtItsMaximumDoesNotBlockTheOtherPool(t *testing.T) {
	base := basePool()
	base.MaxRunners = 1
	busy := reconcile.Service{ID: "s1", Name: "rr-" + repo.Hash() + "-aaaaaa", ScopeHash: repo.Hash(), CreatedAt: now.Add(-10 * time.Minute)}
	gh := &fakeGH{
		jobs:    map[string][]ghapp.Job{repo.Key(): {job(1, "railway", now), job(2, "railway-pg", now)}},
		runners: map[string][]reconcile.Runner{repo.Key(): {{ID: 9, Name: busy.Name, Busy: true}}},
	}
	rw := &fakeRW{services: []reconcile.Service{busy}}
	if err := newPooled(gh, rw, base, pgPool()).Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(rw.created) != 1 || !strings.HasPrefix(rw.created[0], "rp-") {
		t.Fatalf("created %v: the full base pool makes nothing, the pg pool still gets its runner", rw.created)
	}
}

func TestAPoolNeverReapsOrCountsAnotherPoolsServices(t *testing.T) {
	// a busy pg runner in a scope only the pg pool serves
	pgBusy := reconcile.Service{ID: "p1", Name: "rp-" + other.Hash() + "-aaaaaa", ScopeHash: other.Hash(), CreatedAt: now.Add(-10 * time.Minute)}
	base := basePool()
	base.MaxRunners = 1
	gh := &fakeGH{
		jobs:    map[string][]ghapp.Job{repo.Key(): {job(1, "railway", now)}},
		runners: map[string][]reconcile.Runner{other.Key(): {{ID: 9, Name: pgBusy.Name, Busy: true}}},
	}
	rw := &fakeRW{services: []reconcile.Service{pgBusy}}
	if err := newPooled(gh, rw, base, pgPool()).Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(rw.deleted) != 0 {
		t.Errorf("deleted %v: the base pool must not reap a pg service", rw.deleted)
	}
	if len(rw.created) != 1 || !strings.HasPrefix(rw.created[0], "rr-") {
		t.Errorf("created %v: the pg service must not count toward the base pool's maximum", rw.created)
	}
}

func TestAnIdleRunnerOfOnePoolIsNoSupplyForTheOtherPool(t *testing.T) {
	idle := reconcile.Service{ID: "r1", Name: "rr-" + repo.Hash() + "-aaaaaa", ScopeHash: repo.Hash(), CreatedAt: now.Add(-time.Minute)}
	gh := &fakeGH{
		jobs:    map[string][]ghapp.Job{repo.Key(): {job(2, "railway-pg", now)}},
		runners: map[string][]reconcile.Runner{repo.Key(): {{ID: 9, Name: idle.Name}}},
	}
	rw := &fakeRW{services: []reconcile.Service{idle}}
	if err := newPooled(gh, rw, basePool(), pgPool()).Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(rw.created) != 1 || !strings.HasPrefix(rw.created[0], "rp-") {
		t.Errorf("created %v: a base runner cannot take a pg job", rw.created)
	}
}

func TestAnExitedServiceIsReapedOncePerCycle(t *testing.T) {
	gone := reconcile.Service{ID: "p1", Name: "rp-" + repo.Hash() + "-aaaaaa", ScopeHash: repo.Hash(), CreatedAt: now.Add(-10 * time.Minute), Exited: true}
	rw := &fakeRW{services: []reconcile.Service{gone}}
	if err := newPooled(&fakeGH{}, rw, basePool(), pgPool()).Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(rw.deleted) != 1 || rw.deleted[0] != "p1" {
		t.Errorf("deleted %v, want p1 once", rw.deleted)
	}
}

func TestAnUnreadableSharedScopeCountsOnceAndNoPoolTouchesItsServices(t *testing.T) {
	rr := reconcile.Service{ID: "r1", Name: "rr-" + repo.Hash() + "-aaaaaa", ScopeHash: repo.Hash(), CreatedAt: now.Add(-time.Hour), Exited: true}
	rp := reconcile.Service{ID: "p1", Name: "rp-" + repo.Hash() + "-aaaaaa", ScopeHash: repo.Hash(), CreatedAt: now.Add(-time.Hour), Exited: true}
	gh := &fakeGH{jobsErr: map[string]error{repo.Key(): errTest}}
	rw := &fakeRW{services: []reconcile.Service{rr, rp}}
	c := newPooled(gh, rw, basePool(), pgPool())
	_ = c.Cycle(context.Background())
	if len(rw.deleted) != 0 {
		t.Errorf("deleted %v: services of an unreadable scope are left alone", rw.deleted)
	}
	if got := c.Status().UnreadableScopes; got != 1 {
		t.Errorf("unreadable scopes %d, want 1: a scope in two pools is one scope", got)
	}
}

func TestALateJobIsAlertedOnceWhenTwoPoolsShareItsScope(t *testing.T) {
	gh := &fakeGH{jobs: map[string][]ghapp.Job{repo.Key(): {job(1, "railway", now.Add(-10*time.Minute))}}}
	rw := &fakeRW{createErr: errTest} // no runner can come
	var alerts []string
	c := newPooled(gh, rw, basePool(), pgPool())
	c.cfg.Alert = func(m string) { alerts = append(alerts, m) }
	for i := 0; i < 2; i++ {
		_ = c.Cycle(context.Background())
	}
	late := 0
	for _, a := range alerts {
		if strings.HasPrefix(a, "job 1 ") {
			late++
			if !strings.Contains(a, "railway") {
				t.Errorf("alert %q should name the pool label", a)
			}
		}
	}
	if late != 1 {
		t.Errorf("alerts %v: want the late job alerted once", alerts)
	}
}

func TestAnotherPoolsNewRunnerDoesNotHideALateJob(t *testing.T) {
	base := basePool()
	base.MaxRunners = 1
	busy := reconcile.Service{ID: "s1", Name: "rr-" + repo.Hash() + "-aaaaaa", ScopeHash: repo.Hash(), CreatedAt: now.Add(-10 * time.Minute)}
	gh := &fakeGH{
		jobs:    map[string][]ghapp.Job{repo.Key(): {job(1, "railway", now.Add(-10*time.Minute)), job(2, "railway-pg", now)}},
		runners: map[string][]reconcile.Runner{repo.Key(): {{ID: 9, Name: busy.Name, Busy: true}}},
	}
	rw := &fakeRW{services: []reconcile.Service{busy}}
	var alerts []string
	c := newPooled(gh, rw, base, pgPool())
	c.cfg.Alert = func(m string) { alerts = append(alerts, m) }
	_ = c.Cycle(context.Background())
	if len(rw.created) != 1 || len(alerts) != 1 || !strings.HasPrefix(alerts[0], "job 1 ") {
		t.Errorf("created %v alerts %v: the pg runner made this cycle is no runner for the base job", rw.created, alerts)
	}
}

func TestQueuedOnHealthIsTheSumAcrossPools(t *testing.T) {
	gh := &fakeGH{jobs: map[string][]ghapp.Job{
		repo.Key():  {job(1, "railway", now), job(2, "railway-pg", now)},
		other.Key(): {{ID: 3, Repo: "acme/db", Label: "railway-pg", CreatedAt: now}},
	}}
	c := newPooled(gh, &fakeRW{}, basePool(), pgPool())
	_ = c.Cycle(context.Background())
	if st := c.Status(); st.Queued != 3 || st.Services != 3 {
		t.Errorf("status %+v, want 3 queued and 3 services", st)
	}
}

func TestAQueuedWebhookForEitherPoolsLabelStartsACycle(t *testing.T) {
	for _, label := range []string{"railway", "Railway-PG"} {
		c := newPooled(&fakeGH{}, &fakeRW{}, basePool(), pgPool())
		h := webhook.Handler("s", []string{"railway", "railway-pg"}, c.OnEvent)
		body := `{"action":"queued","workflow_job":{"id":7,"labels":["self-hosted","` + label + `"]},"repository":{"full_name":"acme/app","owner":{"login":"acme"}}}`
		r := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
		r.Header.Set("X-GitHub-Event", "workflow_job")
		r.Header.Set("X-Hub-Signature-256", webhookSig("s", body))
		h.ServeHTTP(httptest.NewRecorder(), r)
		select {
		case <-c.wake:
		default:
			t.Errorf("label %s: a queued job did not wake the loop", label)
		}
	}
}

func TestAFailedReapFailsTheCycleAndIsNotCounted(t *testing.T) {
	gone := reconcile.Service{ID: "p1", Name: "rp-" + repo.Hash() + "-aaaaaa", ScopeHash: repo.Hash(), CreatedAt: now.Add(-10 * time.Minute), Exited: true}
	rw := &fakeRW{services: []reconcile.Service{gone}, deleteErr: errTest}
	c := newPooled(&fakeGH{}, rw, basePool(), pgPool())
	if err := c.Cycle(context.Background()); err == nil {
		t.Fatal("a reap Railway refused must fail the cycle")
	}
	if st := c.Status(); st.Services != 0 || st.LastError == "" {
		t.Errorf("status %+v", st)
	}
}

func TestTheCycleLineCountsWhatEveryPoolCreatedAndReaped(t *testing.T) {
	gh := &fakeGH{jobs: map[string][]ghapp.Job{repo.Key(): {job(1, "railway", now), job(2, "railway-pg", now)}}}
	rw := &fakeRW{services: []reconcile.Service{
		{ID: "r1", Name: "rr-" + repo.Hash() + "-aaaaaa", ScopeHash: repo.Hash(), CreatedAt: now.Add(-time.Hour), Exited: true},
		{ID: "p1", Name: "rp-" + repo.Hash() + "-aaaaaa", ScopeHash: repo.Hash(), CreatedAt: now.Add(-time.Hour), Exited: true},
	}}
	var buf bytes.Buffer
	c := newPooled(gh, rw, basePool(), pgPool())
	c.cfg.Log = slog.New(slog.NewTextHandler(&buf, nil))
	if err := c.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"created=2", "reaped=2", "pool=railway-pg"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("log %q lacks %s", buf.String(), want)
		}
	}
}
