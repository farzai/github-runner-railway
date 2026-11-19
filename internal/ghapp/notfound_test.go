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
