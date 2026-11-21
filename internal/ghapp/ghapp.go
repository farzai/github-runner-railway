// Package ghapp is the GitHub side: authentication as a GitHub App, the queue of jobs
// waiting for a runner, the runners that exist, and JIT runner configs. It uses the
// standard library only, so what it sends is readable in one file.
package ghapp

import (
	"bytes"
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
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/farzai/github-runner-railway/internal/reconcile"
)

// maxPages bounds a listing; more is an error, not a silently short answer.
const maxPages = 50

// ErrBusy is GitHub refusing to delete a runner that is running a job.
var ErrBusy = errors.New("ghapp: runner is busy")

// ErrNotFound is GitHub answering 404: the repository, organization or installation is not there
// (deleted, renamed, or the App no longer holds it).
var ErrNotFound = errors.New("ghapp: not found")

// Config is the App and the label this fleet serves.
type Config struct {
	AppID         string
	PrivateKey    string // PEM; a Railway variable may hold it with the newlines escaped
	BaseURL       string // default https://api.github.com
	Label         string
	RunnerGroupID int // the org runner group the runners join (repo scope always uses the default group, 1)
	HTTP          *http.Client
}

// Client talks to GitHub as the App.
type Client struct {
	calls  atomic.Int64 // API requests sent
	cfg    Config
	key    *rsa.PrivateKey
	mu     sync.Mutex
	insts  map[string]int64     // scope key -> installation id
	toks   map[int64]token      // installation id -> access token, shared by every scope of that installation
	owners map[string]string    // access token -> the account it belongs to, for the rate limit
	etags  map[string]cachedGet // URL -> the last answer, for conditional requests
	limits map[string]RateLimit // account -> what GitHub last reported
}

// cachedGet is a GET answer kept for If-None-Match: a 304 does not count against the rate limit.
type cachedGet struct {
	etag string
	body []byte
	hdr  http.Header
	used time.Time
}

// maxETags bounds the conditional-request cache; entries unused for an hour go first.
const maxETags = 2000

type token struct {
	value   string
	expires time.Time
}

// New parses the key and builds a Client.
func New(cfg Config) (*Client, error) {
	if cfg.AppID == "" {
		return nil, errors.New("ghapp: no app id")
	}
	pemText := strings.ReplaceAll(strings.TrimSpace(cfg.PrivateKey), `\n`, "\n")
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return nil, errors.New("ghapp: the private key is not PEM")
	}
	var key *rsa.PrivateKey
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		key = k
	} else if k8, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rk, ok := k8.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("ghapp: the private key is not RSA")
		}
		key = rk
	} else {
		return nil, errors.New("ghapp: the private key cannot be parsed")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.github.com"
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	// A redirect would carry the Authorization header to wherever it points: never follow one.
	// (on a copy: the caller's client, maybe http.DefaultClient, is not ours to change)
	hc := *cfg.HTTP
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	cfg.HTTP = &hc
	return &Client{cfg: cfg, key: key, insts: map[string]int64{}, toks: map[int64]token{}, owners: map[string]string{},
		etags: map[string]cachedGet{}, limits: map[string]RateLimit{}}, nil
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// appJWT is a ten-minute RS256 token that proves we are the App.
func (c *Client) appJWT() (string, error) {
	now := time.Now()
	head, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{"iss": c.cfg.AppID, "iat": now.Add(-time.Minute).Unix(), "exp": now.Add(9 * time.Minute).Unix()})
	signed := b64(head) + "." + b64(claims)
	h := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.key, crypto.SHA256, h[:])
	if err != nil {
		return "", err
	}
	return signed + "." + b64(sig), nil
}

func (c *Client) call(ctx context.Context, method, rawURL, bearer string, body any, into any) (http.Header, int, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rd)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "github-runner-railway")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	var cached cachedGet
	conditional := false
	if method == http.MethodGet {
		c.mu.Lock()
		cached, conditional = c.etags[rawURL]
		c.mu.Unlock()
		if conditional {
			req.Header.Set("If-None-Match", cached.etag)
		}
	}
	c.calls.Add(1)
	res, err := c.cfg.HTTP.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("ghapp: %s %s: %w", method, req.URL.Path, err)
	}
	defer res.Body.Close()
	c.noteLimit(bearer, res.Header)
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if res.StatusCode == http.StatusNotModified && conditional {
		c.mu.Lock()
		cached.used = time.Now()
		c.etags[rawURL] = cached
		c.mu.Unlock()
		if into != nil && len(cached.body) > 0 {
			if err := json.Unmarshal(cached.body, into); err != nil {
				return cached.hdr, http.StatusOK, fmt.Errorf("ghapp: %s %s: undecodable answer", method, req.URL.Path)
			}
		}
		return cached.hdr, http.StatusOK, nil // the cached Link header still names the next page
	}
	if res.StatusCode >= 300 {
		return res.Header, res.StatusCode, statusError(method, req.URL.Path, res.StatusCode)
	}
	if method == http.MethodGet {
		// Only a list answer with total_count is reused on a 304: total_count changes when the list
		// grows, so page 1 cannot stay byte-identical while a page beyond it appears (a 304 replays
		// the cached Link header, and a hidden page would make its runners look gone).
		if etag := res.Header.Get("ETag"); etag != "" && hasTotalCount(raw) {
			c.keep(rawURL, cachedGet{etag: etag, body: raw, hdr: res.Header.Clone(), used: time.Now()})
		}
	}
	if into != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, into); err != nil {
			return res.Header, res.StatusCode, fmt.Errorf("ghapp: %s %s: undecodable answer", method, req.URL.Path)
		}
	}
	return res.Header, res.StatusCode, nil
}

// statusError is the error for an answer that is not a success; a 404 wraps ErrNotFound.
func statusError(method, path string, code int) error {
	if code == http.StatusNotFound {
		return &httpError{code: code, err: fmt.Errorf("%w: %s %s: HTTP 404", ErrNotFound, method, path)}
	}
	return &httpError{code: code, err: fmt.Errorf("ghapp: %s %s: HTTP %d", method, path, code)}
}

// httpError is an answer that is not a success, keeping its status for the caller.
type httpError struct {
	code int
	err  error
}

func (e *httpError) Error() string { return e.err.Error() }
func (e *httpError) Unwrap() error { return e.err }

// statusOf is the HTTP status behind err, or 0 when GitHub gave no answer.
func statusOf(err error) int {
	var he *httpError
	if errors.As(err, &he) {
		return he.code
	}
	return 0
}

// keep stores a GET answer for conditional requests, pruning entries unused for an hour once the
// cache is full.
func (c *Client) keep(rawURL string, g cachedGet) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.etags) >= maxETags {
		for u, e := range c.etags {
			if time.Since(e.used) > time.Hour {
				delete(c.etags, u)
			}
		}
		if len(c.etags) >= maxETags {
			return
		}
	}
	c.etags[rawURL] = g
}

// noteLimit records the rate limit GitHub reports for the installation the token belongs to.
func (c *Client) noteLimit(bearer string, h http.Header) {
	rem, err1 := strconv.Atoi(h.Get("X-RateLimit-Remaining"))
	lim, err2 := strconv.Atoi(h.Get("X-RateLimit-Limit"))
	if err1 != nil || err2 != nil {
		return
	}
	reset, _ := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64)
	c.mu.Lock()
	defer c.mu.Unlock()
	if owner, ok := c.owners[bearer]; ok {
		c.limits[owner] = RateLimit{Limit: lim, Remaining: rem, Reset: time.Unix(reset, 0).UTC()}
	}
}

// installToken is an access token for the installation that holds the scope, shared by every
// scope of that installation and cached until a minute before it expires.
func (c *Client) installToken(ctx context.Context, s reconcile.Scope) (string, error) {
	c.mu.Lock()
	id, known := c.insts[s.Key()]
	t, ok := c.toks[id]
	c.mu.Unlock()
	if known && ok && time.Until(t.expires) > time.Minute {
		return t.value, nil
	}
	jwt, err := c.appJWT()
	if err != nil {
		return "", err
	}
	if !known {
		path := "/orgs/" + s.Owner + "/installation"
		if s.Kind == "repo" {
			path = "/repos/" + s.Owner + "/" + s.Repo + "/installation"
		}
		var inst struct {
			ID int64 `json:"id"`
		}
		if _, _, err := c.call(ctx, http.MethodGet, c.cfg.BaseURL+path, jwt, nil, &inst); err != nil {
			return "", err
		}
		id = inst.ID
		c.mu.Lock()
		c.insts[s.Key()] = id
		t, ok = c.toks[id]
		c.mu.Unlock()
		if ok && time.Until(t.expires) > time.Minute {
			return t.value, nil
		}
	}
	var tok struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if _, _, err := c.call(ctx, http.MethodPost, fmt.Sprintf("%s/app/installations/%d/access_tokens", c.cfg.BaseURL, id), jwt, nil, &tok); err != nil {
		c.mu.Lock()
		delete(c.insts, s.Key()) // the installation may have moved: look it up again next time
		c.mu.Unlock()
		return "", err
	}
	c.mu.Lock()
	if old, had := c.toks[id]; had {
		delete(c.owners, old.value)
	}
	c.toks[id] = token{tok.Token, tok.ExpiresAt}
	c.owners[tok.Token] = strings.ToLower(s.Owner)
	c.mu.Unlock()
	return tok.Token, nil
}

func scopePath(s reconcile.Scope) string {
	if s.Kind == "org" {
		return "/orgs/" + s.Owner
	}
	return "/repos/" + s.Owner + "/" + s.Repo
}

// each follows Link: rel="next" pages of a list endpoint, never leaving the API host.
func (c *Client) each(ctx context.Context, first, bearer string, page func() any, done func()) error {
	base, _ := url.Parse(c.cfg.BaseURL)
	next := first
	for i := 0; next != ""; i++ {
		if i >= maxPages {
			return fmt.Errorf("ghapp: more than %d pages of %s", maxPages, base.Host)
		}
		into := page()
		hdr, _, err := c.call(ctx, http.MethodGet, next, bearer, nil, into)
		if err != nil {
			return err
		}
		done()
		next = ""
		for _, part := range strings.Split(hdr.Get("Link"), ",") {
			if strings.Contains(part, `rel="next"`) {
				if lo, hi := strings.Index(part, "<"), strings.Index(part, ">"); lo >= 0 && hi > lo {
					if u, err := url.Parse(part[lo+1 : hi]); err == nil && u.Host == base.Host && u.Scheme == base.Scheme {
						next = u.String()
					}
				}
			}
		}
	}
	return nil
}

// JITConfig registers a one-job runner called name in the scope and returns its encoded
// JIT config and the runner's id.
func (c *Client) JITConfig(ctx context.Context, s reconcile.Scope, name string) (string, int64, error) {
	tok, err := c.installToken(ctx, s)
	if err != nil {
		return "", 0, err
	}
	group := 1
	if s.Kind == "org" && c.cfg.RunnerGroupID > 0 {
		group = c.cfg.RunnerGroupID
	}
	body := map[string]any{"name": name, "runner_group_id": group, "labels": []string{"self-hosted", "linux", "x64", c.cfg.Label}, "work_folder": "_work"}
	var out struct {
		Runner struct {
			ID int64 `json:"id"`
		} `json:"runner"`
		Encoded string `json:"encoded_jit_config"`
	}
	if _, _, err := c.call(ctx, http.MethodPost, c.cfg.BaseURL+scopePath(s)+"/actions/runners/generate-jitconfig", tok, body, &out); err != nil {
		return "", 0, err
	}
	if out.Encoded == "" {
		return "", 0, errors.New("ghapp: no JIT config in the answer")
	}
	return out.Encoded, out.Runner.ID, nil
}

// Runners lists the scope's registered runners.
func (c *Client) Runners(ctx context.Context, s reconcile.Scope) ([]reconcile.Runner, error) {
	tok, err := c.installToken(ctx, s)
	if err != nil {
		return nil, err
	}
	var out []reconcile.Runner
	var cur struct {
		Runners []struct {
			ID     int64  `json:"id"`
			Name   string `json:"name"`
			Busy   bool   `json:"busy"`
			Status string `json:"status"`
		} `json:"runners"`
	}
	err = c.each(ctx, c.cfg.BaseURL+scopePath(s)+"/actions/runners?per_page=100", tok, func() any { cur.Runners = nil; return &cur }, func() {
		for _, r := range cur.Runners {
			out = append(out, reconcile.Runner{ID: r.ID, Name: r.Name, Busy: r.Busy, Offline: r.Status == "offline"})
		}
	})
	return out, err
}

// DeleteRunner removes a runner; GitHub refuses (ErrBusy) while it runs a job.
func (c *Client) DeleteRunner(ctx context.Context, s reconcile.Scope, id int64) error {
	tok, err := c.installToken(ctx, s)
	if err != nil {
		return err
	}
	_, code, err := c.call(ctx, http.MethodDelete, fmt.Sprintf("%s%s/actions/runners/%d", c.cfg.BaseURL, scopePath(s), id), tok, nil, nil)
	if code == http.StatusUnprocessableEntity {
		return ErrBusy
	}
	if code == http.StatusNotFound {
		return nil // already gone
	}
	return err
}

// Job is a job waiting for a runner of ours.
type Job struct {
	ID        int64
	Repo      string
	CreatedAt time.Time
	Unlisted  bool // found only through the unfiltered runs list: GitHub's status filter left its run out
}

// UnlistedReadFailure is one repository whose fallback read failed, with the HTTP status (0: no answer).
type UnlistedReadFailure struct {
	Repo   string
	Status int
}

// UnlistedReadError is QueuedJobs' answer when every read but the fallback for runs the status
// filter left out worked. The fallback only adds jobs, so the jobs returned with it are what the
// scope served before the fallback existed: the caller serves them and logs the failures.
type UnlistedReadError struct {
	Failed []UnlistedReadFailure
}

func (e *UnlistedReadError) Error() string {
	parts := make([]string, len(e.Failed))
	for i, f := range e.Failed {
		parts[i] = fmt.Sprintf("%s (HTTP %d)", f.Repo, f.Status)
	}
	return "ghapp: the unfiltered run list could not be read for " + strings.Join(parts, ", ")
}

// QueuedJobs are the queued jobs with our label in the scope's repositories: every repository
// the installation holds under the org, or the one repository of a repo scope. A repo scope that
// answers 404 is ErrNotFound; in an org scope a repository that vanished after the listing is skipped.
// When only fallback reads failed, the jobs come with an *UnlistedReadError naming them.
func (c *Client) QueuedJobs(ctx context.Context, s reconcile.Scope) ([]Job, error) {
	tok, err := c.installToken(ctx, s)
	if err != nil {
		return nil, err
	}
	repos := []string{s.Owner + "/" + s.Repo}
	if s.Kind == "org" {
		if repos, err = c.orgRepos(ctx, s, tok); err != nil {
			return nil, err
		}
	}
	return c.reposJobs(ctx, s, repos, tok)
}

// reposJobs are the queued jobs of the scope's repositories, as QueuedJobs answers them.
func (c *Client) reposJobs(ctx context.Context, s reconcile.Scope, repos []string, tok string) ([]Job, error) {
	seen := map[int64]bool{}
	var out []Job
	var failed []UnlistedReadFailure
	for _, repo := range repos {
		jobs, err := c.repoJobs(ctx, repo, tok, seen)
		if errors.Is(err, ErrNotFound) && s.Kind == "org" {
			continue
		}
		if err != nil {
			return nil, err
		}
		unlisted, err := c.unlistedJobs(ctx, repo, tok, jobs.listed, seen)
		if err != nil {
			failed = append(failed, UnlistedReadFailure{Repo: repo, Status: statusOf(err)})
		}
		out = append(append(out, jobs.jobs...), unlisted...)
	}
	if failed != nil {
		return out, &UnlistedReadError{Failed: failed}
	}
	return out, nil
}

// orgRepos are the repositories of the org that the installation holds.
func (c *Client) orgRepos(ctx context.Context, s reconcile.Scope, tok string) ([]string, error) {
	var repos []string
	var cur struct {
		Repositories []struct {
			Name  string `json:"name"`
			Owner struct {
				Login string `json:"login"`
			} `json:"owner"`
		} `json:"repositories"`
	}
	err := c.each(ctx, c.cfg.BaseURL+"/installation/repositories?per_page=100", tok, func() any { cur.Repositories = nil; return &cur }, func() {
		for _, r := range cur.Repositories {
			if strings.EqualFold(r.Owner.Login, s.Owner) {
				repos = append(repos, r.Owner.Login+"/"+r.Name)
			}
		}
	})
	return repos, err
}

// listedJobs are what the status-filtered lists of one repository found: the queued jobs and every
// run they listed.
type listedJobs struct {
	jobs   []Job
	listed map[int64]bool
}

// repoJobs are the queued jobs with our label that the status-filtered lists of one repository
// find, skipping the ids in seen and adding its own. A run that vanished after the runs listing is
// skipped.
func (c *Client) repoJobs(ctx context.Context, repo, tok string, seen map[int64]bool) (listedJobs, error) {
	var out []Job
	listed := map[int64]bool{}
	// A run waiting to start reports "pending" (behind a concurrency group, say) while its
	// jobs are already queued, so pending runs are read too.
	for _, status := range []string{"queued", "pending", "in_progress"} {
		var runIDs []int64
		var runs struct {
			Runs []struct {
				ID int64 `json:"id"`
			} `json:"workflow_runs"`
		}
		err := c.each(ctx, fmt.Sprintf("%s/repos/%s/actions/runs?status=%s&per_page=100", c.cfg.BaseURL, repo, status), tok,
			func() any { runs.Runs = nil; return &runs }, func() {
				for _, r := range runs.Runs {
					runIDs = append(runIDs, r.ID)
					listed[r.ID] = true
				}
			})
		if err != nil {
			return listedJobs{}, err
		}
		jobs, err := c.jobsOfRuns(ctx, repo, runIDs, tok, seen)
		if err != nil {
			return listedJobs{}, err
		}
		out = append(out, jobs...)
	}
	return listedJobs{jobs: out, listed: listed}, nil
}

// unlistedJobs are the queued jobs of runs that GitHub's status filter left out of every listing
// (it did on 2026-11-21, for 25 minutes): it reads the first page of the unfiltered runs list, the
// newest runs, once with no paging, and the jobs of each run that is not completed and was not
// listed. A run pushed off that page by newer runs is never found here. It only adds jobs, so its
// error never costs the jobs the filtered lists found.
func (c *Client) unlistedJobs(ctx context.Context, repo, tok string, listed, seen map[int64]bool) ([]Job, error) {
	var runs struct {
		Runs []struct {
			ID     int64  `json:"id"`
			Status string `json:"status"`
		} `json:"workflow_runs"`
	}
	if _, _, err := c.call(ctx, http.MethodGet, fmt.Sprintf("%s/repos/%s/actions/runs?per_page=20", c.cfg.BaseURL, repo), tok, nil, &runs); err != nil {
		return nil, err
	}
	var runIDs []int64
	for _, r := range runs.Runs {
		if r.Status != "completed" && !listed[r.ID] {
			runIDs = append(runIDs, r.ID)
		}
	}
	jobs, err := c.jobsOfRuns(ctx, repo, runIDs, tok, seen)
	for i := range jobs {
		jobs[i].Unlisted = true
	}
	return jobs, err
}

// jobsOfRuns are the queued jobs with our label in the runs, skipping a run that vanished.
func (c *Client) jobsOfRuns(ctx context.Context, repo string, runIDs []int64, tok string, seen map[int64]bool) ([]Job, error) {
	var out []Job
	for _, id := range runIDs {
		jobs, err := c.runJobs(ctx, repo, id, tok, seen)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		out = append(out, jobs...)
	}
	return out, nil
}

// runJobs are the queued jobs with our label in one run, skipping the ids in seen and adding its own.
func (c *Client) runJobs(ctx context.Context, repo string, run int64, tok string, seen map[int64]bool) ([]Job, error) {
	var out []Job
	var jobs struct {
		Jobs []struct {
			ID        int64     `json:"id"`
			Status    string    `json:"status"`
			Labels    []string  `json:"labels"`
			CreatedAt time.Time `json:"created_at"`
		} `json:"jobs"`
	}
	err := c.each(ctx, fmt.Sprintf("%s/repos/%s/actions/runs/%d/jobs?filter=latest&per_page=100", c.cfg.BaseURL, repo, run), tok,
		func() any { jobs.Jobs = nil; return &jobs }, func() {
			for _, j := range jobs.Jobs {
				if j.Status != "queued" || seen[j.ID] || !c.canTake(j.Labels) {
					continue
				}
				seen[j.ID] = true
				out = append(out, Job{ID: j.ID, Repo: repo, CreatedAt: j.CreatedAt})
			}
		})
	return out, err
}

// canTake: a runner of ours has self-hosted, linux, x64 (GitHub adds the last two) and our
// label, so it can take a job only if every label the job asks for is one of those and ours is
// among them. Labels compare without case.
func (c *Client) canTake(jobLabels []string) bool {
	ours := false
	for _, l := range jobLabels {
		switch strings.ToLower(l) {
		case strings.ToLower(c.cfg.Label):
			ours = true
		case "self-hosted", "linux", "x64":
		default:
			return false
		}
	}
	return ours
}

// RateLimit is what GitHub last reported for one installation.
type RateLimit struct {
	Limit     int       `json:"limit"`
	Remaining int       `json:"remaining"`
	Reset     time.Time `json:"reset"`
}

// RateLimits is the last reported limit per installation account.
func (c *Client) RateLimits() map[string]RateLimit {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]RateLimit, len(c.limits))
	for k, v := range c.limits {
		out[k] = v
	}
	return out
}

func hasTotalCount(raw []byte) bool {
	var probe struct {
		TotalCount *int `json:"total_count"`
	}
	return json.Unmarshal(raw, &probe) == nil && probe.TotalCount != nil
}

// Calls is the number of API requests sent so far.
func (c *Client) Calls() int64 { return c.calls.Load() }
