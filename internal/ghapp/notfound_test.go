package ghapp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A deleted repository answers 404 to its runs listing while its installation id is still cached;
// the error says so with ErrNotFound, so the controller can tell it from an outage.
func TestARepoWhoseRunsAnswer404IsErrNotFound(t *testing.T) {
	f := &fake{h: func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"message":"Not Found"}`)
	}}
	c := newGH(t, f)
	_, err := c.QueuedJobs(context.Background(), repo)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err %v, want ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "/repos/acme/app/actions/runs") || !strings.Contains(err.Error(), "404") {
		t.Errorf("err %q, want the path and the status", err)
	}
}

// After a restart the installation lookup is the first call, and a deleted repository answers 404 there.
func TestARepoWhoseInstallationLookupAnswers404IsErrNotFound(t *testing.T) {
	pemKey, _ := testKey(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(ts.Close)
	c, err := New(Config{AppID: "123", PrivateKey: pemKey, BaseURL: ts.URL, Label: "railway"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Runners(context.Background(), repo); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err %v, want ErrNotFound", err)
	}
}

// Any other failure is not ErrNotFound: an outage must keep failing the cycle.
func TestOtherFailuresAreNotErrNotFound(t *testing.T) {
	for _, code := range []int{http.StatusInternalServerError, http.StatusForbidden, http.StatusUnauthorized, http.StatusTooManyRequests} {
		f := &fake{h: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }}
		c := newGH(t, f)
		_, err := c.QueuedJobs(context.Background(), repo)
		if err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("HTTP %d: err %v, want an error that is not ErrNotFound", code, err)
		}
	}
}

// In an org scope, a repository deleted between the repository listing and its runs answers 404:
// only that repository is skipped, so the org's other repositories keep their runners. A run
// deleted between the runs listing and its jobs is skipped the same way.
func TestAnOrgSkipsARepoOrRunThatVanishedMidRead(t *testing.T) {
	f := &fake{h: func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/installation/repositories":
			io.WriteString(w, `{"total_count":2,"repositories":[{"name":"gone","owner":{"login":"acme"}},{"name":"a","owner":{"login":"acme"}}]}`)
		case strings.HasPrefix(r.URL.Path, "/repos/acme/gone/"):
			w.WriteHeader(http.StatusNotFound)
		case strings.HasSuffix(r.URL.Path, "/actions/runs") && r.URL.Query().Get("status") == "queued":
			io.WriteString(w, `{"workflow_runs":[{"id":11},{"id":12}]}`)
		case strings.HasSuffix(r.URL.Path, "/actions/runs"):
			io.WriteString(w, `{"workflow_runs":[]}`)
		case strings.HasSuffix(r.URL.Path, "/runs/11/jobs"):
			w.WriteHeader(http.StatusNotFound)
		case strings.HasSuffix(r.URL.Path, "/runs/12/jobs"):
			io.WriteString(w, `{"jobs":[{"id":4,"status":"queued","labels":["railway"]}]}`)
		default:
			t.Errorf("unexpected %s", r.URL.RequestURI())
		}
	}}
	c := newGH(t, f)
	jobs, err := c.QueuedJobs(context.Background(), org)
	if err != nil {
		t.Fatalf("err %v, want the vanished repo and run skipped", err)
	}
	if len(jobs) != 1 || jobs[0].ID != 4 || jobs[0].Repo != "acme/a" {
		t.Errorf("jobs %+v, want job 4 of acme/a", jobs)
	}
}

// An org the App is no longer installed on answers 404 to the installation lookup.
func TestAnOrgWhoseInstallationLookupAnswers404IsErrNotFound(t *testing.T) {
	pemKey, _ := testKey(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/orgs/acme/installation" {
			t.Errorf("unexpected %s", r.URL.RequestURI())
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(ts.Close)
	c, err := New(Config{AppID: "123", PrivateKey: pemKey, BaseURL: ts.URL, Label: "railway"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.QueuedJobs(context.Background(), org); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err %v, want ErrNotFound", err)
	}
}

// An installation removed after its id was looked up answers 404 to the token request; the id is
// dropped, so the next read looks the installation up again.
func TestAnInstallationWhoseTokenRequestAnswers404IsErrNotFoundAndForgotten(t *testing.T) {
	pemKey, _ := testKey(t)
	var lookups int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			lookups++
			io.WriteString(w, `{"id":55}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(ts.Close)
	c, err := New(Config{AppID: "123", PrivateKey: pemKey, BaseURL: ts.URL, Label: "railway"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := c.Runners(context.Background(), repo); !errors.Is(err, ErrNotFound) {
			t.Fatalf("read %d: err %v, want ErrNotFound", i+1, err)
		}
	}
	if lookups != 2 {
		t.Errorf("%d installation lookups, want 2: the id must be forgotten after a 404", lookups)
	}
}
