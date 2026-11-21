package ghapp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// unlistedFake is a GitHub whose status filter leaves run 31 out: only the unfiltered runs list
// shows it. Run 30 is a stale run the queued filter still lists but whose jobs are all done, and
// run 32 was cancelled. Run 34 waits for an approval: not completed, but nothing of it is queued.
func unlistedFake(t *testing.T) *fake {
	return &fake{h: func(w http.ResponseWriter, r *http.Request) {
		status := r.URL.Query().Get("status")
		switch {
		case strings.HasSuffix(r.URL.Path, "/actions/runs") && status == "queued":
			io.WriteString(w, `{"total_count":1,"workflow_runs":[{"id":30,"status":"queued"}]}`)
		case strings.HasSuffix(r.URL.Path, "/actions/runs") && status != "":
			io.WriteString(w, `{"total_count":0,"workflow_runs":[]}`)
		case strings.HasSuffix(r.URL.Path, "/actions/runs"):
			if r.URL.Query().Get("per_page") != "20" {
				t.Errorf("unfiltered list %s, want one page of 20", r.URL.RequestURI())
			}
			io.WriteString(w, `{"total_count":5,"workflow_runs":[{"id":33,"status":"completed"},{"id":32,"status":"completed"},{"id":34,"status":"waiting"},{"id":31,"status":"queued"},{"id":30,"status":"queued"}]}`)
		case strings.HasSuffix(r.URL.Path, "/runs/30/jobs"):
			io.WriteString(w, `{"total_count":1,"jobs":[{"id":300,"status":"completed","labels":["self-hosted","railway"]}]}`)
		case strings.HasSuffix(r.URL.Path, "/runs/34/jobs"):
			io.WriteString(w, `{"total_count":1,"jobs":[{"id":340,"status":"waiting","labels":["railway"]}]}`)
		case strings.HasSuffix(r.URL.Path, "/runs/31/jobs"):
			io.WriteString(w, `{"total_count":2,"jobs":[{"id":310,"status":"queued","labels":["self-hosted","linux","x64","railway"],"created_at":"2026-11-21T18:38:55Z"},{"id":311,"status":"queued","labels":["ubuntu-latest"]}]}`)
		default:
			t.Errorf("unexpected %s", r.URL.RequestURI())
			w.WriteHeader(404)
		}
	}}
}

// The 2026-11-21 incident: GitHub's runs?status=queued left a queued run out (and still listed a
// stale one with nothing to run), so its job never got a runner. One page of the unfiltered list is
// read too, and the job is found, marked unlisted.
func TestAQueuedRunTheStatusFilterLeavesOutIsFoundThroughTheUnfilteredList(t *testing.T) {
	f := unlistedFake(t)
	jobs, err := newGH(t, f).QueuedJobs(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != 310 || !jobs[0].Unlisted || jobs[0].Repo != "acme/app" || jobs[0].CreatedAt.IsZero() {
		t.Fatalf("jobs %+v, want only job 310 of the unlisted run, marked unlisted", jobs)
	}
	for _, r := range f.reqs {
		if strings.Contains(r, "/runs/32/") || strings.Contains(r, "/runs/33/") {
			t.Errorf("request %q: a completed run has nothing to serve and must not be read", r)
		}
	}
	reads := 0
	for _, r := range f.reqs {
		if strings.Contains(r, "/runs/30/jobs") {
			reads++
		}
	}
	if reads != 1 {
		t.Errorf("run 30 jobs read %d times, want once: a run the filter already listed is not read again", reads)
	}
}

// The unfiltered page is read even when the filtered lists found a job of ours, so a run they left
// out gets its runner in the same cycle; the job they found is not counted again.
func TestTheUnfilteredListIsReadEvenWhenAQueuedJobWasFound(t *testing.T) {
	f := &fake{h: func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/actions/runs") && r.URL.Query().Get("status") == "queued":
			io.WriteString(w, `{"workflow_runs":[{"id":11,"status":"queued"}]}`)
		case strings.HasSuffix(r.URL.Path, "/actions/runs") && r.URL.Query().Get("status") != "":
			io.WriteString(w, `{"workflow_runs":[]}`)
		case strings.HasSuffix(r.URL.Path, "/actions/runs"):
			io.WriteString(w, `{"workflow_runs":[{"id":31,"status":"queued"},{"id":11,"status":"queued"}]}`)
		case strings.HasSuffix(r.URL.Path, "/runs/11/jobs"):
			io.WriteString(w, `{"jobs":[{"id":1,"status":"queued","labels":["railway"]}]}`)
		case strings.HasSuffix(r.URL.Path, "/runs/31/jobs"):
			io.WriteString(w, `{"jobs":[{"id":310,"status":"queued","labels":["railway"]}]}`)
		default:
			t.Errorf("unexpected %s", r.URL.RequestURI())
			w.WriteHeader(404)
		}
	}}
	jobs, err := newGH(t, f).QueuedJobs(context.Background(), repo)
	if err != nil || len(jobs) != 2 || jobs[0].ID != 1 || jobs[0].Unlisted || jobs[1].ID != 310 || !jobs[1].Unlisted {
		t.Fatalf("jobs %+v err %v, want job 1 listed and job 310 unlisted", jobs, err)
	}
}

// A run that vanishes between the unfiltered list and its jobs read is skipped, like a listed one.
// Any other failure of the fallback (a 5xx, a 403 rate limit, on the list or on an unlisted run's
// jobs) keeps the jobs the filtered lists found: it comes back as an UnlistedReadError naming the
// repository and the status, which the caller logs and counts, never as a failed scope.
func TestTheFallbackSkipsAVanishedRunAndKeepsTheListedJobsOnOtherErrors(t *testing.T) {
	for _, tc := range []struct {
		list, jobs int
		failed     int // the status the error names; 0 when the fallback worked
	}{{200, 404, 0}, {200, 500, 500}, {500, 200, 500}, {403, 200, 403}, {200, 403, 403}} {
		f := &fake{h: func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/actions/runs") && r.URL.Query().Get("status") == "queued":
				io.WriteString(w, `{"workflow_runs":[{"id":11}]}`)
			case strings.HasSuffix(r.URL.Path, "/actions/runs") && r.URL.Query().Get("status") != "":
				io.WriteString(w, `{"workflow_runs":[]}`)
			case strings.HasSuffix(r.URL.Path, "/actions/runs"):
				w.WriteHeader(tc.list)
				io.WriteString(w, `{"workflow_runs":[{"id":31,"status":"queued"},{"id":11,"status":"queued"}]}`)
			case strings.HasSuffix(r.URL.Path, "/runs/11/jobs"):
				io.WriteString(w, `{"jobs":[{"id":1,"status":"queued","labels":["railway"]}]}`)
			default:
				w.WriteHeader(tc.jobs)
				io.WriteString(w, `{"jobs":[{"id":310,"status":"queued","labels":["railway"]}]}`)
			}
		}}
		jobs, err := newGH(t, f).QueuedJobs(context.Background(), repo)
		if len(jobs) != 1 || jobs[0].ID != 1 || jobs[0].Unlisted {
			t.Errorf("list %d jobs %d: jobs %+v, want the listed job 1 kept and nothing else", tc.list, tc.jobs, jobs)
		}
		var ue *UnlistedReadError
		if tc.failed == 0 {
			if err != nil {
				t.Errorf("list %d jobs %d: err %v, want none: a vanished run is skipped", tc.list, tc.jobs, err)
			}
			continue
		}
		if !errors.As(err, &ue) || len(ue.Failed) != 1 || ue.Failed[0].Repo != "acme/app" || ue.Failed[0].Status != tc.failed {
			t.Errorf("list %d jobs %d: err %#v, want an UnlistedReadError naming acme/app and HTTP %d", tc.list, tc.jobs, err, tc.failed)
		}
	}
}

// A fallback that fails with no answer at all (the connection dropped) names status 0.
func TestAFallbackWithNoAnswerNamesStatusZero(t *testing.T) {
	f := &fake{h: func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/actions/runs") && r.URL.Query().Get("status") != "":
			io.WriteString(w, `{"workflow_runs":[]}`)
		default:
			hj, _ := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			conn.Close()
		}
	}}
	_, err := newGH(t, f).QueuedJobs(context.Background(), repo)
	var ue *UnlistedReadError
	if !errors.As(err, &ue) || len(ue.Failed) != 1 || ue.Failed[0].Status != 0 || !strings.Contains(ue.Error(), "acme/app") {
		t.Fatalf("err %#v, want an UnlistedReadError for acme/app with status 0", err)
	}
}

// A jobs read that fails on a run the filtered lists found fails the read, as it always has.
func TestAFailedJobsReadOfAListedRunFailsTheRead(t *testing.T) {
	f := &fake{h: func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/actions/runs") && r.URL.Query().Get("status") == "queued":
			io.WriteString(w, `{"workflow_runs":[{"id":11}]}`)
		case strings.HasSuffix(r.URL.Path, "/actions/runs"):
			io.WriteString(w, `{"workflow_runs":[]}`)
		default:
			w.WriteHeader(500)
		}
	}}
	if jobs, err := newGH(t, f).QueuedJobs(context.Background(), repo); err == nil {
		t.Fatalf("jobs %+v, want the 500 on the jobs read reported", jobs)
	}
}

// orgFallbackFake is an org of three repositories, each with one queued job in the filtered lists.
// The fallback read of acme/a answers 500 and that of acme/b 403; listC is what acme/c's filtered
// queued list answers.
func orgFallbackFake(t *testing.T, listC int) *fake {
	return &fake{h: func(w http.ResponseWriter, r *http.Request) {
		status := r.URL.Query().Get("status")
		switch {
		case r.URL.Path == "/installation/repositories":
			io.WriteString(w, `{"total_count":3,"repositories":[{"name":"a","owner":{"login":"acme"}},{"name":"b","owner":{"login":"acme"}},{"name":"c","owner":{"login":"acme"}}]}`)
		case r.URL.Path == "/repos/acme/c/actions/runs" && status == "queued" && listC != 200:
			w.WriteHeader(listC)
		case strings.HasSuffix(r.URL.Path, "/actions/runs") && status == "queued":
			io.WriteString(w, `{"workflow_runs":[{"id":11}]}`)
		case strings.HasSuffix(r.URL.Path, "/actions/runs") && status != "":
			io.WriteString(w, `{"workflow_runs":[]}`)
		case r.URL.Path == "/repos/acme/a/actions/runs":
			w.WriteHeader(500)
		case r.URL.Path == "/repos/acme/b/actions/runs":
			w.WriteHeader(403)
		case strings.HasSuffix(r.URL.Path, "/actions/runs"):
			io.WriteString(w, `{"workflow_runs":[]}`)
		case strings.HasSuffix(r.URL.Path, "/runs/11/jobs"):
			io.WriteString(w, `{"jobs":[{"id":1,"status":"queued","labels":["railway"]}]}`)
		default:
			t.Errorf("unexpected %s", r.URL.RequestURI())
		}
	}}
}

// In an org scope a failed fallback in one repository does not stop the others: every repository is
// still read, and every failed fallback is named in the one UnlistedReadError.
func TestAnOrgNamesEveryFailedFallbackAndKeepsEveryListedJob(t *testing.T) {
	jobs, err := newGH(t, orgFallbackFake(t, 200)).QueuedJobs(context.Background(), org)
	if len(jobs) != 1 || jobs[0].ID != 1 || jobs[0].Repo != "acme/a" {
		t.Errorf("jobs %+v, want job 1 once (the same id in every repository is one job)", jobs)
	}
	var ue *UnlistedReadError
	if !errors.As(err, &ue) || len(ue.Failed) != 2 ||
		ue.Failed[0] != (UnlistedReadFailure{Repo: "acme/a", Status: 500}) || ue.Failed[1] != (UnlistedReadFailure{Repo: "acme/b", Status: 403}) {
		t.Fatalf("err %#v, want acme/a 500 and acme/b 403 named", err)
	}
	if msg := ue.Error(); !strings.Contains(msg, "acme/a") || !strings.Contains(msg, "acme/b") || !strings.Contains(msg, "403") {
		t.Errorf("message %q, want both repositories and their status", msg)
	}
}

// A real read failure in a later repository still fails the scope, whatever fallbacks failed before it.
func TestAFailedFilteredListAfterAFailedFallbackStillFailsTheScope(t *testing.T) {
	jobs, err := newGH(t, orgFallbackFake(t, 500)).QueuedJobs(context.Background(), org)
	var ue *UnlistedReadError
	if err == nil || errors.As(err, &ue) || jobs != nil {
		t.Fatalf("jobs %+v err %v, want the 500 on acme/c's filtered list to fail the scope", jobs, err)
	}
}
