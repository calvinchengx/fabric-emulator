package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Every 202 this emulator emits must carry the three headers Fabric's own
// samples show — `Location`, `x-ms-operation-id` and `Retry-After` — and the
// two ids must agree.
//
// True by construction today (one helper writes the envelope), and asserted
// here because "by construction" is a property of the current code, not a
// guarantee about the next handler someone adds. A client polls whichever
// header it was written against: some read `x-ms-operation-id`, some parse the
// `Location` tail, and a 202 missing either is unpollable for half of them.
//
// See docs/49-async-outcome-audit.md for which surfaces answer 202 and why.
func TestEvery202CarriesTheDocumentedPollingHeaders(t *testing.T) {
	a, _ := newAPI(t)
	a.RetryAfterSeconds = 7

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "https://api.fabric.microsoft.com/v1/anything", nil)
	a.startOperation(w, r, "AuditProbe", "")

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}
	op := w.Header().Get("x-ms-operation-id")
	loc := w.Header().Get("Location")
	retry := w.Header().Get("Retry-After")
	if op == "" {
		t.Error("no x-ms-operation-id: a client reading that header cannot poll")
	}
	if loc == "" {
		t.Error("no Location: a client parsing the tail cannot poll")
	}
	if retry != "7" {
		t.Errorf("Retry-After = %q, want the configured 7", retry)
	}
	// The two must name the SAME operation. A Location pointing at a different
	// id than x-ms-operation-id would send two clients to two places, and each
	// would look correct on its own.
	if want := "/v1/operations/" + op; len(loc) < len(want) || loc[len(loc)-len(want):] != want {
		t.Errorf("Location %q does not end in %q — the headers disagree about which operation this is", loc, want)
	}
}
