package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
)

// Response recording, for conformance against Microsoft's published OpenAPI.
//
// WHY IT LIVES IN THE EMULATOR RATHER THAN IN A PROXY. A proxy container would
// have to be inserted into each e2e's compose file, and every suite that
// forgot would silently contribute nothing. Recording here is opt-in by one
// environment variable and applies to whatever traffic the suite already
// generates, so a new suite is covered the day it is written rather than the
// day somebody remembers to wire it.
//
// WHAT IT IS FOR. The claims in docs/witnesses.json rest on clients: a client
// drives a surface and its own model rejects a wrong shape. That is strong
// evidence and it is narrow — a surface no packaged client speaks has only
// this repository's own tests behind it, which is the implementation and the
// expectation written by the same hand. A recording validated against
// Microsoft's swagger is a different kind of oracle: machine-readable, theirs,
// and it covers every route a suite happens to touch rather than the few a
// client bothers with.
//
// WHAT IT IS NOT. Shape, never semantics. It cannot catch a job reported
// Succeeded that ran nothing, which is the failure this repository exists to
// hunt — that still needs a client and an engine. A conformance pass means the
// answer was SHAPED right, and says nothing about whether it was TRUE.
//
// WHAT IS RECORDED, and deliberately no more: method, path, status and the
// response body. No request bodies and NO HEADERS — the Authorization header
// carries a bearer token, and a recording written to a file that CI may upload
// as an artifact is exactly the place a token should never be. The bodies are
// the emulator's own deterministic fixtures, not a tenant's data.

// recorded is one response, in the shape scripts/check_openapi_conformance.py
// reads. One JSON object per line: a suite that is killed mid-run still leaves
// a readable file up to the last complete line, where a single JSON array
// would leave an unparseable one.
type recorded struct {
	Method string          `json:"method"`
	Path   string          `json:"path"`
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body,omitempty"`
}

// recorder appends responses to a file. Nil when recording is off, which is
// every run except the conformance job.
type recorder struct {
	mu   sync.Mutex
	file *os.File
}

// newRecorder returns nil unless FABRIC_RECORD_RESPONSES names a file.
//
// A FAILURE TO OPEN IT IS NOT FATAL. Recording is diagnostic: an emulator that
// refused to start because a recording path was unwritable would turn a
// developer's typo into a broken stack, and the conformance job notices an
// absent recording by finding nothing to validate.
func newRecorder() *recorder {
	path := os.Getenv("FABRIC_RECORD_RESPONSES")
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil
	}
	return &recorder{file: f}
}

func (rec *recorder) write(entry recorded) {
	line, err := json.Marshal(entry)
	if err != nil {
		return
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	_, _ = rec.file.Write(append(line, '\n'))
}

// capture is a ResponseWriter that keeps the status and a bounded copy of the
// body.
//
// BOUNDED ON PURPOSE. A OneLake read can be arbitrarily large and a Delta file
// is not a documented JSON shape, so buffering it would cost memory to record
// something no schema describes. Past the cap the body is dropped and the
// status still recorded, which is enough for the status-vs-spec check.
type capture struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
	over   bool
}

const captureLimit = 1 << 20 // 1 MiB

func (c *capture) WriteHeader(status int) {
	c.status = status
	c.ResponseWriter.WriteHeader(status)
}

func (c *capture) Write(p []byte) (int, error) {
	if !c.over {
		if c.body.Len()+len(p) > captureLimit {
			c.over = true
			c.body.Reset()
		} else {
			c.body.Write(p)
		}
	}
	return c.ResponseWriter.Write(p)
}

// Flush and Unwrap keep streaming surfaces working through the wrapper: the
// events endpoint is server-sent events, and without Flush it would buffer
// until the handler returned, which for a stream is never.
func (c *capture) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (c *capture) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// recordable reports whether a path is worth recording: the documented API
// surfaces only.
//
// The portal, its data endpoints and the OneLake data plane are excluded. None
// is described by Microsoft's swagger — the portal is this emulator's own, and
// OneLake speaks the ADLS/Blob protocols, which have their own witnesses
// (ci:adls-sdk, ci:azcopy, ci:delta-rs). Recording them would bury the
// documented routes under traffic no spec covers.
func recordable(path string) bool {
	if strings.HasPrefix(path, "/_emulator/") || strings.HasPrefix(path, "/onelake") {
		return false
	}
	return strings.HasPrefix(path, "/v1/") || strings.HasPrefix(path, "/v1.0/")
}

// record wraps a handler so every documented-surface response is appended.
func (s *Server) record(next http.Handler) http.Handler {
	if s.rec == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !recordable(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		c := &capture{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(c, r)

		entry := recorded{Method: r.Method, Path: r.URL.Path, Status: c.status}
		if body := c.body.Bytes(); len(body) > 0 && json.Valid(body) {
			entry.Body = append(json.RawMessage(nil), body...)
		}
		s.rec.write(entry)
	})
}
