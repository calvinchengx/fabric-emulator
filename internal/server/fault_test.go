package server

// The 5xx fault log (fault.go) and -- the part that can actually break
// something -- the ResponseWriter wrapper it needs to observe the status.
//
// WHY THE WRAPPER IS THE RISK AND THE LOGGING IS NOT. logFaults is installed at
// the root, so it now sits in front of every response this emulator writes,
// including the two that reach past the ResponseWriter interface by type
// assertion: terminal.go asserts http.Hijacker for the WebSocket upgrade and
// events.go asserts http.Flusher for SSE. A direct `w.(http.Flusher)` does NOT
// follow Unwrap, so a wrapper that merely embedded the writer would break both
// and the fault log would be paid for with a dead terminal and a dead flow
// stream. Those two assertions are what the second half of this file pins.

import (
	"bufio"
	"bytes"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// captureFaultLog redirects the standard logger for the duration of a test.
// These paths report ONLY to the log, so the log is the observable -- the same
// device, and the same reason, as captureLog in
// internal/api/livy_catalog_test.go and captureOneLakeLog in
// internal/onelake/blob_test.go.
func captureFaultLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })
	return buf
}

// faultServe drives one request through logFaults and returns the recorder and
// whatever was logged.
func faultServe(t *testing.T, h http.HandlerFunc, method, target string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	buf := captureFaultLog(t)
	s := &Server{}
	rec := httptest.NewRecorder()
	s.logFaults(h).ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec, buf.String()
}

// TestAFaultIsLoggedWithItsMethodPathStatusAndCode.
//
// THE FINDING THIS CLOSES: 214 non-test call sites write a 500 and not one of
// them logged anything, so the detail reached the caller in the error body and
// the operator reading the compose log learned nothing -- not even that a 500
// had happened (docs/64-logging-quality.md, F6).
//
// writeJSONError is used rather than internal/api's writeErr because it is the
// in-package writer and sets the IDENTICAL `x-ms-public-api-error-code` header,
// which is the field the log line reads.
func TestAFaultIsLoggedWithItsMethodPathStatusAndCode(t *testing.T) {
	_, out := faultServe(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSONError(w, http.StatusInternalServerError, "InternalError", "the store is gone")
	}, http.MethodPost, "/v1/workspaces/ws-1/items")

	if n := strings.Count(out, "fault: "); n != 1 {
		t.Fatalf("want exactly one fault line, got %d:\n%s", n, out)
	}
	for _, want := range []string{"fault: ", "POST", "/v1/workspaces/ws-1/items", "500", "InternalError"} {
		if !strings.Contains(out, want) {
			t.Errorf("the fault line does not carry %q -- an operator cannot act on it:\n%s",
				want, out)
		}
	}
}

// TestTheOneLakeErrorCodeSpellingIsReadToo. The DFS and Blob dialects set
// `x-ms-error-code`, not the control plane's `x-ms-public-api-error-code`, and
// both surfaces are inside this wrapper. Reading only the Fabric spelling would
// log every OneLake fault with no code at all.
func TestTheOneLakeErrorCodeSpellingIsReadToo(t *testing.T) {
	_, out := faultServe(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("x-ms-error-code", "InternalError")
		w.WriteHeader(http.StatusInternalServerError)
	}, http.MethodGet, "/ws/item/Files/x.parquet")

	if !strings.Contains(out, "InternalError") {
		t.Errorf("the OneLake error-code header was not read:\n%s", out)
	}
}

// TestAFaultWithNoErrorCodeStillLogs. The Atlas and Kusto writers set neither
// header, so their faults must be logged WITHOUT a code rather than not at all
// -- method, path and status still name the route, and a guess would be worse
// than an omission.
func TestAFaultWithNoErrorCodeStillLogs(t *testing.T) {
	_, out := faultServe(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}, http.MethodGet, "/atlas/v2/entity/guid/abc")

	if !strings.Contains(out, "fault: GET /atlas/v2/entity/guid/abc -> 500") {
		t.Fatalf("a codeless fault was not logged plainly:\n%s", out)
	}
	// And no trailing separator left where a code would have been.
	if strings.Contains(out, "500 \n") {
		t.Errorf("the line carries an empty code suffix:\n%s", out)
	}
}

// TestOnlyA5xxIsAFault. A 4xx is the emulator working: a refused request is an
// answer, not a failure, and logging every one would bury the 5xx this exists to
// surface. boundBodies's own 413 is in that class (and sits outside this
// wrapper besides).
func TestOnlyA5xxIsAFault(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		fault  bool
	}{
		{"ok", http.StatusOK, false},
		{"created", http.StatusCreated, false},
		{"bad request", http.StatusBadRequest, false},
		{"not found", http.StatusNotFound, false},
		{"too large", http.StatusRequestEntityTooLarge, false},
		{"internal error", http.StatusInternalServerError, true},
		{"not implemented", http.StatusNotImplemented, true},
		{"bad gateway", http.StatusBadGateway, true},
		{"unavailable", http.StatusServiceUnavailable, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, out := faultServe(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}, http.MethodGet, "/health")
			if got := strings.Contains(out, "fault: "); got != tc.fault {
				t.Errorf("status %d logged=%v; want %v:\n%s", tc.status, got, tc.fault, out)
			}
		})
	}
}

// TestABodyOnlyResponseIsNotAFault. A handler that writes a body without ever
// calling WriteHeader is a 200, exactly as net/http treats it. Leaving `status`
// at 0 would be harmless (0 < 500), but implying 200 is what keeps the field
// meaning what it says for anything that reads it later.
func TestABodyOnlyResponseIsNotAFault(t *testing.T) {
	rec, out := faultServe(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}, http.MethodGet, "/health")

	if strings.Contains(out, "fault: ") {
		t.Errorf("an implicit 200 was logged as a fault:\n%s", out)
	}
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Errorf("response = %d %q; want 200 \"ok\"", rec.Code, rec.Body.String())
	}
}

// TestTheQueryStringNeverReachesTheFaultLine.
//
// F7's rule applied to F6's fix. A OneLake request's RawQuery can carry an
// Azure SAS, and this line is UNCONDITIONAL -- unlike the trace, which at least
// requires somebody to set a knob -- so a query string here would put a
// credential into every compose log of every run. The path alone names the
// route.
func TestTheQueryStringNeverReachesTheFaultLine(t *testing.T) {
	_, out := faultServe(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}, http.MethodGet, "/ws/item/Files/x?sig=SECRETSIGNATURE&sv=2022-11-02")

	if strings.Contains(out, "SECRETSIGNATURE") || strings.Contains(out, "sig=") {
		t.Fatalf("the fault line leaked the query string, which can carry a SAS:\n%s", out)
	}
	if !strings.Contains(out, "/ws/item/Files/x") {
		t.Errorf("the fault line lost the path as well as the query:\n%s", out)
	}
}

// TestTheWrapperDeliversTheResponseUnchanged. The logging is worthless if the
// wrapper costs a byte of the response: status, headers and body must arrive at
// the client exactly as the handler wrote them.
func TestTheWrapperDeliversTheResponseUnchanged(t *testing.T) {
	rec, _ := faultServe(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSONError(w, http.StatusInternalServerError, "InternalError", "the store is gone")
	}, http.MethodGet, "/v1/workspaces")

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
	if got := rec.Header().Get("x-ms-public-api-error-code"); got != "InternalError" {
		t.Errorf("error-code header = %q; want InternalError", got)
	}
	if !strings.Contains(rec.Body.String(), "the store is gone") {
		t.Errorf("the body did not survive the wrapper: %q", rec.Body.String())
	}
}

// TestOnlyTheFirstStatusIsReported. A handler that calls WriteHeader twice gets
// a "superfluous WriteHeader" warning from net/http and the FIRST status is the
// one on the wire, so that is the one the fault line must name.
func TestOnlyTheFirstStatusIsReported(t *testing.T) {
	_, out := faultServe(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.WriteHeader(http.StatusOK)
	}, http.MethodGet, "/v1/workspaces")

	if !strings.Contains(out, "-> 500") {
		t.Errorf("the second WriteHeader won; the line must report what the client saw:\n%s", out)
	}
}

// --------------------------------------------------------------------------
// The two type assertions this wrapper must not break.
// --------------------------------------------------------------------------

// flushRecorder is a ResponseWriter that is a Flusher and records the call.
type flushRecorder struct {
	http.ResponseWriter
	flushed int
}

func (f *flushRecorder) Flush() { f.flushed++ }

// TestAFlusherSurvivesTheFaultWrapper.
//
// internal/server/events.go:106 does `flusher, ok := w.(http.Flusher)` and
// answers 500 "streaming unsupported by this connection" when the assertion
// fails. A direct assertion does not follow Unwrap, so without faultWriter.Flush
// the flow stream would refuse every connection -- and the refusal is itself a
// 500, so the fault log would have cheerfully recorded the breakage it caused.
func TestAFlusherSurvivesTheFaultWrapper(t *testing.T) {
	inner := &flushRecorder{ResponseWriter: httptest.NewRecorder()}
	s := &Server{}
	saw := false
	s.logFaults(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fl, ok := w.(http.Flusher)
		saw = ok
		if ok {
			fl.Flush()
		}
	})).ServeHTTP(inner, httptest.NewRequest(http.MethodGet, "/_emulator/events", nil))

	if !saw {
		t.Fatal("the handler could not assert http.Flusher through the fault wrapper — " +
			"the flow stream would refuse every connection with \"streaming unsupported\"")
	}
	if inner.flushed != 1 {
		t.Errorf("Flush reached the underlying writer %d time(s); want 1 — a flush that "+
			"goes nowhere buffers an SSE stream until the handler returns, which for a "+
			"stream is never", inner.flushed)
	}
}

// TestTheWrapperMirrorsTheConnectionsCapabilities.
//
// THE DIRECTION THAT ACTUALLY BROKE, and the reason this file has four cases
// instead of one. The obvious hazard is a wrapper that hides Flush and Hijack,
// which breaks the flow stream and the terminal. The non-obvious one is a
// wrapper that ADVERTISES them unconditionally: events.go's first act is
// `w.(http.Flusher)`, and that check exists so an unstreamable connection is
// refused with a clean 500 rather than hung on a stream nobody can read. A
// wrapper that always satisfies the interface deletes that check for every
// caller -- TestFlowStreamRefusesAConnectionItCannotStream failed on exactly
// that before wrapFault selected a variant.
//
// So the wrapper must satisfy an interface if and only if the connection
// underneath does. All four combinations, because each is a live case: net/http
// supplies both, httptest.ResponseRecorder supplies only Flush, and
// events_transport_test.go's nonFlusher supplies neither on purpose.
func TestTheWrapperMirrorsTheConnectionsCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name         string
		inner        http.ResponseWriter
		flush, hijck bool
	}{
		{"neither", &plainWriter{header: http.Header{}}, false, false},
		{"flusher only", httptest.NewRecorder(), true, false},
		{"hijacker only", &hijackOnly{plainWriter{header: http.Header{}}}, false, true},
		{"both", &flushAndHijack{plainWriter{header: http.Header{}}}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			down, fw := wrapFault(tc.inner)
			if _, ok := down.(http.Flusher); ok != tc.flush {
				t.Errorf("http.Flusher = %v; want %v -- the wrapper must not "+
					"invent or hide a capability the connection decides", ok, tc.flush)
			}
			if _, ok := down.(http.Hijacker); ok != tc.hijck {
				t.Errorf("http.Hijacker = %v; want %v", ok, tc.hijck)
			}
			// Whichever variant was chosen, the status it records must be the
			// one logFaults reads -- they share the base writer.
			down.WriteHeader(http.StatusInternalServerError)
			if fw.status != http.StatusInternalServerError {
				t.Errorf("the variant did not record the status: %d", fw.status)
			}
		})
	}
}

// TestEveryVariantStillCarriesTheResponse. The capability variants are where a
// promoted method could go missing silently, so each one is driven end to end.
func TestEveryVariantStillCarriesTheResponse(t *testing.T) {
	for _, inner := range []http.ResponseWriter{
		&plainWriter{header: http.Header{}},
		httptest.NewRecorder(),
		&hijackOnly{plainWriter{header: http.Header{}}},
		&flushAndHijack{plainWriter{header: http.Header{}}},
	} {
		down, fw := wrapFault(inner)
		down.Header().Set("x-ms-public-api-error-code", "InternalError")
		down.WriteHeader(http.StatusInternalServerError)
		if _, err := down.Write([]byte("body")); err != nil {
			t.Errorf("%T: Write = %v", inner, err)
		}
		if fw.status != http.StatusInternalServerError {
			t.Errorf("%T: status = %d", inner, fw.status)
		}
		if got := fw.codeSuffix(); got != " InternalError" {
			t.Errorf("%T: codeSuffix = %q", inner, got)
		}
	}
}

// plainWriter is a bare ResponseWriter: no Flush, no Hijack.
type plainWriter struct {
	header http.Header
	code   int
	wrote  bool
}

func (p *plainWriter) Header() http.Header         { return p.header }
func (p *plainWriter) WriteHeader(c int)           { p.code = c }
func (p *plainWriter) Write(b []byte) (int, error) { p.wrote = true; return len(b), nil }

// hijackOnly can be hijacked but cannot stream — a connection shape the
// wrapper must reproduce exactly rather than round up to "both".
type hijackOnly struct{ plainWriter }

func (h *hijackOnly) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, http.ErrNotSupported
}

// flushAndHijack is what net/http's own writer is, and therefore the ordinary
// case for every request arriving over a real connection.
type flushAndHijack struct{ plainWriter }

func (f *flushAndHijack) Flush() {}
func (f *flushAndHijack) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, http.ErrNotSupported
}

// hijackRecorder is a ResponseWriter that can be hijacked.
type hijackRecorder struct {
	http.ResponseWriter
	conn     net.Conn
	hijacked int
}

func (h *hijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h.hijacked++
	return h.conn, bufio.NewReadWriter(bufio.NewReader(h.conn), bufio.NewWriter(h.conn)), nil
}

// TestAHijackerSurvivesTheFaultWrapper.
//
// internal/server/terminal.go:157 does `hj, ok := w.(http.Hijacker)` and answers
// 500 "TerminalMisconfigured" when it fails. `capture` in record.go gets away
// with having no Hijack only because recordable() excludes /_emulator/ and the
// terminal lives there; this wrapper wraps EVERYTHING, so omitting it would
// break the terminal outright.
func TestAHijackerSurvivesTheFaultWrapper(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })

	inner := &hijackRecorder{ResponseWriter: httptest.NewRecorder(), conn: server}
	s := &Server{}
	var got net.Conn
	saw := false
	s.logFaults(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		saw = ok
		if !ok {
			return
		}
		c, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("Hijack through the wrapper failed: %v", err)
			return
		}
		got = c
	})).ServeHTTP(inner, httptest.NewRequest(http.MethodGet, "/_emulator/portal/terminal/ws", nil))

	if !saw {
		t.Fatal("the handler could not assert http.Hijacker through the fault wrapper — " +
			"the terminal upgrade would answer TerminalMisconfigured")
	}
	if inner.hijacked != 1 {
		t.Errorf("Hijack reached the underlying writer %d time(s); want 1", inner.hijacked)
	}
	if got != server {
		t.Error("the wrapper handed back a different connection than the one it hijacked")
	}
}

// TestAHijackedConnectionIsNotAFault.
//
// A hijacked connection writes its status bytes straight to the socket, so
// nothing passes through WriteHeader and `status` stays 0. Defaulting that to
// 500 would log a fault on every successful terminal upgrade; defaulting it to
// 200 would assert a status nobody wrote. It is simply not reported.
func TestAHijackedConnectionIsNotAFault(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })

	buf := captureFaultLog(t)
	inner := &hijackRecorder{ResponseWriter: httptest.NewRecorder(), conn: server}
	s := &Server{}
	s.logFaults(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hj, ok := w.(http.Hijacker); ok {
			_, _, _ = hj.Hijack()
		}
	})).ServeHTTP(inner, httptest.NewRequest(http.MethodGet, "/_emulator/portal/terminal/ws", nil))

	if strings.Contains(buf.String(), "fault: ") {
		t.Errorf("a successful terminal upgrade was logged as a fault:\n%s", buf.String())
	}
}

// TestHijackOnAConnectionThatCannotBeHijackedReturnsAnError. The defensive
// branch inside hijack(): a hijackWriter is only ever handed out over a
// hijackable connection, but if that ever stopped being true the wrapper must
// return an error rather than panic on the type assertion -- which is the shape
// terminal.go's own `ok` check reports as TerminalMisconfigured.
func TestHijackOnAConnectionThatCannotBeHijackedReturnsAnError(t *testing.T) {
	// Parenthesised: a composite literal in an `if` header is a parse error.
	hw := hijackWriter{&faultWriter{ResponseWriter: httptest.NewRecorder()}}
	if _, _, err := hw.Hijack(); err == nil {
		t.Fatal("Hijack on a non-hijackable writer returned no error")
	}
}

// TestUnwrapReachesTheUnderlyingWriter. http.ResponseController and anything
// else that walks the chain needs this; `capture` in record.go provides it for
// the same reason.
func TestUnwrapReachesTheUnderlyingWriter(t *testing.T) {
	inner := httptest.NewRecorder()
	fw := &faultWriter{ResponseWriter: inner}
	if fw.Unwrap() != http.ResponseWriter(inner) {
		t.Error("Unwrap did not return the wrapped writer")
	}
}
