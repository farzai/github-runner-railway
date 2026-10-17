package ghapp

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/farzai/github-runner-railway/internal/reconcile"
)

// The second identical cycle sends If-None-Match with the ETag of the first answer, gets 304
// (which GitHub does not count against the rate limit) and still reports the same jobs.
func TestASecondIdenticalReadIsConditionalAndReusesTheBody(t *testing.T) {
	var conditional []string
	f := &fake{h: func(w http.ResponseWriter, r *http.Request) {
		etag := `"e-` + r.URL.Path + "-" + r.URL.Query().Get("status") + `"`
		if r.Header.Get("If-None-Match") == etag {
			conditional = append(conditional, r.URL.Path)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		switch {
		case strings.HasSuffix(r.URL.Path, "/actions/runs") && r.URL.Query().Get("status") == "queued":
			io.WriteString(w, `{"total_count":1,"workflow_runs":[{"id":11}]}`)
		case strings.HasSuffix(r.URL.Path, "/actions/runs"):
			io.WriteString(w, `{"total_count":0,"workflow_runs":[]}`)
		default:
			io.WriteString(w, `{"total_count":1,"jobs":[{"id":1,"status":"queued","labels":["self-hosted","railway"]}]}`)
		}
	}}
	c := newGH(t, f)
	first, err := c.QueuedJobs(context.Background(), repo)
	if err != nil || len(first) != 1 {
		t.Fatalf("first %v %v", first, err)
	}
	second, err := c.QueuedJobs(context.Background(), repo)
	if err != nil || len(second) != 1 || second[0].ID != first[0].ID {
		t.Fatalf("second %v %v, want the same job as the first", second, err)
	}
	if len(conditional) != 3 {
		t.Fatalf("304s for %v, want the two run lists and the jobs list", conditional)
	}
}

// Two repo scopes of one owner share the owner's installation: one access-token request.
func TestTwoRepoScopesOfOneInstallationShareOneToken(t *testing.T) {
	f := &fake{h: func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"runners":[]}`) }}
	c := newGH(t, f)
	other := reconcile.Scope{Kind: "repo", Owner: "acme", Repo: "api"}
	for _, s := range []reconcile.Scope{repo, other, repo, other} {
		if _, err := c.Runners(context.Background(), s); err != nil {
			t.Fatal(err)
		}
	}
	tokens := 0
	for _, r := range f.reqs {
		if strings.HasSuffix(r, "/access_tokens") {
			tokens++
		}
	}
	if tokens != 1 {
		t.Fatalf("token requests %d (%v), want 1", tokens, f.reqs)
	}
}

// The remaining requests GitHub reports are kept per installation for /healthz.
func TestTheRateLimitIsKeptPerInstallation(t *testing.T) {
	f := &fake{h: func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4321")
		w.Header().Set("X-RateLimit-Reset", "1760702400")
		io.WriteString(w, `{"runners":[]}`)
	}}
	c := newGH(t, f)
	if _, err := c.Runners(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	rl := c.RateLimits()
	got, ok := rl["acme"]
	if !ok || got.Remaining != 4321 || got.Limit != 5000 || got.Reset.Unix() != 1760702400 {
		t.Fatalf("rate limits %+v, want acme 4321/5000", rl)
	}
}

// A 304 on page 1 replays its cached Link header, so a list that grew past a full page without
// changing page 1's bytes would hide the new page, and a runner on it would look gone. Only answers
// that carry total_count (which changes when the list grows) are reused on a 304; any other GET
// is always read in full.
func TestOnlyAnswersWithATotalCountAreReadConditionally(t *testing.T) {
	var conditional int
	f := &fake{h: func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "" {
			conditional++
		}
		w.Header().Set("ETag", `"same"`)
		io.WriteString(w, `{"runners":[{"id":1,"name":"rr-a","status":"online"}]}`) // no total_count
	}}
	c := newGH(t, f)
	for i := 0; i < 2; i++ {
		if _, err := c.Runners(context.Background(), repo); err != nil {
			t.Fatal(err)
		}
	}
	if conditional != 0 {
		t.Fatalf("%d conditional requests for an answer without total_count, want 0", conditional)
	}
}
