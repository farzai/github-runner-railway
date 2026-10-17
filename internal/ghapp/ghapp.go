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
	"strings"
	"sync"
	"time"

	"github.com/farzai/github-runner-railway/internal/reconcile"
)

// ErrBusy is GitHub refusing to delete a runner that is running a job.
var ErrBusy = errors.New("ghapp: runner is busy")

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
	cfg  Config
	key  *rsa.PrivateKey
	mu   sync.Mutex
	toks map[string]token // by owner, or owner/repo for a repo scope
}

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
	return &Client{cfg: cfg, key: key, toks: map[string]token{}}, nil
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
	res, err := c.cfg.HTTP.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("ghapp: %s %s: %w", method, req.URL.Path, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if res.StatusCode >= 300 {
		return res.Header, res.StatusCode, fmt.Errorf("ghapp: %s %s: HTTP %d", method, req.URL.Path, res.StatusCode)
	}
	if into != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, into); err != nil {
			return res.Header, res.StatusCode, fmt.Errorf("ghapp: %s %s: undecodable answer", method, req.URL.Path)
		}
	}
	return res.Header, res.StatusCode, nil
}

// installToken is an installation access token for the scope's owner, cached until a minute
// before it expires.
func (c *Client) installToken(ctx context.Context, s reconcile.Scope) (string, error) {
	key := s.Owner
	if s.Kind == "repo" {
		key = s.Owner + "/" + s.Repo
	}
	c.mu.Lock()
	t, ok := c.toks[key]
	c.mu.Unlock()
	if ok && time.Until(t.expires) > time.Minute {
		return t.value, nil
	}
	jwt, err := c.appJWT()
	if err != nil {
		return "", err
	}
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
	var tok struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if _, _, err := c.call(ctx, http.MethodPost, fmt.Sprintf("%s/app/installations/%d/access_tokens", c.cfg.BaseURL, inst.ID), jwt, nil, &tok); err != nil {
		return "", err
	}
	c.mu.Lock()
	c.toks[key] = token{tok.Token, tok.ExpiresAt}
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
	for i := 0; next != "" && i < 50; i++ {
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
					if u, err := url.Parse(part[lo+1 : hi]); err == nil && u.Host == base.Host {
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
	body := map[string]any{"name": name, "runner_group_id": group, "labels": []string{"self-hosted", c.cfg.Label}, "work_folder": "_work"}
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
			ID   int64  `json:"id"`
			Name string `json:"name"`
			Busy bool   `json:"busy"`
		} `json:"runners"`
	}
	err = c.each(ctx, c.cfg.BaseURL+scopePath(s)+"/actions/runners?per_page=100", tok, func() any { cur.Runners = nil; return &cur }, func() {
		for _, r := range cur.Runners {
			out = append(out, reconcile.Runner{ID: r.ID, Name: r.Name, Busy: r.Busy})
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
}

// QueuedJobs are the queued jobs with our label in the scope's repositories: every repository
// the installation holds under the org, or the one repository of a repo scope.
func (c *Client) QueuedJobs(ctx context.Context, s reconcile.Scope) ([]Job, error) {
	tok, err := c.installToken(ctx, s)
	if err != nil {
		return nil, err
	}
	var repos []string
	if s.Kind == "repo" {
		repos = []string{s.Owner + "/" + s.Repo}
	} else {
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
		if err != nil {
			return nil, err
		}
	}
	seen := map[int64]bool{}
	var out []Job
	for _, repo := range repos {
		for _, status := range []string{"queued", "in_progress"} {
			var runs struct {
				Runs []struct {
					ID int64 `json:"id"`
				} `json:"workflow_runs"`
			}
			if _, _, err := c.call(ctx, http.MethodGet, fmt.Sprintf("%s/repos/%s/actions/runs?status=%s&per_page=100", c.cfg.BaseURL, repo, status), tok, nil, &runs); err != nil {
				return nil, err
			}
			for _, run := range runs.Runs {
				var jobs struct {
					Jobs []struct {
						ID        int64     `json:"id"`
						Status    string    `json:"status"`
						Labels    []string  `json:"labels"`
						CreatedAt time.Time `json:"created_at"`
					} `json:"jobs"`
				}
				if _, _, err := c.call(ctx, http.MethodGet, fmt.Sprintf("%s/repos/%s/actions/runs/%d/jobs?filter=latest&per_page=100", c.cfg.BaseURL, repo, run.ID), tok, nil, &jobs); err != nil {
					return nil, err
				}
				for _, j := range jobs.Jobs {
					if j.Status != "queued" || seen[j.ID] || !hasLabel(j.Labels, c.cfg.Label) {
						continue
					}
					seen[j.ID] = true
					out = append(out, Job{ID: j.ID, Repo: repo, CreatedAt: j.CreatedAt})
				}
			}
		}
	}
	return out, nil
}

func hasLabel(ls []string, want string) bool {
	for _, l := range ls {
		if l == want {
			return true
		}
	}
	return false
}
