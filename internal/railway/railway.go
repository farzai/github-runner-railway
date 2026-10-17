// Package railway is the small part of Railway's GraphQL API the controller uses: one
// service per runner, created with its JIT config, deployed once and deleted when done.
// A project token (Project-Access-Token header, not Bearer) reaches only its own project;
// it can delete a service only when the delete names the environment.
package railway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/farzai/github-runner-railway/internal/reconcile"
)

// Backboard sits behind Cloudflare, which refuses a bot user agent with error 1010.
const browserUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36"

// Config is the project and the image the runner services use.
type Config struct {
	URL           string // default https://backboard.railway.com/graphql/v2
	Token         string
	ProjectID     string
	EnvironmentID string
	Image         string
	Prefix        string // service names start with it, e.g. "rr-"
	HTTP          *http.Client
}

// Client talks to Railway.
type Client struct {
	cfg Config
}

// New builds a Client.
func New(cfg Config) *Client {
	if cfg.URL == "" {
		cfg.URL = "https://backboard.railway.com/graphql/v2"
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{cfg: cfg}
}

func (c *Client) do(ctx context.Context, query string, vars map[string]any, into any) error {
	body, _ := json.Marshal(map[string]any{"query": query, "variables": vars})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Project-Access-Token", c.cfg.Token)
	req.Header.Set("User-Agent", browserUA)
	res, err := c.cfg.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("railway: %w", scrub(err, c.cfg.Token))
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("railway: HTTP %d", res.StatusCode)
	}
	var env struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("railway: undecodable answer")
	}
	if len(env.Errors) > 0 {
		return fmt.Errorf("railway: %s", strings.ReplaceAll(env.Errors[0].Message, c.cfg.Token, "[redacted]"))
	}
	if into != nil {
		return json.Unmarshal(env.Data, into)
	}
	return nil
}

func scrub(err error, token string) error {
	if token == "" {
		return err
	}
	return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), token, "[redacted]"))
}

const listQuery = `query($p:String!){project(id:$p){services{edges{node{id name createdAt serviceInstances{edges{node{environmentId latestDeployment{status}}}}}}}}}`

// List is our runner services (by name prefix) with whether their container has ended.
func (c *Client) List(ctx context.Context) ([]reconcile.Service, error) {
	var d struct {
		Project struct {
			Services struct {
				Edges []struct {
					Node struct {
						ID               string    `json:"id"`
						Name             string    `json:"name"`
						CreatedAt        time.Time `json:"createdAt"`
						ServiceInstances struct {
							Edges []struct {
								Node struct {
									EnvironmentID    string `json:"environmentId"`
									LatestDeployment *struct {
										Status string `json:"status"`
									} `json:"latestDeployment"`
								} `json:"node"`
							} `json:"edges"`
						} `json:"serviceInstances"`
					} `json:"node"`
				} `json:"edges"`
			} `json:"services"`
		} `json:"project"`
	}
	if err := c.do(ctx, listQuery, map[string]any{"p": c.cfg.ProjectID}, &d); err != nil {
		return nil, err
	}
	var out []reconcile.Service
	for _, e := range d.Project.Services.Edges {
		n := e.Node
		if !strings.HasPrefix(n.Name, c.cfg.Prefix) {
			continue
		}
		s := reconcile.Service{ID: n.ID, Name: n.Name, CreatedAt: n.CreatedAt}
		if rest := strings.TrimPrefix(n.Name, c.cfg.Prefix); len(rest) >= 6 {
			s.ScopeHash = rest[:6]
		}
		for _, in := range n.ServiceInstances.Edges {
			if in.Node.EnvironmentID == c.cfg.EnvironmentID && in.Node.LatestDeployment != nil {
				switch in.Node.LatestDeployment.Status {
				case "FAILED", "CRASHED", "REMOVED", "REMOVING", "COMPLETED", "SKIPPED":
					s.Exited = true
				}
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// Create makes a runner service from the image with vars, never restarted, and deploys it
// once. A service that cannot be configured is deleted, not left half-made.
func (c *Client) Create(ctx context.Context, name string, vars map[string]string) (string, error) {
	var created struct {
		ServiceCreate struct {
			ID string `json:"id"`
		} `json:"serviceCreate"`
	}
	in := map[string]any{"projectId": c.cfg.ProjectID, "environmentId": c.cfg.EnvironmentID, "name": name,
		"source": map[string]any{"image": c.cfg.Image}, "variables": vars}
	if err := c.do(ctx, `mutation($i:ServiceCreateInput!){serviceCreate(input:$i){id name}}`, map[string]any{"i": in}, &created); err != nil {
		return "", err
	}
	id := created.ServiceCreate.ID
	fail := func(err error) (string, error) {
		_ = c.Delete(ctx, id)
		return "", err
	}
	if err := c.do(ctx, `mutation($s:String!,$e:String!,$i:ServiceInstanceUpdateInput!){serviceInstanceUpdate(serviceId:$s,environmentId:$e,input:$i)}`,
		map[string]any{"s": id, "e": c.cfg.EnvironmentID, "i": map[string]any{"restartPolicyType": "NEVER", "numReplicas": 1}}, nil); err != nil {
		return fail(err)
	}
	if err := c.do(ctx, `mutation($s:String!,$e:String!){serviceInstanceDeployV2(serviceId:$s,environmentId:$e)}`,
		map[string]any{"s": id, "e": c.cfg.EnvironmentID}, nil); err != nil {
		return fail(err)
	}
	return id, nil
}

// Delete removes a service from the environment.
func (c *Client) Delete(ctx context.Context, id string) error {
	return c.do(ctx, `mutation($i:String!,$e:String){serviceDelete(id:$i,environmentId:$e)}`, map[string]any{"i": id, "e": c.cfg.EnvironmentID}, nil)
}
