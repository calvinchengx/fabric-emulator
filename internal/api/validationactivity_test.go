package api

import (
	"strings"
	"testing"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

func validationPipeline(tp string) string {
	return `{"properties":{"activities":[
      {"name":"Wait","type":"Validation","typeProperties":{` + tp + `}}]}}`
}

// TestValidationPassesOnDataThatIsThere: the ordinary case, and the one that
// has to stay cheap — an existing path validates without any clock games.
func TestValidationPassesOnDataThatIsThere(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lh := seedLakehouse(t, st, ws.ID, "lake")
	seedFile(t, st, ws.ID, lh.ID, "Files/in/day.csv", []byte("id,name\n1,ada\n"))

	pl := createPipeline(t, st, ws.ID, validationPipeline(
		`"dataset":{"itemId":"`+lh.ID+`","path":"Files/in/day.csv"},"timeout":"00:01:00"`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
		t.Fatalf("job = %s; runs=%+v", s, runs)
	}
	out := func() map[string]any {
		_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
		return outputOf(runs, "Wait")
	}()
	if out["exists"] != true || out["itemName"] != "day.csv" || out["itemType"] != "File" {
		t.Fatalf("output does not describe the validated path: %+v", out)
	}
	if sz, _ := out["size"].(float64); int(sz) != len("id,name\n1,ada\n") {
		t.Fatalf("size = %v, want the real byte count: %+v", out["size"], out)
	}
}

// TestValidationFailsWhenTheDataNeverArrives is the assertion the whole
// activity exists for, and the one the stubbed default got backwards: a
// Validation that passes on absent data hands the pipeline a guard's blessing
// to read a file that is not there. The deadline is measured on the VIRTUAL
// clock, so this takes no real time.
func TestValidationFailsWhenTheDataNeverArrives(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lh := seedLakehouse(t, st, ws.ID, "lake")
	st.Clock.Freeze()
	t.Cleanup(st.Clock.Unfreeze)

	pl := createPipeline(t, st, ws.ID, validationPipeline(
		`"dataset":{"itemId":"`+lh.ID+`","path":"Files/in/never.csv"},
         "timeout":"00:10:00","sleep":30`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")

	s := advanceUntilTerminal(t, a, st, ws.ID, pl.ID, jid)
	if s != "Failed" {
		t.Fatalf("job = %s, want Failed — absent data must not validate", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	e, _ := runs[0]["error"].(string)
	if !strings.Contains(e, "never.csv") || !strings.Contains(e, "does not exist") {
		t.Fatalf("timeout error %q does not say what was still wrong", e)
	}
	if !strings.Contains(e, "not there") {
		t.Fatalf("timeout error %q does not say why failing is the point", e)
	}
}

// advanceUntilTerminal drives the virtual clock forward while the job runs, so
// a parked activity reaches its deadline without any real waiting. Advancing
// in a loop rather than once removes the need to detect the exact moment the
// activity subscribed — an advance that lands early simply moves the deadline
// with it, and a later one passes it.
func advanceUntilTerminal(t *testing.T, a *API, st *store.Store, wid, iid, jid string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s := jobStatus(t, a, wid, iid, jid); s != "InProgress" && s != "NotStarted" {
			return s
		}
		st.Clock.Advance(120)
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("job never reached a terminal state")
	return ""
}

// TestValidationHonoursItsPredicates: minimumSize and childItems are the two
// properties that make Validation more than an existence check, and each is
// asserted in BOTH directions — a satisfied case that must pass and a
// violated case that must not. A one-sided test would pass on an
// implementation that parsed the field and ignored it.
func TestValidationHonoursItsPredicates(t *testing.T) {
	for _, tc := range []struct {
		name, tp string
		seed     func(t *testing.T, st *store.Store, wsID, lhID string)
		wantPass bool
		wantErr  string
	}{
		{
			name: "minimumSize satisfied",
			tp:   `"dataset":{"itemId":"LH","path":"Files/in/big.csv"},"minimumSize":5`,
			seed: func(t *testing.T, st *store.Store, ws, lh string) {
				seedFile(t, st, ws, lh, "Files/in/big.csv", []byte("0123456789"))
			},
			wantPass: true,
		},
		{
			name: "minimumSize violated",
			tp:   `"dataset":{"itemId":"LH","path":"Files/in/small.csv"},"minimumSize":100`,
			seed: func(t *testing.T, st *store.Store, ws, lh string) {
				seedFile(t, st, ws, lh, "Files/in/small.csv", []byte("tiny"))
			},
			wantErr: "below the minimumSize",
		},
		{
			name: "a folder with no predicates",
			tp:   `"dataset":{"itemId":"LH","path":"Files/landing"}`,
			seed: func(t *testing.T, st *store.Store, ws, lh string) {
				seedFile(t, st, ws, lh, "Files/landing/a.csv", []byte("x"))
			},
			wantPass: true,
		},
		{
			name: "childItems true satisfied",
			tp:   `"dataset":{"itemId":"LH","path":"Files/landing"},"childItems":true`,
			seed: func(t *testing.T, st *store.Store, ws, lh string) {
				seedFile(t, st, ws, lh, "Files/landing/a.csv", []byte("x"))
			},
			wantPass: true,
		},
		{
			name: "childItems true on an empty folder",
			tp:   `"dataset":{"itemId":"LH","path":"Files/empty"},"childItems":true`,
			seed: func(t *testing.T, st *store.Store, ws, lh string) {
				seedFile(t, st, ws, lh, "Files/empty/.keep/inner", []byte("x"))
			},
			wantErr: "holds no files yet",
		},
		{
			name: "childItems false on a full folder",
			tp:   `"dataset":{"itemId":"LH","path":"Files/landing"},"childItems":false`,
			seed: func(t *testing.T, st *store.Store, ws, lh string) {
				seedFile(t, st, ws, lh, "Files/landing/a.csv", []byte("x"))
			},
			wantErr: "asks for an empty folder",
		},
		{
			name: "minimumSize against a folder",
			tp:   `"dataset":{"itemId":"LH","path":"Files/landing"},"minimumSize":1`,
			seed: func(t *testing.T, st *store.Store, ws, lh string) {
				seedFile(t, st, ws, lh, "Files/landing/a.csv", []byte("x"))
			},
			wantErr: "minimumSize asks about a file",
		},
		{
			name: "childItems against a file",
			tp:   `"dataset":{"itemId":"LH","path":"Files/in/one.csv"},"childItems":true`,
			seed: func(t *testing.T, st *store.Store, ws, lh string) {
				seedFile(t, st, ws, lh, "Files/in/one.csv", []byte("x"))
			},
			wantErr: "childItems asks about a folder",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, st := newAPI(t)
			ws := seedWorkspace(t, st)
			lh := seedLakehouse(t, st, ws.ID, "lake")
			tc.seed(t, st, ws.ID, lh.ID)
			st.Clock.Freeze()
			t.Cleanup(st.Clock.Unfreeze)

			pl := createPipeline(t, st, ws.ID, validationPipeline(
				strings.ReplaceAll(tc.tp, `"LH"`, `"`+lh.ID+`"`)+`,"timeout":"00:05:00","sleep":30`))
			_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")

			if tc.wantPass {
				if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Completed" {
					_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
					t.Fatalf("job = %s, want Completed; runs=%+v", s, runs)
				}
				return
			}
			if s := advanceUntilTerminal(t, a, st, ws.ID, pl.ID, jid); s != "Failed" {
				t.Fatalf("job = %s, want Failed", s)
			}
			_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
			if e, _ := runs[0]["error"].(string); !strings.Contains(e, tc.wantErr) {
				t.Fatalf("error %q does not carry %q", e, tc.wantErr)
			}
		})
	}
}

// TestValidationInputSurface: the shapes a definition can get wrong, each
// failing before any waiting starts — a malformed `sleep` must not park for a
// week first.
func TestValidationInputSurface(t *testing.T) {
	for _, tc := range []struct{ name, tp, wantErr string }{
		{"missing dataset", `"timeout":"00:01:00"`, "no location"},
		{"bad timeout", `"dataset":{"itemId":"LH","path":"Files/x"},"timeout":"soon"`,
			"is not D.HH:MM:SS"},
		{"timeout expr", `"dataset":{"itemId":"LH","path":"Files/x"},"timeout":"@nope(1)"`, "timeout"},
		{"bad sleep", `"dataset":{"itemId":"LH","path":"Files/x"},"sleep":0`,
			"positive number of seconds"},
		{"sleep expr", `"dataset":{"itemId":"LH","path":"Files/x"},"sleep":"@nope(1)"`, "sleep"},
		{"bad minimumSize", `"dataset":{"itemId":"LH","path":"Files/x"},"minimumSize":-5`,
			"non-negative number of bytes"},
		{"minimumSize expr", `"dataset":{"itemId":"LH","path":"Files/x"},"minimumSize":"@nope(1)"`,
			"minimumSize"},
		{"bad childItems", `"dataset":{"itemId":"LH","path":"Files/x"},"childItems":"yes"`,
			"must be true or false"},
		{"childItems expr", `"dataset":{"itemId":"LH","path":"Files/x"},"childItems":"@nope(1)"`,
			"childItems"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, st := newAPI(t)
			ws := seedWorkspace(t, st)
			lh := seedLakehouse(t, st, ws.ID, "lake")
			pl := createPipeline(t, st, ws.ID,
				validationPipeline(strings.ReplaceAll(tc.tp, `"LH"`, `"`+lh.ID+`"`)))
			_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
			if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
				t.Fatalf("%s = %s, want Failed", tc.name, s)
			}
			_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
			if e, _ := runs[0]["error"].(string); !strings.Contains(e, tc.wantErr) {
				t.Errorf("error %q does not carry %q", e, tc.wantErr)
			}
		})
	}
}

// --- validationWait, driven directly -----------------------------------------
//
// The loop's timing behaviour is unit-tested rather than sampled through a
// pipeline: "it re-checked" and "it stopped at the deadline" are claims about
// an interleaving, and an integration test can only observe the outcome.

// TestValidationWaitRetriesUntilTheDataArrives: the defining behaviour. A
// check that fails twice and then passes must be RE-RUN, not answered once —
// an implementation that checked a single time would satisfy every
// integration test above whose data is seeded up front.
func TestValidationWaitRetriesUntilTheDataArrives(t *testing.T) {
	clk := &countingClock{}
	calls := 0
	ok, why := validationWait(clk, 1000, 10, func() (bool, string) {
		calls++
		return calls >= 3, "not yet"
	})
	if !ok || why != "" {
		t.Fatalf("validationWait = %v %q, want a pass once the data arrived", ok, why)
	}
	if calls != 3 {
		t.Fatalf("check ran %d time(s), want 3 — the wait must re-evaluate", calls)
	}
}

// TestValidationWaitStopsAtTheDeadline: it gives up when virtual time runs
// out, and it carries the LAST reason rather than a generic timeout, so the
// pipeline author learns which predicate was still unsatisfied.
func TestValidationWaitStopsAtTheDeadline(t *testing.T) {
	clk := &countingClock{step: 400}
	ok, why := validationWait(clk, 1000, 10, func() (bool, string) {
		return false, "still empty"
	})
	if ok {
		t.Fatal("validationWait passed on data that never arrived")
	}
	if why != "still empty" {
		t.Fatalf("reason = %q, want the last failure's own reason", why)
	}
}

// TestValidationWaitSubscribesBeforeReading pins the ordering with the same
// scripted clock the WebHook park uses. Under subscribe-first the wait holds
// the channel the mutation closes and wakes at once; under read-first it
// subscribes after, holds a channel nobody closes, and sleeps toward a real
// `sleep` it should never have used. The window is nanoseconds wide in the
// real clock, so the interleaving is made an input instead of sampled for.
func TestValidationWaitSubscribesBeforeReading(t *testing.T) {
	clk := &scriptedClock{now: 0, pending: 601, changed: make(chan struct{})}
	done := make(chan bool, 1)
	start := time.Now()
	go func() {
		ok, _ := validationWait(clk, 600, 30, func() (bool, string) { return false, "absent" })
		done <- ok
	}()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("the wait passed on data that never arrived")
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("the wait took %s of real time — it slept through the clock change", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the wait never woke: it subscribed to the clock AFTER reading it, so the " +
			"change that moved time past the deadline closed a channel nobody was holding")
	}
}

// countingClock advances a fixed step on every Now(), which makes the
// deadline arithmetic deterministic without any real time passing. Its
// Changed() is always closed so the loop never blocks.
type countingClock struct {
	now  int64
	step int64
}

func (c *countingClock) Now() int64 {
	v := c.now
	c.now += c.step
	return v
}

func (c *countingClock) Changed() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

// TestValidationWaitClampsTheSleepToTheDeadline: `sleep` is a delay between
// attempts, not a licence to overshoot. A 100-second sleep against 3 seconds
// of remaining time must wait 3, or the activity sits past its own timeout and
// reports the failure late — which for a guard is the difference between
// blocking a bad run and blocking it after the fact.
func TestValidationWaitClampsTheSleepToTheDeadline(t *testing.T) {
	clk := &countingClock{step: 1}
	start := time.Now()
	ok, why := validationWait(clk, 3, 100, func() (bool, string) { return false, "absent" })
	if ok || why != "absent" {
		t.Fatalf("validationWait = %v %q, want a clamped give-up", ok, why)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("the wait took %s — it slept the full sleep past its deadline", elapsed)
	}
}

// TestValidationPassesOnAFolderThatIsThere: the output for a folder, which has
// no size and is not addressable by GetOneLakePath at all — OneLake keeps no
// row for an implicit directory, so an activity that only looked one up would
// report a landing folder as absent forever.
func TestValidationPassesOnAFolderThatIsThere(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lh := seedLakehouse(t, st, ws.ID, "lake")
	seedFile(t, st, ws.ID, lh.ID, "Files/landing/a.csv", []byte("x"))

	pl := createPipeline(t, st, ws.ID, validationPipeline(
		`"dataset":{"itemId":"`+lh.ID+`","path":"Files/landing"},"childItems":true`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
		t.Fatalf("job = %s; runs=%+v", s, runs)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	out := outputOf(runs, "Wait")
	if out["exists"] != true || out["itemType"] != "Folder" || out["itemName"] != "landing" {
		t.Fatalf("output does not describe the folder: %+v", out)
	}
	if _, hasSize := out["size"]; hasSize {
		t.Fatalf("a folder was given a size: %+v", out)
	}
}
