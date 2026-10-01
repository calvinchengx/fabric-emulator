package server

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The recorder is the input to scripts/check_openapi_conformance.py, so a
// recorder that quietly writes nothing makes that checker pass on an empty
// file — which the checker refuses, but only if the file is empty rather than
// wrong. These assert the two properties the conformance job depends on:
// documented surfaces are recorded, and headers never are.

func recorderFor(t *testing.T) (*Server, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rec.jsonl")
	t.Setenv("FABRIC_RECORD_RESPONSES", path)
	s := &Server{rec: newRecorder()}
	if s.rec == nil {
		t.Fatal("newRecorder returned nil with the variable set")
	}
	// Windows cannot delete an open file, so t.TempDir()'s cleanup fails if
	// the handle is still held. See recorder.Close.
	t.Cleanup(func() { _ = s.rec.Close() })
	return s, path
}

func lines(t *testing.T, path string) []recorded {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []recorded
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var entry recorded
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("recorded a line that is not JSON: %q", line)
		}
		out = append(out, entry)
	}
	return out
}

func serve(s *Server, method, target string, body string, status int) {
	h := s.record(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, target, nil))
}

func TestRecordingIsOffWithoutTheVariable(t *testing.T) {
	t.Setenv("FABRIC_RECORD_RESPONSES", "")
	if newRecorder() != nil {
		t.Fatal("recording must be off unless the variable names a file")
	}
}

// TestAnUnwritableRecordingPathIsNotFatal. Recording is diagnostic: an
// emulator that refused to start because a path was unwritable would turn a
// typo into a broken stack, and the conformance job already fails on a
// recording it cannot find.
func TestAnUnwritableRecordingPathIsNotFatal(t *testing.T) {
	t.Setenv("FABRIC_RECORD_RESPONSES", filepath.Join(t.TempDir(), "no-such-dir", "r.jsonl"))
	if newRecorder() != nil {
		t.Fatal("an unopenable path should disable recording, not panic or succeed")
	}
}

// TestAnUnwritableRecordingPathSaysSo, which is the half the test above cannot
// make: it asserts that recording is DISABLED, and a branch that disabled it
// silently would satisfy it exactly.
//
// WHY THAT MATTERS HERE MORE THAN MOST PLACES. newRecorder's own comment records
// what the silence cost: on a Linux runner the medallion stack runs the emulator
// as a non-root user, the bind-mounted directory belongs to the runner, the open
// failed, the suite went GREEN having recorded nothing, and the failure
// surfaced a job later in the aggregate conformance gate as seven routes
// missing traffic -- pointing at the routes rather than at the suite that had
// stopped recording. The log line is the entire fix for that, and until this
// test it was asserted by nothing: 29 of the 30 log call sites in this tree had
// no test at all, and this was one of them.
//
// So the LINE is the observable, not the nil return. It must name its subsystem
// (`record: `, the tag scripts/check_logging_quality.py now enforces, so a grep
// for one subsystem in a compose log selects it) and it must name the variable,
// because "could not be opened" with no path is a sentence that cannot be acted
// on.
func TestAnUnwritableRecordingPathSaysSo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-dir", "r.jsonl")
	t.Setenv("FABRIC_RECORD_RESPONSES", path)

	buf := &bytes.Buffer{}
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })

	if newRecorder() != nil {
		t.Fatal("an unopenable path should disable recording")
	}

	out := buf.String()
	if out == "" {
		t.Fatal("recording was disabled SILENTLY. A suite then passes having " +
			"recorded nothing, and the conformance gate fails a job later " +
			"naming the routes instead of the cause.")
	}
	if !strings.HasPrefix(out, "record: ") {
		t.Errorf("the line does not name its subsystem, so no grep selects it "+
			"out of a compose log: %q", out)
	}
	for _, want := range []string{"FABRIC_RECORD_RESPONSES", path} {
		if !strings.Contains(out, want) {
			t.Errorf("the line does not name %q, so a reader cannot act on it: %q",
				want, out)
		}
	}
}

func TestADocumentedSurfaceIsRecordedWithItsBody(t *testing.T) {
	s, path := recorderFor(t)
	serve(s, "GET", "/v1/workspaces", `{"value":[]}`, http.StatusOK)

	got := lines(t, path)
	if len(got) != 1 {
		t.Fatalf("recorded %d lines, want 1", len(got))
	}
	if got[0].Method != "GET" || got[0].Path != "/v1/workspaces" || got[0].Status != 200 {
		t.Errorf("recorded %+v", got[0])
	}
	if string(got[0].Body) != `{"value":[]}` {
		t.Errorf("body = %s", got[0].Body)
	}
}

// TestTheStatusIsTheOneTheHandlerWrote, not the 200 the wrapper starts with.
// A recorder that always said 200 would hide every undocumented-status finding
// the conformance checker exists to make.
func TestTheStatusIsTheOneTheHandlerWrote(t *testing.T) {
	s, path := recorderFor(t)
	serve(s, "GET", "/v1/workspaces/x", `{"error":{}}`, http.StatusNotFound)

	got := lines(t, path)
	if len(got) != 1 || got[0].Status != http.StatusNotFound {
		t.Fatalf("recorded %+v, want status 404", got)
	}
}

func TestOnlyDocumentedSurfacesAreRecorded(t *testing.T) {
	cases := map[string]bool{
		"/v1/workspaces":                                       true,
		"/v1.0/myorg/groups":                                   true,
		"/_emulator/portal/workspaces":                         false, // this emulator's own portal
		"/onelake/ws/item/Files/x":                             false, // ADLS/Blob, witnessed elsewhere
		"/powerbi/globalservice/v201606/environments/discover": false,
		"/": false,
	}
	for path, want := range cases {
		if got := recordable(path); got != want {
			t.Errorf("recordable(%q) = %v, want %v", path, got, want)
		}
	}
}

// TestANonJSONBodyIsRecordedWithoutOne. A surface can answer with something
// that is not JSON; the status is still worth having, and writing invalid JSON
// into a JSONL file would make the whole recording unreadable.
func TestANonJSONBodyIsRecordedWithoutOne(t *testing.T) {
	s, path := recorderFor(t)
	serve(s, "GET", "/v1/whatever", "not json at all", http.StatusOK)

	got := lines(t, path)
	if len(got) != 1 {
		t.Fatalf("recorded %d lines, want 1", len(got))
	}
	if len(got[0].Body) != 0 {
		t.Errorf("body = %s, want omitted", got[0].Body)
	}
}

// TestAnOversizedBodyIsDroppedAndTheStatusKept. A OneLake read can be
// arbitrarily large and no schema describes it; buffering it would cost memory
// to record something the checker cannot use.
func TestAnOversizedBodyIsDroppedAndTheStatusKept(t *testing.T) {
	s, path := recorderFor(t)
	serve(s, "GET", "/v1/big", `{"a":"`+strings.Repeat("x", captureLimit)+`"}`, http.StatusOK)

	got := lines(t, path)
	if len(got) != 1 || got[0].Status != 200 {
		t.Fatalf("recorded %+v", got)
	}
	if len(got[0].Body) != 0 {
		t.Error("an oversized body should be dropped, not buffered")
	}
}

// TestTheResponseStillReachesTheClient. The wrapper sits in front of every
// documented surface, so a bug here breaks the emulator rather than the
// recording.
func TestTheResponseStillReachesTheClient(t *testing.T) {
	s, _ := recorderFor(t)
	rec := httptest.NewRecorder()
	h := s.record(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"w1"}`))
	}))
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/workspaces", nil))

	if rec.Code != http.StatusCreated || rec.Body.String() != `{"id":"w1"}` {
		t.Fatalf("client saw %d %q", rec.Code, rec.Body.String())
	}
}

// TestNoHeadersAreEverRecorded is the privacy assertion, and the one worth
// breaking a build over: the Authorization header carries a bearer token, and
// the recording is uploaded as a CI artifact.
func TestNoHeadersAreEverRecorded(t *testing.T) {
	s, path := recorderFor(t)
	h := s.record(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-ms-operation-id", "op-1")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	req := httptest.NewRequest("GET", "/v1/workspaces", nil)
	req.Header.Set("Authorization", "Bearer super-secret-token")
	h.ServeHTTP(httptest.NewRecorder(), req)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"super-secret-token", "Authorization", "Bearer", "x-ms-operation-id"} {
		if strings.Contains(string(raw), forbidden) {
			t.Errorf("the recording contains %q:\n%s", forbidden, raw)
		}
	}
}

// TestRecordingIsOffByDefaultInTheHandler: a Server with no recorder must not
// wrap at all, so the ordinary path costs nothing.
func TestTheHandlerIsUnwrappedWhenRecordingIsOff(t *testing.T) {
	s := &Server{}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	if got := s.record(inner); got == nil {
		t.Fatal("record returned nil")
	}
}
