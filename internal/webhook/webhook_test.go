package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func sign(secret, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func post(h http.Handler, secret, event, body string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	r.Header.Set("X-GitHub-Event", event)
	r.Header.Set("X-Hub-Signature-256", sign(secret, body))
	if mutate != nil {
		mutate(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

const queued = `{"action":"queued","workflow_job":{"id":7,"labels":["self-hosted","railway-farzai"],"runner_name":""},"repository":{"full_name":"parsilver/skyabove","owner":{"login":"parsilver"}}}`

func TestAValidQueuedJobWithOurLabelNudgesTheController(t *testing.T) {
	var got []Event
	h := Handler("s3cret", "railway-farzai", func(e Event) { got = append(got, e) })
	if w := post(h, "s3cret", "workflow_job", queued, nil); w.Code != http.StatusAccepted {
		t.Fatalf("status %d", w.Code)
	}
	if len(got) != 1 || got[0].Action != "queued" || got[0].Repo != "parsilver/skyabove" || got[0].JobID != 7 {
		t.Errorf("events %+v", got)
	}
}

func TestABadOrMissingSignatureIsRefusedAndNothingIsHandled(t *testing.T) {
	called := false
	h := Handler("s3cret", "railway-farzai", func(Event) { called = true })
	if w := post(h, "wrong", "workflow_job", queued, nil); w.Code != http.StatusUnauthorized {
		t.Errorf("wrong secret: status %d", w.Code)
	}
	if w := post(h, "s3cret", "workflow_job", queued, func(r *http.Request) { r.Header.Del("X-Hub-Signature-256") }); w.Code != http.StatusUnauthorized {
		t.Errorf("no signature: status %d", w.Code)
	}
	if w := post(h, "s3cret", "workflow_job", queued, func(r *http.Request) { r.Header.Set("X-Hub-Signature-256", "sha256=zz") }); w.Code != http.StatusUnauthorized {
		t.Errorf("garbage signature: status %d", w.Code)
	}
	if called {
		t.Error("an unsigned event was handled")
	}
}

func TestAnEmptySecretRefusesEverything(t *testing.T) {
	h := Handler("", "railway-farzai", func(Event) { t.Error("handled") })
	if w := post(h, "", "workflow_job", queued, nil); w.Code != http.StatusUnauthorized {
		t.Errorf("status %d: with no secret configured nothing can be verified", w.Code)
	}
}

func TestOtherEventsAndOtherLabelsAreAcknowledgedAndIgnored(t *testing.T) {
	h := Handler("s", "railway-farzai", func(Event) { t.Error("handled") })
	if w := post(h, "s", "ping", `{}`, nil); w.Code != http.StatusOK {
		t.Errorf("ping: %d", w.Code)
	}
	other := strings.Replace(queued, "railway-farzai", "ubuntu-latest", 1)
	if w := post(h, "s", "workflow_job", other, nil); w.Code != http.StatusOK {
		t.Errorf("other label: %d", w.Code)
	}
}

func TestAHugeBodyIsRefused(t *testing.T) {
	h := Handler("s", "railway-farzai", func(Event) {})
	big := strings.Repeat("a", 2<<20)
	if w := post(h, "s", "workflow_job", big, nil); w.Code != http.StatusRequestEntityTooLarge && w.Code != http.StatusUnauthorized {
		t.Errorf("status %d", w.Code)
	}
}
