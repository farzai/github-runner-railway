package ghapp

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// unlistedFake is a GitHub whose status filter leaves run 31 out: only the unfiltered runs list
// shows it. Run 30 is a stale run the queued filter still lists but whose jobs are all done, and
// run 32 was cancelled.
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
			io.WriteString(w, `{"total_count":4,"workflow_runs":[{"id":33,"status":"completed"},{"id":32,"status":"completed"},{"id":31,"status":"queued"},{"id":30,"status":"queued"}]}`)
		case strings.HasSuffix(r.URL.Path, "/runs/30/jobs"):
			io.WriteString(w, `{"total_count":1,"jobs":[{"id":300,"status":"completed","labels":["self-hosted","railway"]}]}`)
		case strings.HasSuffix(r.URL.Path, "/runs/31/jobs"):
			io.WriteString(w, `{"total_count":2,"jobs":[{"id":310,"status":"queued","labels":["self-hosted","linux","x64","railway"],"created_at":"2026-11-21T18:38:55Z"},{"id":311,"status":"queued","labels":["ubuntu-latest"]}]}`)
		default:
			t.Errorf("unexpected %s", r.URL.RequestURI())
			w.WriteHeader(404)
		}
	}}
}

// The 2026-11-21 incident: GitHub's runs?status=queued left a queued run out (and still listed a
// stale one with nothing to run), so its job never got a runner. With no queued job found through
// the filtered lists, one page of the unfiltered list is read and the job is found, marked unlisted.
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

// When the filtered lists already found a job of ours, the unfiltered list is not read.
func TestTheUnfilteredListIsNotReadWhenAQueuedJobWasFound(t *testing.T) {
	f := &fake{h: func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/actions/runs") && r.URL.Query().Get("status") == "queued":
			io.WriteString(w, `{"workflow_runs":[{"id":11}]}`)
		case strings.HasSuffix(r.URL.Path, "/actions/runs") && r.URL.Query().Get("status") != "":
			io.WriteString(w, `{"workflow_runs":[]}`)
		case strings.HasSuffix(r.URL.Path, "/runs/11/jobs"):
			io.WriteString(w, `{"jobs":[{"id":1,"status":"queued","labels":["railway"]}]}`)
		default:
			t.Errorf("unexpected %s", r.URL.RequestURI())
			w.WriteHeader(404)
		}
	}}
	jobs, err := newGH(t, f).QueuedJobs(context.Background(), repo)
	if err != nil || len(jobs) != 1 || jobs[0].Unlisted {
		t.Fatalf("jobs %+v err %v, want job 1, listed", jobs, err)
	}
}

// A run that vanishes between the unfiltered list and its jobs read is skipped, like a listed one;
// any other error on the fallback fails the read.
func TestTheFallbackSkipsAVanishedRunAndFailsOnOtherErrors(t *testing.T) {
	for _, tc := range []struct {
		list, jobs int
		wantErr    bool
	}{{200, 404, false}, {200, 500, true}, {500, 200, true}} {
		f := &fake{h: func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/actions/runs") && r.URL.Query().Get("status") != "":
				io.WriteString(w, `{"workflow_runs":[]}`)
			case strings.HasSuffix(r.URL.Path, "/actions/runs"):
				w.WriteHeader(tc.list)
				io.WriteString(w, `{"workflow_runs":[{"id":31,"status":"queued"}]}`)
			default:
				w.WriteHeader(tc.jobs)
				io.WriteString(w, `{"jobs":[{"id":310,"status":"queued","labels":["railway"]}]}`)
			}
		}}
		jobs, err := newGH(t, f).QueuedJobs(context.Background(), repo)
		if (err != nil) != tc.wantErr || len(jobs) != 0 && tc.jobs != 200 {
			t.Errorf("list %d jobs %d: jobs %+v err %v, want error %v", tc.list, tc.jobs, jobs, err, tc.wantErr)
		}
	}
}
