package server

import (
	"bufio"
	"log"
	"net"
	"net/http"
)

// logFaults records every 5xx this emulator emits, which before it nothing did.
//
// THE FINDING. 214 non-test call sites write a 500 (268 write some 5xx), spread
// over 47 files and funnelled through seven error writers -- writeErr alone is
// 150 of them. Not one logged anything. Every `log.*` line in the tree belongs
// to a background driver, a trace or a lifecycle path, so the single status that
// always means "the emulator is broken, not the request" was the one class of
// failure with no server-side record at all.
//
// The detail was not lost, it went the wrong way. `writeErr(w, 500,
// "InternalError", err.Error())` echoes the error into the response body, so the
// CLIENT learns what happened and the operator reading the compose log learns
// nothing -- not even that a 500 occurred. docs/64-logging-quality.md, F6.
//
// WHY AT THE ROOT AND NOT AT THE 214 SITES, in boundBodies's own words one
// function down: "One bound at the root closes the class instead of the
// instances, which is why it is not 68 edits." The same argument holds harder
// here, because the status is observable at the root without knowing anything
// about which handler produced it. One wrapper covers the control plane, the
// portal, the Purview Data Map and both OneLake dialects, and it covers the
// 215th site added tomorrow without an edit.
//
// UNCONDITIONAL, not env-gated. A 5xx is always abnormal, and gating the record
// of it behind a knob reproduces exactly the silence F6 is about -- the operator
// who needs the line is the one who did not set the variable before the failure
// they are now trying to explain. It also means no new row in
// docs/compat-surface.json: this adds no configuration surface.
//
// 4xx IS NOT A FAULT and is deliberately not logged, including boundBodies's own
// 413 (which sits outside this wrapper anyway). A refused request is the
// emulator working.
func (s *Server) logFaults(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		down, fw := wrapFault(w)
		next.ServeHTTP(down, r)
		if fw.status < 500 {
			return
		}
		// THE PATH, NEVER THE QUERY. A OneLake request's RawQuery can carry an
		// Azure SAS, which is the credential class F7 is about
		// (internal/onelake/trace.go) -- and this line is unconditional, so a
		// token in it would reach every compose log rather than only a traced
		// one. The path alone names the route that failed.
		log.Printf("fault: %s %s -> %d%s", r.Method, r.URL.Path, fw.status, fw.codeSuffix())
	})
}

// faultWriter observes the final status without touching the response.
//
// THE ONE REAL RISK IN THIS CHANGE is the wrapper, not the logging, and it cost
// a failing test before it was got right. Two surfaces reach past the
// ResponseWriter interface by TYPE ASSERTION, which does not follow Unwrap:
// terminal.go asserts http.Hijacker for the WebSocket upgrade, and events.go
// asserts http.Flusher for SSE.
//
// THAT CUTS BOTH WAYS, and the second direction is the one that is easy to miss.
// A wrapper that merely embedded the writer would make both assertions FAIL --
// a dead terminal and a dead flow stream. But a wrapper that declares Flush and
// Hijack unconditionally makes them both SUCCEED, including over a connection
// that can do neither: events.go's up-front `w.(http.Flusher)` check exists
// precisely so an unstreamable connection is refused with a clean 500 instead
// of hanging on a stream nobody can read, and a wrapper that always satisfies
// the interface removes that check for every caller.
// TestFlowStreamRefusesAConnectionItCannotStream caught exactly that.
//
// SO THE WRAPPER MIRRORS THE CONNECTION IT WRAPS. wrapFault inspects the
// writer underneath and returns the variant with the matching method set --
// four types, which is the shape this problem has in Go, since a method cannot
// be declared conditionally. `capture` in record.go gets away with one
// unconditional Flush and no Hijack at all only because it is installed on
// nothing: s.rec is nil on every ordinary run, and recordable() excludes
// /_emulator/, where both of these endpoints live. This wrapper is
// unconditional and wraps everything, so it cannot borrow either excuse.
type faultWriter struct {
	http.ResponseWriter
	// 0 until the handler commits a status. Left at 0 by a hijacked connection,
	// which writes its status bytes straight to the socket, and by a handler
	// that returns without writing anything at all -- and 0 < 500, so both are
	// correctly not faults without a second field to track them.
	status int
}

// wrapFault returns the writer to hand downstream and the recorder to read the
// status from afterwards. They are the same object in the no-capability case
// and share one in every other, so the caller always reads the real status.
func wrapFault(w http.ResponseWriter) (http.ResponseWriter, *faultWriter) {
	fw := &faultWriter{ResponseWriter: w}
	_, canFlush := w.(http.Flusher)
	_, canHijack := w.(http.Hijacker)
	switch {
	case canFlush && canHijack:
		// What net/http's own writer is, so this is the ordinary case for
		// every request that arrives over a real connection.
		return flushHijackWriter{flushWriter{fw}}, fw
	case canFlush:
		// httptest.ResponseRecorder, and the gated writers the flow-stream
		// transport tests drive Handler() with.
		return flushWriter{fw}, fw
	case canHijack:
		return hijackWriter{fw}, fw
	default:
		// A writer that can neither stream nor be hijacked must look like one
		// downstream, or events.go admits a connection it cannot serve.
		return fw, fw
	}
}

func (f *faultWriter) WriteHeader(status int) {
	if f.status == 0 {
		f.status = status
	}
	f.ResponseWriter.WriteHeader(status)
}

// Write implies 200 when the handler never called WriteHeader, exactly as
// net/http does -- otherwise an ordinary body-only response would read as
// status 0 rather than as the success it is.
func (f *faultWriter) Write(p []byte) (int, error) {
	if f.status == 0 {
		f.status = http.StatusOK
	}
	return f.ResponseWriter.Write(p)
}

func (f *faultWriter) Unwrap() http.ResponseWriter { return f.ResponseWriter }

// hijack hands the connection over.
//
// A hijacked connection writes its status bytes straight to the socket, so
// nothing passes through WriteHeader and `status` stays 0. That is deliberately
// NOT reported as a fault, and equally not defaulted to 200: the terminal
// upgrade is a success whose status this wrapper cannot see, and inventing one
// either way would be reporting a number nobody wrote.
//
// Unexported, so the base type does not satisfy http.Hijacker. hijackWriter and
// flushHijackWriter expose it, and they are only ever handed to a handler when
// the connection underneath really can be hijacked -- the type assertion here
// is the belt to that braces, so a future caller gets an error rather than a
// panic.
func (f *faultWriter) hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := f.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return hj.Hijack()
}

// The three capability variants. Each embeds the shared *faultWriter, so the
// status every one of them records is the status logFaults reads.

type flushWriter struct{ *faultWriter }

func (f flushWriter) Flush() {
	if fl, ok := f.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

type hijackWriter struct{ *faultWriter }

func (h hijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) { return h.hijack() }

type flushHijackWriter struct{ flushWriter }

func (f flushHijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) { return f.hijack() }

// codeSuffix reports the error code the handler named, when it named one.
//
// TWO HEADERS, because the surfaces spell it differently and both are in scope:
// the control plane and the portal set `x-ms-public-api-error-code` (writeErr,
// writeRetriableErr, writeJSONError), while the OneLake dialects set
// `x-ms-error-code` (writeDFSErr, writeBlobErr). The Atlas and Kusto writers set
// neither, so their faults are logged without a code rather than with a guess --
// the method, path and status still name the route.
func (f *faultWriter) codeSuffix() string {
	for _, h := range [...]string{"x-ms-public-api-error-code", "x-ms-error-code"} {
		if code := f.Header().Get(h); code != "" {
			return " " + code
		}
	}
	return ""
}
