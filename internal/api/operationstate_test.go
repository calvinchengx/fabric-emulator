package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// Fabric's OperationState is documented as {status, createdTimeUtc,
// lastUpdatedTimeUtc, percentComplete, error}. This emulator sent only
// {id, status, error}, so a client rendering progress or logging a start time
// saw NOTHING locally and real values on a tenant — the divergence that only
// shows up in production.
//
// See docs/49-async-outcome-audit.md.

func pollOperation(t *testing.T, a *API, id string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "https://api.fabric.microsoft.com/v1/operations/"+id, nil)
	r.SetPathValue("oid", id)
	a.getOperation(w, r, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("poll status = %d: %s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return w, body
}

func TestOperationStateCarriesEveryDocumentedField(t *testing.T) {
	a, st := newAPI(t)
	now := st.Now()
	op := &store.Operation{Kind: "AuditProbe", CompleteAt: now + 100}
	if err := st.CreateOperation(op); err != nil {
		t.Fatal(err)
	}

	_, body := pollOperation(t, a, op.ID)
	for _, field := range []string{"status", "createdTimeUtc", "lastUpdatedTimeUtc", "percentComplete"} {
		if _, ok := body[field]; !ok {
			t.Errorf("%s is missing — the documented OperationState carries it", field)
		}
	}
	// Times are RFC3339, which is what `string (date-time)` means and what a
	// client will try to parse.
	for _, field := range []string{"createdTimeUtc", "lastUpdatedTimeUtc"} {
		s, _ := body[field].(string)
		if _, err := time.Parse(time.RFC3339, s); err != nil {
			t.Errorf("%s = %q is not RFC3339: %v", field, s, err)
		}
	}
}

// percentComplete must be DERIVED from the same clock the status is, or the two
// disagree — "Succeeded" at 40%, or "Running" at 100%, both of which a progress
// UI would render as a stuck or lying bar.
func TestPercentCompleteAgreesWithStatus(t *testing.T) {
	a, st := newAPI(t)
	now := st.Now()

	running := &store.Operation{Kind: "AuditProbe", CompleteAt: now + 1000}
	done := &store.Operation{Kind: "AuditProbe", CompleteAt: now}
	failed := &store.Operation{Kind: "AuditProbe", CompleteAt: now, FailWith: "OperationFailed"}
	for _, op := range []*store.Operation{running, done, failed} {
		if err := st.CreateOperation(op); err != nil {
			t.Fatal(err)
		}
	}

	_, b := pollOperation(t, a, running.ID)
	if b["status"] == "Succeeded" {
		t.Fatal("the probe completed too early to test the running case")
	}
	if pc := b["percentComplete"].(float64); pc >= 100 {
		t.Errorf("a running operation reports %v%% — status and progress disagree", pc)
	}

	_, b = pollOperation(t, a, done.ID)
	if b["status"] != "Succeeded" || b["percentComplete"].(float64) != 100 {
		t.Errorf("succeeded operation = %v at %v%%, want Succeeded at 100", b["status"], b["percentComplete"])
	}

	// A failed operation stopped progressing too. The status says which
	// outcome; percentComplete says only that it is no longer moving.
	_, b = pollOperation(t, a, failed.ID)
	if b["status"] != "Failed" || b["percentComplete"].(float64) != 100 {
		t.Errorf("failed operation = %v at %v%%, want Failed at 100", b["status"], b["percentComplete"])
	}
	if b["error"] == nil {
		t.Error("a failed operation carries no error object")
	}
}

// Location MOVES: the state URL while running, the RESULT URL once succeeded.
// That is how a client following Location alone reaches the result without
// building a URL, and it is the documented behaviour in both samples.
func TestPollingHeadersFollowTheDocumentedSamples(t *testing.T) {
	a, st := newAPI(t)
	a.RetryAfterSeconds = 11
	now := st.Now()

	running := &store.Operation{Kind: "AuditProbe", CompleteAt: now + 1000, ResultRef: "item-1"}
	done := &store.Operation{Kind: "CreateItem", CompleteAt: now, ResultRef: "item-1"}
	noResult := &store.Operation{Kind: "CommitToGit", CompleteAt: now}
	for _, op := range []*store.Operation{running, done, noResult} {
		if err := st.CreateOperation(op); err != nil {
			t.Fatal(err)
		}
	}

	w, _ := pollOperation(t, a, running.ID)
	if got := w.Header().Get("x-ms-operation-id"); got != running.ID {
		t.Errorf("running: x-ms-operation-id = %q, want %q", got, running.ID)
	}
	if got, want := w.Header().Get("Location"), "/v1/operations/"+running.ID; !hasSuffix(got, want) {
		t.Errorf("running: Location = %q, want it to end in %q (the STATE url)", got, want)
	}
	if got := w.Header().Get("Retry-After"); got != "11" {
		t.Errorf("running: Retry-After = %q, want the configured 11 — a client has to know how long to sleep", got)
	}

	w, _ = pollOperation(t, a, done.ID)
	if got, want := w.Header().Get("Location"), "/v1/operations/"+done.ID+"/result"; !hasSuffix(got, want) {
		t.Errorf("succeeded: Location = %q, want it to end in %q (the RESULT url)", got, want)
	}
	if got := w.Header().Get("Retry-After"); got != "" {
		t.Errorf("succeeded: Retry-After = %q, want none — telling a finished client to sleep is wrong", got)
	}

	// An operation with no result has nowhere to point once done; the
	// documented shape is simply no Location, not a dangling one.
	w, _ = pollOperation(t, a, noResult.ID)
	if got := w.Header().Get("Location"); got != "" {
		t.Errorf("resultless operation: Location = %q, want none", got)
	}
	if got := w.Header().Get("x-ms-operation-id"); got != noResult.ID {
		t.Errorf("resultless operation: x-ms-operation-id = %q", got)
	}
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}
