package server

// The outer request-body ceiling (Server.boundBodies), asserted in the three
// directions that matter: traffic under the bound is untouched, traffic over it
// is refused with 413 rather than allocated, and the bound does not fire below
// the OneLake ceilings it sits above.
//
// WHY THE THIRD ONE IS HERE. An earlier design put a 64 MiB bound on the control
// plane and excluded the data plane; the correction was a single bound above
// every inner ceiling instead. Those two designs are indistinguishable from a
// test that only checks that something big is refused -- both refuse. What tells
// them apart is a body LARGER than the control plane's old limit being accepted,
// which is what TestTheOuterBoundSitsAboveTheOneLakeCeilings pins.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/config"
	"github.com/calvinchengx/fabric-emulator/internal/httpx"
)

// boundedServer is newControlServer with the bound armed. The shared helper
// builds a Config literal, which leaves MaxRequestBytes at 0 (unlimited) -- that
// is what keeps every other test in this package unaffected by this change, and
// it means a test OF the bound has to ask for it explicitly.
func boundedServer(t *testing.T, max int64) *Server {
	t.Helper()
	cfg := &config.Config{EntraIssuer: "https://unused/t/v2.0", MaxRequestBytes: max}
	if err := cfg.Finish(); err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// post drives the root handler with a body of n bytes and a declared
// Content-Length, which is what every real client sends.
func post(t *testing.T, s *Server, path string, n int) *httptest.ResponseRecorder {
	t.Helper()
	body := strings.Repeat("x", n)
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.ContentLength = int64(n)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

// TestABodyUnderTheBoundIsUntouched. The bound must be invisible to ordinary
// traffic, or it is not a backstop but a behaviour change. `{"advance":0}` on the
// control surface is a documented no-op tick, so a 200 here is the handler
// actually running rather than merely not being refused.
func TestABodyUnderTheBoundIsUntouched(t *testing.T) {
	s := boundedServer(t, 1<<20)
	r := httptest.NewRequest(http.MethodPost, "/_emulator/clock",
		strings.NewReader(`{"advance":0}`))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("a small control-plane POST answered %d, want 200: %s",
			w.Code, w.Body.String())
	}
}

// TestABodyOverTheBoundIsRefusedWithout413BeingAGuess.
//
// 413 and not 400: the caller's request was well-formed and too big, and telling
// them it was malformed sends them to debug their JSON. That distinction is the
// whole reason boundBodies checks Content-Length itself instead of relying on
// http.MaxBytesReader alone -- MaxBytesReader makes the READ fail, so the
// decoding handler would have reported "malformed JSON" for a body it never saw
// the end of.
func TestABodyOverTheBoundIsRefusedWith413(t *testing.T) {
	const max = 1 << 16
	s := boundedServer(t, max)
	w := post(t, s, "/_emulator/clock", max+1)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("an oversized POST answered %d, want 413: %s", w.Code, w.Body.String())
	}
	// The reason has to name the lever, or the operator cannot act on it.
	if !strings.Contains(w.Body.String(), "FABRIC_MAX_REQUEST_BYTES") {
		t.Errorf("the 413 body does not name the knob that raises the bound: %s",
			w.Body.String())
	}
	// Exactly at the bound is NOT over it: an off-by-one here would refuse a
	// body of precisely the documented size.
	if got := post(t, s, "/_emulator/clock", max).Code; got == http.StatusRequestEntityTooLarge {
		t.Errorf("a body of exactly the bound (%d) was refused; the limit is inclusive", max)
	}
}

// TestAChunkedBodyOverTheBoundIsStillCapped.
//
// The other path, and the reason boundBodies has two. With no declared
// Content-Length the early check cannot fire, so MaxBytesReader is what holds
// the line: the read fails and the handler reports in its own voice. The status
// is the handler's to choose (400 here -- it was decoding), and what this asserts
// is that the request does NOT succeed, because succeeding would mean the body
// was read in full.
func TestAChunkedBodyOverTheBoundIsStillCapped(t *testing.T) {
	const max = 1 << 12
	s := boundedServer(t, max)
	r := httptest.NewRequest(http.MethodPost, "/_emulator/clock",
		strings.NewReader(`{"advance":0,"pad":"`+strings.Repeat("x", max*2)+`"}`))
	// -1 is how net/http spells "length unknown", i.e. chunked.
	r.ContentLength = -1
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code == http.StatusOK {
		t.Fatalf("an oversized chunked body was accepted (200), so nothing bounded "+
			"the read: %s", w.Body.String())
	}
}

// TestAZeroBoundMeansUnlimited. The documented escape hatch for anyone already
// posting something larger than the default. If this ever starts refusing, the
// migration path in docs/04-configuration.md is a lie.
func TestAZeroBoundMeansUnlimited(t *testing.T) {
	s := boundedServer(t, 0)
	r := httptest.NewRequest(http.MethodPost, "/_emulator/clock",
		strings.NewReader(`{"advance":0}`))
	r.ContentLength = 1 << 40 // far past any plausible ceiling
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code == http.StatusRequestEntityTooLarge {
		t.Fatal("FABRIC_MAX_REQUEST_BYTES=0 refused a body; 0 must mean unlimited")
	}
}

// TestTheOuterBoundSitsAboveTheOneLakeCeilings is the ordering assertion, in Go,
// beside the static one in scripts/check_perf_regressions.py.
//
// Two different failures are being prevented and neither is hypothetical. An
// outer bound BELOW MaxBlobWrite would break large Delta and Parquet uploads --
// the thing the data plane is for. An outer bound EQUAL to it would let the
// upload through but replace httpx's specific fit-vs-truncated message with
// net/http's generic refusal, because ReadBounded probes max+1: a silent
// downgrade of the diagnosis on the one ceiling `fab cp` has actually crossed.
func TestTheOuterBoundSitsAboveTheOneLakeCeilings(t *testing.T) {
	inner := map[string]int64{
		"MaxDFSAppend":    httpx.MaxDFSAppend,
		"MaxDFSPut":       httpx.MaxDFSPut,
		"MaxBlobWrite":    httpx.MaxBlobWrite,
		"MaxBlobMetadata": httpx.MaxBlobMetadata,
		"MaxItemContent":  httpx.MaxItemContent,
		"MaxProxyBody":    httpx.MaxProxyBody,
		"MaxControlBody":  httpx.MaxControlBody,
		"MaxExternalRead": httpx.MaxExternalRead,
	}
	for name, v := range inner {
		if v >= int64(httpx.DefaultMaxRequestBody) {
			t.Errorf("%s=%d is not strictly below DefaultMaxRequestBody=%d; "+
				"ReadBounded probes max+1, so its specific message would be "+
				"replaced by net/http's generic refusal", name, v,
				int64(httpx.DefaultMaxRequestBody))
		}
	}
	// And the relay ceilings specifically, because the design this replaced
	// would have halved them: a 64 MiB control-plane bound sat under the
	// 128 MiB MaxProxyBody that internal/api/mlflow.go and internal/api/kql.go
	// read with, so two documented relay paths would have silently stopped
	// accepting what they advertise.
	if httpx.MaxProxyBody >= httpx.DefaultMaxRequestBody {
		t.Fatalf("the MLflow/Kusto relay ceiling (%d) is not below the outer "+
			"bound (%d)", httpx.MaxProxyBody, httpx.DefaultMaxRequestBody)
	}
}

// TestTheDataPlaneIsNotExcludedFromTheBound.
//
// The bound wraps the Host-routed OneLake branches too, and that is a decision
// rather than an oversight: the data plane is the surface most able to exhaust
// memory, so excluding it would have left the largest reads as the only unbounded
// ones. Asserted through the account-prefixed /onelake path, which reaches the
// Blob surface on any host and so needs no Host header games.
func TestTheDataPlaneIsNotExcludedFromTheBound(t *testing.T) {
	const max = 1 << 16
	s := boundedServer(t, max)
	path := fmt.Sprintf("/onelake/%s/Files/big.parquet", "ws")
	if got := post(t, s, path, max+1).Code; got != http.StatusRequestEntityTooLarge {
		t.Fatalf("an oversized data-plane PUT answered %d, want 413 — the bound "+
			"is not reaching the OneLake branches", got)
	}
}
