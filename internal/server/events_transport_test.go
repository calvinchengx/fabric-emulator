package server_test

// The flow stream's transport edges: what happens when the connection is not
// streamable, when the client goes away mid-stream, and when a client falls far
// enough behind that the bus drops events on it.
//
// events_test.go drives this endpoint over real HTTP, which is the right shape
// for "does a running pipeline appear on the wire" but cannot reach any of
// this: net/http always supplies a Flusher, and a healthy httptest client never
// stalls. Driving Server.Handler() with a ResponseWriter under test control is
// what makes these observable.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The events under test need only an id on the wire, not a real workspace.
const streamWS = "ws-under-test"

// allSeqs returns the sequence numbers currently replayable, in order.
func allSeqs(t *testing.T, f *fixture) []int64 {
	t.Helper()
	var out []int64
	for _, ev := range f.srv.Store.Replay(0) {
		out = append(out, ev.Seq)
	}
	return out
}

// waitForSeqs blocks until the bus has DISPATCHED at least n events past
// `since`. Publishing is asynchronous — it hands the event to a queue and
// returns — so anything that reasons about sequence numbers has to wait for the
// dispatch goroutine rather than assume it already ran.
func waitForSeqs(t *testing.T, f *fixture, since int64, n int, what string) []int64 {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		var out []int64
		for _, ev := range f.srv.Store.Replay(since) {
			out = append(out, ev.Seq)
		}
		if len(out) >= n {
			return out
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s: the bus dispatched %d of %d", what, len(out), n)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(20 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func waitForClose(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// nonFlusher is a ResponseWriter that is deliberately NOT an http.Flusher.
// httptest.ResponseRecorder implements Flush, so it cannot stand in here.
type nonFlusher struct {
	header http.Header
	body   strings.Builder
	code   int
}

func (n *nonFlusher) Header() http.Header {
	if n.header == nil {
		n.header = http.Header{}
	}
	return n.header
}
func (n *nonFlusher) Write(b []byte) (int, error) { return n.body.Write(b) }
func (n *nonFlusher) WriteHeader(c int)           { n.code = c }

// gatedWriter is a streaming ResponseWriter whose writes can be held and then
// released, and which can be made to fail like a client that hung up.
type gatedWriter struct {
	mu      sync.Mutex
	body    strings.Builder
	code    int
	header  http.Header
	gate    chan struct{} // nil = never blocks; closed = released
	err     error         // returned by every Write once set
	written chan struct{} // signalled on the first write
	stalled chan struct{} // signalled when a write is about to block
	once    sync.Once
	onceB   sync.Once
}

func newGatedWriter() *gatedWriter {
	return &gatedWriter{header: http.Header{},
		written: make(chan struct{}), stalled: make(chan struct{})}
}

func (g *gatedWriter) Header() http.Header { return g.header }
func (g *gatedWriter) WriteHeader(c int)   { g.code = c }
func (g *gatedWriter) Flush()              {}

func (g *gatedWriter) Write(b []byte) (int, error) {
	g.mu.Lock()
	gate, err := g.gate, g.err
	g.mu.Unlock()
	if err != nil {
		return 0, err
	}
	if gate != nil {
		g.onceB.Do(func() { close(g.stalled) })
		<-gate // held until release()
	}
	g.mu.Lock()
	n, _ := g.body.Write(b)
	g.mu.Unlock()
	g.once.Do(func() { close(g.written) })
	return n, nil
}

func (g *gatedWriter) hold()            { g.mu.Lock(); g.gate = make(chan struct{}); g.mu.Unlock() }
func (g *gatedWriter) release()         { g.mu.Lock(); close(g.gate); g.gate = nil; g.mu.Unlock() }
func (g *gatedWriter) failWith(e error) { g.mu.Lock(); g.err = e; g.mu.Unlock() }
func (g *gatedWriter) text() string     { g.mu.Lock(); defer g.mu.Unlock(); return g.body.String() }
func (g *gatedWriter) status() int      { g.mu.Lock(); defer g.mu.Unlock(); return g.code }

// TestFlowStreamRefusesAConnectionItCannotStream: the handler asserts a Flusher
// up front. Without the check it would stream into a writer that never reaches
// the client and the caller would hang on a connection that looks healthy — a
// clean 500 naming the reason is the recoverable outcome.
func TestFlowStreamRefusesAConnectionItCannotStream(t *testing.T) {
	f := newFixture(t)
	w := &nonFlusher{}
	req := httptest.NewRequest("GET", "/_emulator/events", nil)

	f.srv.Handler().ServeHTTP(w, req)

	if w.code != http.StatusInternalServerError {
		t.Fatalf("status = %d; want 500", w.code)
	}
	if !strings.Contains(w.body.String(), "streaming unsupported") {
		t.Errorf("body does not name the cause: %s", w.body.String())
	}
	// It must refuse rather than emit a stream nobody can read.
	if strings.Contains(w.body.String(), "data: ") {
		t.Errorf("refused connection still emitted SSE frames: %s", w.body.String())
	}
}

// TestFlowStreamResumesFromLastEventID covers the reconnect path. EventSource
// sets Last-Event-ID itself, so this is the header a browser actually sends
// after a dropped connection — and it must win over a stale ?since= left in the
// URL the browser reconnects to, or every reconnect replays from the original
// query and shows the run from the top again.
func TestFlowStreamResumesFromLastEventID(t *testing.T) {
	f := newFixture(t)
	// Three events with known sequence numbers.
	for i := 0; i < 3; i++ {
		f.srv.Store.PublishJobEvent(streamWS, "item", fmt.Sprintf("job-%d", i),
			"Pipeline", "Manual", "Completed", "")
	}
	seqs := waitForSeqs(t, f, 0, 3, "the three seeded events")
	resumeAfter := seqs[len(seqs)-2] // replay must yield exactly the last one

	// ?since=0 says "from the beginning"; the header says "from resumeAfter".
	// The header is the truthful one — it is what the client last SAW.
	w := newGatedWriter()
	req := httptest.NewRequest("GET", "/_emulator/events?since=0", nil)
	req.Header.Set("Last-Event-ID", strconv.FormatInt(resumeAfter, 10))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.srv.Handler().ServeHTTP(w, req.WithContext(ctx)); close(done) }()

	waitFor(t, w.written, "the replayed frame")
	cancel()
	waitForClose(t, done, "the handler to return")

	body := w.text()
	last := seqs[len(seqs)-1]
	if !strings.Contains(body, "id: "+strconv.FormatInt(last, 10)) {
		t.Errorf("resume did not deliver event %d:\n%s", last, body)
	}
	// The decisive assertion: everything at or before the resume point must be
	// absent, or the reconnect duplicates what the client already displayed.
	if strings.Contains(body, "id: "+strconv.FormatInt(seqs[0], 10)) {
		t.Errorf("Last-Event-ID was ignored and ?since=0 replayed from the top:\n%s", body)
	}
}

// TestFlowStreamStopsWhenTheClientIsGone: once a write fails the peer is gone,
// so the handler must return instead of looping on a dead connection. Each such
// handler holds a bus subscription, and leaking them makes every publisher
// slower for the rest of the process's life.
func TestFlowStreamStopsWhenTheClientIsGone(t *testing.T) {
	f := newFixture(t)
	f.srv.Store.PublishJobEvent(streamWS, "item", "job-1", "Pipeline", "Manual", "Completed", "")
	waitForSeqs(t, f, 0, 1, "the seeded event to become replayable")

	// The keepalive is pushed out of reach on purpose. The fixture sets it to
	// 100ms, and a failing keepalive write ALSO ends the handler — so with the
	// default this test passes even when the event-send path ignores its write
	// error entirely, which is exactly the bug it is supposed to catch. With no
	// keepalive coming, returning at all requires the send path to check.
	f.srv.EventKeepalive = time.Hour

	w := newGatedWriter()
	w.failWith(fmt.Errorf("connection reset by peer"))
	req := httptest.NewRequest("GET", "/_emulator/events?since=0", nil)

	done := make(chan struct{})
	go func() { f.srv.Handler().ServeHTTP(w, req); close(done) }()

	// No cancellation: the handler must notice by itself, from the write alone.
	waitForClose(t, done, "the handler to give up on a dead connection")
}

// TestFlowStreamReportsADroppedGapEvenWhenFiltering is the invariant the source
// states as "a gap must never pass silently": the bus drops events on a
// subscriber that falls behind, and only this handler can tell the client. It
// is asserted under ?kinds= because that is where it is easy to get wrong — a
// client that asked for a subset still needs to know it missed some of that
// subset, so `dropped` is seeded into the filter rather than subjected to it.
func TestFlowStreamReportsADroppedGapEvenWhenFiltering(t *testing.T) {
	f := newFixture(t)

	since := int64(0)
	if s := allSeqs(t, f); len(s) > 0 {
		since = s[len(s)-1]
	}
	w := newGatedWriter()
	w.hold()
	req := httptest.NewRequest("GET",
		"/_emulator/events?kinds=job&since="+strconv.FormatInt(since, 10), nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.srv.Handler().ServeHTTP(w, req.WithContext(ctx)); close(done) }()

	// Wait until the handler is genuinely stuck in a write — the fixture's short
	// keepalive guarantees one arrives — before flooding. Releasing on a timer
	// instead races the dispatcher, which drains the subscription and drops
	// nothing at all.
	waitFor(t, w.stalled, "the handler to stall on a write")

	const flood = 400
	for i := 0; i < flood; i++ {
		f.srv.Store.PublishJobEvent(streamWS, "item", fmt.Sprintf("job-%d", i),
			"Pipeline", "Manual", "Completed", "")
	}
	// The flood must be DISPATCHED, not merely queued: a drop happens when the
	// dispatcher finds the subscriber's 256-deep channel full, so waiting for
	// the ring to show every event is what makes the gap certain rather than
	// likely. 400 > 256, so at least 144 events cannot have been delivered.
	waitForSeqs(t, f, since, flood, "the flood to reach the subscriber")
	w.release()

	deadline := time.After(20 * time.Second)
	for !strings.Contains(w.text(), "event: dropped") {
		select {
		case <-deadline:
			cancel()
			t.Fatalf("a client that fell behind was never told about the gap:\n%s",
				tail(w.text()))
		default:
			time.Sleep(20 * time.Millisecond)
		}
	}
	body := w.text()
	cancel()
	waitForClose(t, done, "the handler to return")

	// The notice must carry a count — "you missed some" without a number gives a
	// UI no way to say how much of the run it is missing.
	if !strings.Contains(body, `"dropped":`) {
		t.Errorf("the dropped notice carries no count:\n%s", tail(body))
	}
	// A dropped notice is generated here, not by the bus, so offering it as a
	// resume point would tell a reconnecting client to skip past real events.
	for _, frame := range strings.Split(body, "\n\n") {
		if strings.Contains(frame, "event: dropped") && strings.Contains(frame, "id: ") {
			t.Errorf("the dropped notice was offered as a resume point:\n%s", frame)
		}
	}
}

func tail(s string) string {
	if len(s) > 2000 {
		return "…" + s[len(s)-2000:]
	}
	return s
}
