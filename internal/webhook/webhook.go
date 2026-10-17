// Package webhook receives GitHub workflow_job events. The events only nudge the controller
// to look at once: GitHub does not redeliver a missed event, so the controller's polling is
// the source of truth and nothing here is trusted for what it says, only for when.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

const maxBody = 1 << 20

// Event is the part of a workflow_job event the controller cares about.
type Event struct {
	Action     string
	JobID      int64
	Repo       string // owner/name
	Owner      string
	RunnerName string
}

type payload struct {
	Action      string `json:"action"`
	WorkflowJob struct {
		ID         int64    `json:"id"`
		Labels     []string `json:"labels"`
		RunnerName string   `json:"runner_name"`
	} `json:"workflow_job"`
	Repository struct {
		FullName string `json:"full_name"`
		Owner    struct {
			Login string `json:"login"`
		} `json:"owner"`
	} `json:"repository"`
}

// Handler verifies the signature (HMAC SHA-256 with the shared secret) and calls handle for
// a workflow_job event that carries the label. Every other verified event is acknowledged
// and dropped. With no secret nothing can be verified, so everything is refused.
func Handler(secret, label string, handle func(Event)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
		if !verified(secret, r.Header.Get("X-Hub-Signature-256"), body) {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("X-GitHub-Event") != "workflow_job" {
			w.WriteHeader(http.StatusOK)
			return
		}
		var p payload
		if err := json.Unmarshal(body, &p); err != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		has := false
		for _, l := range p.WorkflowJob.Labels {
			if l == label {
				has = true
			}
		}
		if !has {
			w.WriteHeader(http.StatusOK)
			return
		}
		handle(Event{Action: p.Action, JobID: p.WorkflowJob.ID, Repo: p.Repository.FullName, Owner: p.Repository.Owner.Login, RunnerName: p.WorkflowJob.RunnerName})
		w.WriteHeader(http.StatusAccepted)
	})
}

func verified(secret, header string, body []byte) bool {
	if secret == "" || !strings.HasPrefix(header, "sha256=") {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(header, "sha256="))
	if err != nil {
		return false
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hmac.Equal(m.Sum(nil), got)
}
