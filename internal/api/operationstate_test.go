package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
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
	// NOT RFC3339, though `string (date-time)` reads that way and this test
	// asserted it first. A tenant sends ISO 8601 with up to 7 fractional
	// digits, trailing zeros trimmed, and no `Z`.
	//
	// The pattern is written out LITERALLY rather than built from
	// fabricOperationTime. Parsing with the same constant the handler formats
	// with is self-referential — it passes for any layout the two share, which
	// is every layout. Asked the wrong way it proves nothing; this asks whether
	// the bytes match what a tenant sent. See [[assertions-one-level-off]].
	//
	// The fraction is OPTIONAL and its width is 1-7, because the tenant's width
	// varies: `…07:49:13.5612398` and `…08:00:43.654668`, minutes apart. An
	// earlier version of this line demanded exactly 7 — which our own emitter
	// satisfied and a tenant sending 6 would not. A test can be wrong in the
	// emulator's favour just as easily as the code can.
	shape := regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d{1,7})?$`)
	for _, field := range []string{"createdTimeUtc", "lastUpdatedTimeUtc"} {
		s, _ := body[field].(string)
		if !shape.MatchString(s) {
			t.Errorf("%s = %q, want the measured tenant shape (ISO 8601, up to 7 "+
				"fractional digits with trailing zeros trimmed, no trailing Z)", field, s)
		}
		if _, err := time.Parse(fabricOperationTime, s); err != nil {
			t.Errorf("%s = %q does not round-trip through the handler's own layout: %v", field, s, err)
		}
	}

	// Undocumented, and on every tenant response including the terminal one.
	for _, field := range []string{"error", "blobInfoId", "resultContentType"} {
		v, ok := body[field]
		if !ok {
			t.Errorf("%s key is absent; a tenant sends it with a null value on every poll", field)
		}
		if v != nil {
			t.Errorf("%s = %v on a running operation, want null", field, v)
		}
	}
}

// The tenant's own fractional widths must parse. This is the assertion that
// would have caught a fixed-width `.0000000` emitter paired with a `\d{7}`
// expectation — both wrong, and green together.
func TestMeasuredTenantTimestampsParse(t *testing.T) {
	shape := regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d{1,7})?$`)
	// Captured 2026-08-11 by local_0cdd48fd: a Warehouse create and a failed
	// SemanticModel create.
	for _, s := range []string{
		"2026-08-11T07:49:13.5612398", // 7 digits
		"2026-08-11T08:00:43.654668",  // 6 digits, from the same tenant
		"2026-08-11T08:00:43.7910071",
	} {
		if !shape.MatchString(s) {
			t.Errorf("measured tenant timestamp %q does not match the asserted shape", s)
		}
		if _, err := time.Parse(fabricOperationTime, s); err != nil {
			t.Errorf("measured tenant timestamp %q does not parse with the handler's layout: %v", s, err)
		}
	}
}

// percentComplete is 100 ON SUCCESS ONLY — null while running AND on failure —
// and never an intermediate value. All three measured on a real tenant.
//
// This test previously asserted only `< 100` while running, which an
// interpolated 47 satisfies. The assertion was one level off the property that
// mattered: it checked that progress and status did not contradict each other,
// when the real question was whether the emulator emits a quantity Fabric
// publishes at all. See [[assertions-one-level-off]].
func TestPercentCompleteIs100OnlyOnSuccess(t *testing.T) {
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
	// The KEY must be present and its VALUE null. A missing key and a null are
	// different wires, and a client doing `body["percentComplete"]` in Python
	// tells them apart the hard way.
	pc, ok := b["percentComplete"]
	if !ok {
		t.Error("running: percentComplete key is absent; a tenant sends the key with a null value")
	}
	if pc != nil {
		t.Errorf("running: percentComplete = %v, want null — a tenant publishes no intermediate figure", pc)
	}

	_, b = pollOperation(t, a, done.ID)
	if b["status"] != "Succeeded" || b["percentComplete"] != float64(100) {
		t.Errorf("succeeded operation = %v at %v, want Succeeded at 100", b["status"], b["percentComplete"])
	}

	// A FAILED operation is null, not 100 — measured, and the opposite of what
	// this test asserted first. 100 there would let a client branching on
	// `percentComplete == 100` read a failure as a completion.
	_, b = pollOperation(t, a, failed.ID)
	if b["status"] != "Failed" {
		t.Errorf("failed operation status = %v, want Failed", b["status"])
	}
	if b["percentComplete"] != nil {
		t.Errorf("failed operation percentComplete = %v, want null — 100 only ever means Succeeded",
			b["percentComplete"])
	}
	errObj, _ := b["error"].(map[string]any)
	if errObj == nil {
		t.Fatal("a failed operation carries no error object")
	}
	// isRetriable is measured present on the tenant's failure. A client with no
	// way to read it locally cannot exercise its own retry decision.
	if _, ok := errObj["isRetriable"]; !ok {
		t.Error("the error object has no isRetriable; a tenant sends it")
	}
}

// The interpolation this replaced would pass every assertion above except this
// one: it is the test that pins the ABSENCE of intermediate values, by polling
// across the whole span rather than at one instant.
func TestPercentCompleteNeverTakesAnIntermediateValue(t *testing.T) {
	a, st := newAPI(t)
	st.Clock.Freeze()
	op := &store.Operation{Kind: "AuditProbe", CompleteAt: st.Now() + 10}
	if err := st.CreateOperation(op); err != nil {
		t.Fatal(err)
	}

	for tick := int64(0); tick <= 10; tick++ {
		_, b := pollOperation(t, a, op.ID)
		switch pc := b["percentComplete"].(type) {
		case nil:
			if b["status"] == "Succeeded" {
				t.Errorf("t+%ds: Succeeded with a null percentComplete", tick)
			}
		case float64:
			if pc != 100 {
				t.Errorf("t+%ds: percentComplete = %v — Fabric emits only null or 100", tick, pc)
			}
			if b["status"] != "Succeeded" {
				t.Errorf("t+%ds: percentComplete = 100 with status %v; 100 means Succeeded and nothing else",
					tick, b["status"])
			}
		default:
			t.Errorf("t+%ds: percentComplete = %T(%v), want null or a number", tick, pc, pc)
		}
		st.Clock.Advance(1)
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
