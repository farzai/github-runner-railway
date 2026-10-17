package railway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type call struct {
	Query string
	Vars  map[string]any
}

func server(t *testing.T, reply func(c call) string) (*Client, *[]call, *http.Header) {
	t.Helper()
	var calls []call
	var hdr http.Header
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr = r.Header.Clone()
		b, _ := io.ReadAll(r.Body)
		var c call
		json.Unmarshal(b, &struct {
			Q *string         `json:"query"`
			V *map[string]any `json:"variables"`
		}{&c.Query, &c.Vars})
		calls = append(calls, c)
		io.WriteString(w, reply(c))
	}))
	t.Cleanup(ts.Close)
	return New(Config{URL: ts.URL, Token: "tok-secret", ProjectID: "P", EnvironmentID: "E", Image: "ghcr.io/farzai/runner:1", Prefix: "rr-"}), &calls, &hdr
}

func TestListReturnsOnlyOurServicesWithTheirState(t *testing.T) {
	c, _, hdr := server(t, func(call) string {
		return `{"data":{"project":{"services":{"edges":[
		 {"node":{"id":"1","name":"rr-abc123-x1","createdAt":"2026-10-17T11:00:00Z","serviceInstances":{"edges":[{"node":{"environmentId":"E","latestDeployment":{"status":"SUCCESS"}}}]}}},
		 {"node":{"id":"2","name":"rr-abc123-x2","createdAt":"2026-10-17T11:01:00Z","serviceInstances":{"edges":[{"node":{"environmentId":"E","latestDeployment":{"status":"CRASHED"}}}]}}},
		 {"node":{"id":"3","name":"controller","createdAt":"2026-10-17T10:00:00Z","serviceInstances":{"edges":[]}}}
		]}}}}`
	})
	got, err := c.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ScopeHash != "abc123" || got[0].Exited || !got[1].Exited || !got[0].CreatedAt.Equal(time.Date(2026, 10, 17, 11, 0, 0, 0, time.UTC)) {
		t.Errorf("services %+v", got)
	}
	if hdr.Get("Project-Access-Token") != "tok-secret" || strings.Contains(hdr.Get("User-Agent"), "Go-http") || hdr.Get("Authorization") != "" {
		t.Errorf("headers %v: the project token goes in Project-Access-Token with a browser user agent", *hdr)
	}
}

func TestCreateMakesTheServiceWithItsConfigThenDeploysOnce(t *testing.T) {
	c, calls, _ := server(t, func(c call) string {
		switch {
		case strings.Contains(c.Query, "serviceCreate"):
			return `{"data":{"serviceCreate":{"id":"svc-9","name":"rr-abc123-q"}}}`
		default:
			return `{"data":{"ok":true}}`
		}
	})
	id, err := c.Create(context.Background(), "rr-abc123-q", map[string]string{"ACTIONS_RUNNER_INPUT_JITCONFIG": "JIT"})
	if err != nil || id != "svc-9" {
		t.Fatalf("id %q err %v", id, err)
	}
	if len(*calls) != 3 {
		t.Fatalf("%d calls, want create, configure, deploy: %+v", len(*calls), *calls)
	}
	in := (*calls)[0].Vars["i"].(map[string]any)
	if in["projectId"] != "P" || in["environmentId"] != "E" || in["name"] != "rr-abc123-q" || in["source"].(map[string]any)["image"] != "ghcr.io/farzai/runner:1" {
		t.Errorf("create input %+v", in)
	}
	if in["variables"].(map[string]any)["ACTIONS_RUNNER_INPUT_JITCONFIG"] != "JIT" {
		t.Errorf("the JIT config must travel as a variable: %+v", in["variables"])
	}
	cfg := (*calls)[1].Vars["i"].(map[string]any)
	if cfg["restartPolicyType"] != "NEVER" {
		t.Errorf("restart policy %v, want NEVER so a finished runner is not restarted", cfg["restartPolicyType"])
	}
	if !strings.Contains((*calls)[2].Query, "serviceInstanceDeployV2") {
		t.Errorf("last call %q, want the deploy", (*calls)[2].Query)
	}
}

func TestADeployAfterAFailedConfigureIsNotMadeAndTheServiceIsRemoved(t *testing.T) {
	c, calls, _ := server(t, func(c call) string {
		switch {
		case strings.Contains(c.Query, "serviceCreate"):
			return `{"data":{"serviceCreate":{"id":"svc-9","name":"n"}}}`
		case strings.Contains(c.Query, "serviceInstanceUpdate"):
			return `{"errors":[{"message":"boom"}]}`
		}
		return `{"data":{"serviceDelete":true}}`
	})
	if _, err := c.Create(context.Background(), "n", nil); err == nil {
		t.Fatal("want an error")
	}
	last := (*calls)[len(*calls)-1].Query
	if !strings.Contains(last, "serviceDelete") {
		t.Errorf("last call %q: a half-made service must not be left behind", last)
	}
}

func TestDeleteNamesTheEnvironment(t *testing.T) {
	c, calls, _ := server(t, func(call) string { return `{"data":{"serviceDelete":true}}` })
	if err := c.Delete(context.Background(), "svc-1"); err != nil {
		t.Fatal(err)
	}
	if (*calls)[0].Vars["e"] != "E" || (*calls)[0].Vars["i"] != "svc-1" {
		t.Errorf("vars %+v: a project token can delete only with the environment id", (*calls)[0].Vars)
	}
}

func TestAnErrorNeverEchoesTheToken(t *testing.T) {
	c, _, _ := server(t, func(call) string { return `{"errors":[{"message":"Not Authorized tok-secret"}]}` })
	_, err := c.List(context.Background())
	if err == nil || strings.Contains(err.Error(), "tok-secret") {
		t.Errorf("err %v", err)
	}
}

func TestARedirectIsNotFollowedAndTheTokenGoesNowhereElse(t *testing.T) {
	hits := 0
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer other.Close()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, http.StatusFound) }))
	defer ts.Close()
	c := New(Config{URL: ts.URL, Token: "tok-secret", ProjectID: "P", EnvironmentID: "E", Prefix: "rr-"})
	if _, err := c.List(context.Background()); err == nil || hits != 0 {
		t.Errorf("err %v hits %d: the Project-Access-Token must not follow a redirect", err, hits)
	}
}

func TestRailwayLeavesACallerSuppliedClientAlone(t *testing.T) {
	shared := &http.Client{}
	New(Config{HTTP: shared})
	if shared.CheckRedirect != nil {
		t.Error("New changed the client it was given")
	}
}
