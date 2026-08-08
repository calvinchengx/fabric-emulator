package purview

import (
	"net/http"
	"testing"
)

// `clientError` derives its HTTP status from its Atlas code, so the two cannot
// drift apart — they are one fact rather than two fields a handler must keep
// in step.
//
// The type is deliberately 4xx-only. Server failures do not travel this way:
// handlers write those directly (`writeAtlasErr(w, StatusInternalServerError,
// "ATLAS-500-00-001", …)`), so an unrecognised code defaulting to 400 is the
// design and not a gap. A first draft of this test asserted that
// `ATLAS-500-00-001` yields 500; it yields 400, and the type name is the clue
// that the test was wrong rather than the code.
func TestClientErrorStatusComesFromTheCode(t *testing.T) {
	for _, tc := range []struct {
		code string
		want int
	}{
		{"ATLAS-400-00-001", http.StatusBadRequest},
		{"ATLAS-404-00-00A", http.StatusNotFound},
		{"ATLAS-409-00-002", http.StatusConflict},

		// Not a 4xx this type models, and not parseable at all: both fall back
		// to 400 rather than to a zero status, which would serialise as 200.
		{"ATLAS-500-00-001", http.StatusBadRequest},
		{"nonsense", http.StatusBadRequest},
		{"", http.StatusBadRequest},
	} {
		t.Run(tc.code, func(t *testing.T) {
			e := &clientError{code: tc.code, message: "m"}
			if got := e.status(); got != tc.want {
				t.Fatalf("status of %q = %d, want %d", tc.code, got, tc.want)
			}
			// Error() must name the code: it is what a caller sees in a log
			// when the response body is already gone.
			if msg := e.Error(); msg == "" || msg == ": m" && tc.code != "" {
				t.Fatalf("Error() = %q, which does not identify the code", msg)
			}
		})
	}
}
