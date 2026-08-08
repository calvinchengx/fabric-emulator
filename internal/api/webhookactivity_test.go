package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// The receiver: records the callBackUri it was handed and, when told to,
// calls it back through the API handler — the same route an external service
// would POST, exercised without a network.
type webhookReceiver struct {
	uri  atomic.Value // string: the callBackUri from the initial call's body
	srv  *httptest.Server
	seen atomic.Int32
	// onCall runs INSIDE the receiver's handler, before it answers. It exists
	// to make an interleaving an input instead of something a test hopes for:
	// whatever it does happens strictly between the activity making its call
	// and that call returning.
	onCall func()
}

func newWebhookReceiver(t *testing.T) *webhookReceiver {
	t.Helper()
	r := &webhookReceiver{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(req.Body).Decode(&body)
		uri, _ := body["callBackUri"].(string)
		if uri == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"no callBackUri in body"}`))
			return
		}
		r.uri.Store(uri)
		if r.onCall != nil {
			r.onCall()
		}
		r.seen.Add(1)
		_, _ = w.Write([]byte(`{"accepted":true}`))
	}))
	t.Cleanup(r.srv.Close)
	return r
}

// callBack POSTs the recorded callBackUri into the API, as the receiver would.
func (r *webhookReceiver) callBack(t *testing.T, a *API, body string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for r.uri.Load() == nil {
		if time.Now().After(deadline) {
			t.Fatal("the initial call never delivered a callBackUri")
		}
		time.Sleep(5 * time.Millisecond)
	}
	uri := r.uri.Load().(string)
	parts := strings.Split(strings.TrimPrefix(uri, "/v1/workspaces/"), "/")
	// parts: wid, "items", iid, "jobs", "instances", jid, "webhookcallbacks", token
	// The handler is principal-less by design (the token IS the credential),
	// so it is driven directly rather than through do()'s auth-shaped helper.
	req := httptest.NewRequest("POST", "/x", strings.NewReader(body))
	for k, v := range map[string]string{
		"wid": parts[0], "iid": parts[2], "jid": parts[5], "token": parts[7],
	} {
		req.SetPathValue(k, v)
	}
	w := httptest.NewRecorder()
	a.webhookCallbackHandler(w, req)
	return w.Code
}

func webhookPipeline(tp string) string {
	return `{"properties":{"activities":[
      {"name":"Hook","type":"WebHook","typeProperties":{` + tp + `}}]}}`
}

// TestWebHookParksUntilTheCallback: the defining half, witnessed end to end —
// the job stays open after the initial POST succeeds, and ONLY the callback
// completes it; the callback body's fields surface in the activity output.
func TestWebHookParksUntilTheCallback(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	rcv := newWebhookReceiver(t)

	pl := createPipeline(t, st, ws.ID, webhookPipeline(
		`"url":"`+rcv.srv.URL+`","method":"POST","body":{"job":"nightly"}`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")

	// The initial call has been made and the activity is PARKED: the job must
	// not be terminal, no matter how long the receiver sits on the callback.
	rcv.callBackWait(t)
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s == "Completed" || s == "Failed" {
		t.Fatalf("job = %s while the webhook was parked awaiting its callback", s)
	}

	if code := rcv.callBack(t, a, `{"approved":true}`); code != http.StatusOK {
		t.Fatalf("callback = %d, want 200", code)
	}
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		t.Fatalf("job = %s after the callback, want Completed", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	if v := outputOf(runs, "Hook")["approved"]; v != true {
		t.Fatalf("callback body did not reach the output: %+v", outputOf(runs, "Hook"))
	}
}

// callBackWait blocks until the receiver has seen the initial call.
func (r *webhookReceiver) callBackWait(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for r.seen.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the webhook's initial call never arrived")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestWebHookTimesOutOnTheVirtualClock: freeze the clock, advance past the
// deadline, and the park MUST expire with no real time passing — the repo's
// no-sleep rule applied to a parked activity. This is the test that fails if
// the park only arms a real-time timer.
func TestWebHookTimesOutOnTheVirtualClock(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	rcv := newWebhookReceiver(t)
	st.Clock.Freeze()
	t.Cleanup(st.Clock.Unfreeze)

	pl := createPipeline(t, st, ws.ID, webhookPipeline(
		`"url":"`+rcv.srv.URL+`","method":"POST","timeout":"00:10:00"`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	rcv.callBackWait(t)

	st.Clock.Advance(601)
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job = %s after the virtual deadline passed, want Failed", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	e, _ := runs[0]["error"].(string)
	if !strings.Contains(e, "never called the callBackUri back") {
		t.Fatalf("timeout error %q does not say what never happened", e)
	}
	// A callback after expiry finds nothing waiting: the token was consumed.
	if code := rcv.callBack(t, a, `{}`); code != http.StatusNotFound {
		t.Fatalf("late callback = %d, want 404", code)
	}
}

// TestWebHookReportStatusOnCallBack: the schema's consumption rule — a
// reported non-2xx statusCode fails the activity with the reported error.
func TestWebHookReportStatusOnCallBack(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	rcv := newWebhookReceiver(t)

	pl := createPipeline(t, st, ws.ID, webhookPipeline(
		`"url":"`+rcv.srv.URL+`","method":"POST","reportStatusOnCallBack":true`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if code := rcv.callBack(t, a, `{"statusCode":500,"error":"downstream exploded"}`); code != http.StatusOK {
		t.Fatalf("callback = %d", code)
	}
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job = %s, want Failed from the reported statusCode", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	e, _ := runs[0]["error"].(string)
	if !strings.Contains(e, "downstream exploded") {
		t.Fatalf("error %q does not carry the reported error", e)
	}
}

// TestWebHookSchemaRules: refusals by name, per the ADF schema.
func TestWebHookSchemaRules(t *testing.T) {
	for _, tc := range []struct{ name, tp, wantErr string }{
		{"method outside enum", `"url":"http://x","method":"GET"`, "POST only"},
		{"missing url", `"method":"POST"`, "url is required"},
		{"non-object body", `"url":"http://x","method":"POST","body":"text"`, "nowhere to ride"},
		{"bad timeout", `"url":"http://x","method":"POST","timeout":"tomorrow"`, "not D.HH:MM:SS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, st := newAPI(t)
			ws := seedWorkspace(t, st)
			pl := createPipeline(t, st, ws.ID, webhookPipeline(tc.tp))
			_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
			if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
				t.Fatalf("%s = %s, want Failed", tc.name, s)
			}
			_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
			e, _ := runs[0]["error"].(string)
			if !strings.Contains(e, tc.wantErr) {
				t.Errorf("error %q does not carry %q", e, tc.wantErr)
			}
		})
	}
}

var _ = store.NewID // keep the import honest if helpers move

// scriptedClock forces the interleaving no integration test can: its Now()
// applies a pending mutation AFTER returning the pre-mutation value, modelling
// a clock advance that lands exactly between the read and (under the buggy
// ordering) the subscribe.
type scriptedClock struct {
	now      int64
	pending  int64         // applied by the NEXT Now() call, after it returns
	changed  chan struct{} // the channel a pre-read subscriber holds
	fired    bool
	nowCalls int
}

func (c *scriptedClock) Now() int64 {
	c.nowCalls++
	v := c.now
	if c.pending != 0 && !c.fired {
		// The mutation lands "now": time moves and the current change channel
		// closes — exactly what clock.Clock does under its lock.
		c.now += c.pending
		c.fired = true
		close(c.changed)
	}
	return v
}

func (c *scriptedClock) Changed() <-chan struct{} {
	if c.fired {
		// Post-mutation subscribers get a fresh, open channel — the buggy
		// ordering ends up holding this one and never wakes.
		return make(chan struct{})
	}
	return c.changed
}

// TestParkUntilSubscribesBeforeReading pins the ordering deterministically.
// Under subscribe-first, the park holds the channel the mutation closes and
// wakes immediately; under read-first (the bug fe found), it subscribes after
// the mutation, holds a channel nobody will close, and sleeps toward the full
// real-time deadline. The window in the real clock is nanoseconds wide, so a
// stress loop cannot force it honestly — severing notifyChange proves the
// mechanism is load-bearing but says nothing about its sequencing, which is
// why this test exists separately from the mutation check.
func TestParkUntilSubscribesBeforeReading(t *testing.T) {
	clk := &scriptedClock{now: 0, pending: 601, changed: make(chan struct{})}
	ch := make(chan webhookCallback)

	done := make(chan bool, 1)
	go func() {
		_, ok := parkUntil(clk, 600, ch)
		done <- ok
	}()

	select {
	case ok := <-done:
		if ok {
			t.Fatal("park reported a callback that never happened")
		}
		// Expired via the mutation the held channel announced: correct.
	case <-time.After(2 * time.Second):
		t.Fatal("park missed the clock mutation — it read the clock before subscribing")
	}
}

// TestWebHookDeadlineIsFixedBeforeTheCall forces the interleaving that made
// TestWebHookTimesOutOnTheVirtualClock flaky on a loaded macOS runner, instead
// of sampling for it.
//
// The receiver advances virtual time past the timeout WHILE HOLDING the
// initial call open. With the deadline computed after that call returned, the
// advance was absorbed — the deadline was then measured from the already
// advanced clock, the timeout never fired, and the job sat InProgress until
// the poll helper gave up. The tell was the duration: a failure taking exactly
// the helper's five-second budget is a lost timeout, not a slow machine.
//
// Sampling could not have caught this reliably: the window is the microseconds
// between an HTTP response and the next statement, and on an unloaded laptop
// the test passes 8 times out of 8. Making the advance happen inside the
// handler closes that gap by construction — it is the same discipline the
// scripted clock applies to parkUntil's subscribe-before-read ordering.
func TestWebHookDeadlineIsFixedBeforeTheCall(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	rcv := newWebhookReceiver(t)
	st.Clock.Freeze()
	t.Cleanup(st.Clock.Unfreeze)

	// The whole point: the advance lands mid-call, in the window the old
	// ordering lost it in.
	rcv.onCall = func() { st.Clock.Advance(601) }

	pl := createPipeline(t, st, ws.ID, webhookPipeline(
		`"url":"`+rcv.srv.URL+`","method":"POST","timeout":"00:10:00"`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")

	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job = %s — virtual time moved past the timeout during the call and the "+
			"park absorbed it, which means the deadline was read after the call rather "+
			"than fixed with the rest of the park", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	if e, _ := runs[0]["error"].(string); !strings.Contains(e, "never called the callBackUri back") {
		t.Fatalf("error %q does not say what never happened", e)
	}
}
