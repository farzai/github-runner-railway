package ghapp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/farzai/github-runner-railway/internal/reconcile"
)

func testKey(t *testing.T) (string, *rsa.PrivateKey) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})), k
}

type fake struct {
	mu     sync.Mutex
	reqs   []string
	auths  []string
	bodies []string
	h      func(w http.ResponseWriter, r *http.Request)
}

func newGH(t *testing.T, f *fake) *Client {
	t.Helper()
	pemKey, _ := testKey(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.reqs = append(f.reqs, r.Method+" "+r.URL.RequestURI())
		f.auths = append(f.auths, r.Header.Get("Authorization"))
		b, _ := io.ReadAll(r.Body)
		f.bodies = append(f.bodies, string(b))
		f.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			io.WriteString(w, `{"id":55}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			io.WriteString(w, `{"token":"inst-tok","expires_at":"2099-01-01T00:00:00Z"}`)
		default:
			f.h(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	c, err := New(Config{AppID: "123", PrivateKey: pemKey, BaseURL: ts.URL, Label: "railway-farzai", RunnerGroupID: 7})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var org = reconcile.Scope{Kind: "org", Owner: "farzai"}
var repo = reconcile.Scope{Kind: "repo", Owner: "parsilver", Repo: "skyabove"}

func TestAppJWTIsSignedWithTheKeyAndShortLived(t *testing.T) {
	pemKey, key := testKey(t)
	c, err := New(Config{AppID: "123", PrivateKey: pemKey, BaseURL: "http://x", Label: "l"})
	if err != nil {
		t.Fatal(err)
	}
	jwt, err := c.appJWT()
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt %q", jwt)
	}
	var claims struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	json.Unmarshal(raw, &claims)
	if claims.Iss != "123" || claims.Exp-claims.Iat > 11*60+60 {
		t.Errorf("claims %+v", claims)
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	if err := verifyRS256(&key.PublicKey, parts[0]+"."+parts[1], sig); err != nil {
		t.Errorf("signature: %v", err)
	}
}

func TestANewClientRefusesAMissingOrBrokenKey(t *testing.T) {
	if _, err := New(Config{AppID: "1", PrivateKey: "", Label: "l"}); err == nil {
		t.Error("empty key")
	}
	if _, err := New(Config{AppID: "", PrivateKey: "x", Label: "l"}); err == nil {
		t.Error("no app id")
	}
	if _, err := New(Config{AppID: "1", PrivateKey: "not a pem", Label: "l"}); err == nil {
		t.Error("garbage key")
	}
}

func TestAPEMWithEscapedNewlinesIsAccepted(t *testing.T) {
	pemKey, _ := testKey(t)
	if _, err := New(Config{AppID: "1", PrivateKey: strings.ReplaceAll(pemKey, "\n", `\n`), Label: "l"}); err != nil {
		t.Errorf("a key pasted into a Railway variable keeps its newlines escaped: %v", err)
	}
}

func TestJITConfigForAnOrgUsesTheGroupAndLabelsAndTheInstallationToken(t *testing.T) {
	f := &fake{h: func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"runner":{"id":9},"encoded_jit_config":"ENC"}`)
	}}
	c := newGH(t, f)
	enc, id, err := c.JITConfig(context.Background(), org, "rr-abc-1")
	if err != nil || enc != "ENC" || id != 9 {
		t.Fatalf("enc %q id %d err %v", enc, id, err)
	}
	last := len(f.reqs) - 1
	if f.reqs[last] != "POST /orgs/farzai/actions/runners/generate-jitconfig" || f.auths[last] != "Bearer inst-tok" {
		t.Errorf("request %q auth %q", f.reqs[last], f.auths[last])
	}
	var body struct {
		Name          string   `json:"name"`
		RunnerGroupID int      `json:"runner_group_id"`
		Labels        []string `json:"labels"`
	}
	json.Unmarshal([]byte(f.bodies[last]), &body)
	if body.Name != "rr-abc-1" || body.RunnerGroupID != 7 || strings.Join(body.Labels, ",") != "self-hosted,railway-farzai" {
		t.Errorf("body %+v", body)
	}
}

func TestJITConfigForARepoUsesTheRepoEndpointAndTheDefaultGroup(t *testing.T) {
	f := &fake{h: func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"encoded_jit_config":"E2"}`) }}
	c := newGH(t, f)
	if _, _, err := c.JITConfig(context.Background(), repo, "rr-x"); err != nil {
		t.Fatal(err)
	}
	last := len(f.reqs) - 1
	if f.reqs[last] != "POST /repos/parsilver/skyabove/actions/runners/generate-jitconfig" {
		t.Errorf("request %q", f.reqs[last])
	}
	if !strings.Contains(f.bodies[last], `"runner_group_id":1`) {
		t.Errorf("body %s: a repository's runner is in the default group", f.bodies[last])
	}
}

func TestInstallationTokensAreCachedPerOwner(t *testing.T) {
	f := &fake{h: func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"total_count":0,"runners":[]}`) }}
	c := newGH(t, f)
	for i := 0; i < 3; i++ {
		if _, err := c.Runners(context.Background(), org); err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	for _, r := range f.reqs {
		if strings.HasSuffix(r, "/access_tokens") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d token requests, want 1", n)
	}
}

func TestRunnersAreListedAcrossPages(t *testing.T) {
	f := &fake{h: func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			io.WriteString(w, `{"total_count":3,"runners":[{"id":3,"name":"c","busy":true}]}`)
			return
		}
		w.Header().Set("Link", `<`+"http://"+r.Host+r.URL.Path+`?per_page=100&page=2>; rel="next"`)
		io.WriteString(w, `{"total_count":3,"runners":[{"id":1,"name":"a","busy":false},{"id":2,"name":"b","busy":false}]}`)
	}}
	c := newGH(t, f)
	rs, err := c.Runners(context.Background(), repo)
	if err != nil || len(rs) != 3 || !rs[2].Busy {
		t.Errorf("runners %+v err %v", rs, err)
	}
}

func TestDeletingABusyRunnerIsErrBusy(t *testing.T) {
	f := &fake{h: func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		io.WriteString(w, `{"message":"Bad request - Runner is busy"}`)
	}}
	c := newGH(t, f)
	if err := c.DeleteRunner(context.Background(), org, 4); !errors.Is(err, ErrBusy) {
		t.Errorf("err %v, want ErrBusy", err)
	}
}

func TestQueuedJobsCountOnlyQueuedJobsWithOurLabelInTheScopesRepos(t *testing.T) {
	f := &fake{h: func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/installation/repositories":
			io.WriteString(w, `{"total_count":2,"repositories":[{"name":"a","owner":{"login":"farzai"}},{"name":"other","owner":{"login":"someone"}}]}`)
		case strings.HasSuffix(r.URL.Path, "/actions/runs") && r.URL.Query().Get("status") == "queued":
			io.WriteString(w, `{"workflow_runs":[{"id":11}]}`)
		case strings.HasSuffix(r.URL.Path, "/actions/runs"):
			io.WriteString(w, `{"workflow_runs":[{"id":12}]}`)
		case strings.HasSuffix(r.URL.Path, "/runs/11/jobs"):
			io.WriteString(w, `{"jobs":[{"id":1,"status":"queued","labels":["self-hosted","railway-farzai"],"created_at":"2026-10-17T11:00:00Z"},{"id":2,"status":"queued","labels":["ubuntu-latest"]},{"id":3,"status":"in_progress","labels":["railway-farzai"]}]}`)
		case strings.HasSuffix(r.URL.Path, "/runs/12/jobs"):
			io.WriteString(w, `{"jobs":[{"id":4,"status":"queued","labels":["railway-farzai"],"created_at":"2026-10-17T11:02:00Z"}]}`)
		default:
			t.Errorf("unexpected %s", r.URL.RequestURI())
			w.WriteHeader(404)
		}
	}}
	c := newGH(t, f)
	jobs, err := c.QueuedJobs(context.Background(), org)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 || jobs[0].ID != 1 || jobs[1].ID != 4 || jobs[0].Repo != "farzai/a" {
		t.Errorf("jobs %+v, want jobs 1 and 4 of farzai/a only", jobs)
	}
	for _, r := range f.reqs {
		if strings.Contains(r, "someone") {
			t.Errorf("request %q: another owner's repository must not be read", r)
		}
	}
}

func TestQueuedJobsOfARepoScopeLookOnlyAtThatRepo(t *testing.T) {
	f := &fake{h: func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/actions/runs"):
			io.WriteString(w, `{"workflow_runs":[]}`)
		default:
			t.Errorf("unexpected %s", r.URL.RequestURI())
		}
	}}
	c := newGH(t, f)
	if _, err := c.QueuedJobs(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	for _, r := range f.reqs {
		if strings.Contains(r, "/installation/repositories") {
			t.Errorf("request %q: a repo scope needs no repository listing", r)
		}
	}
}

func verifyRS256(pub *rsa.PublicKey, signed string, sig []byte) error {
	h := sha256.Sum256([]byte(signed))
	return rsa.VerifyPKCS1v15(pub, crypto.SHA256, h[:], sig)
}
