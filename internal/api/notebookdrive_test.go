package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	osexec "os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAgent stands in for the Spark statement agent.
//
// A real engine would need Spark, which no unit test should require — but the
// thing under test here is not Spark. It is whether the emulator drives a
// notebook to a terminal state, in order, stopping where the notebook says to,
// and whether it can tell an intentional exit from a failure. All of that is
// visible at the agent's HTTP boundary.
type fakeAgent struct {
	mu sync.Mutex
	// code received, in order, so a test can assert the driver ran the cells it
	// was given and did not, say, run them twice or skip the prelude.
	got []string
	// reply decides what each statement returns; nil means an empty ok.
	reply func(code string) map[string]any
	// exit is what the notebook exited WITH; empty means it never called
	// notebook_exit. The probe is answered as JSON, like the real agent.
	exit string
}

func newFakeAgent(t *testing.T, a *API) *fakeAgent {
	t.Helper()
	f := &fakeAgent{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			writeJSON(w, 200, map[string]any{"state": "idle"})
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Session string `json:"session"`
			Code    string `json:"code"`
			Kind    string `json:"kind"`
		}
		_ = json.Unmarshal(body, &req)

		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Path {
		case "/statements":
			if strings.Contains(req.Code, "__nb_exit__ is not None") {
				// Model what the real agent does: the notebook's exit probe
				// PRINTS json, and run_code returns captured stdout.
				body := `{"exited":false,"value":""}`
				if f.exit != "" {
					body = `{"exited":true,"value":` + jsonQuote(f.exit) + `}`
				}
				writeJSON(w, 200, map[string]any{
					"status": "ok", "data": map[string]any{"text/plain": body},
				})
				return
			}
			f.got = append(f.got, req.Code)
			if f.reply != nil {
				if out := f.reply(req.Code); out != nil {
					writeJSON(w, 200, out)
					return
				}
			}
			writeJSON(w, 200, map[string]any{"status": "ok", "data": map[string]any{"text/plain": ""}})
		default: // /close, /register
			writeJSON(w, 200, map[string]any{"ok": true})
		}
	}))
	t.Cleanup(srv.Close)
	if err := a.SetLivyAgent(srv.URL); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fakeAgent) statements() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.got...)
}

// awaitJob polls for a terminal status. The driver runs in a goroutine because
// submitting a job returns 202 and the caller polls — so the test polls too,
// rather than sleeping a guessed interval and hoping.
func awaitJob(t *testing.T, a *API, wid, iid, jid string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s := jobStatus(t, a, wid, iid, jid); s != "InProgress" && s != "NotStarted" {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job never reached a terminal state")
	return ""
}

// TestEmulatorDrivesNotebookWhenAnAgentIsConfigured is the whole point: with an
// agent present, nothing external has to report for the job to finish.
//
// Before this, a RunNotebook job submitted against a published stack waited
// forever — the emulator parked it deliberately (a status derived from a clock
// is not evidence of execution) and the only engine that could release it was
// an e2e script that shipped in no artifact.
func TestEmulatorDrivesNotebookWhenAnAgentIsConfigured(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	nb := createNotebook(t, st, ws.ID, sampleNotebook)
	agent := newFakeAgent(t, a)

	_, jid := runJob(t, a, ws.ID, nb.ID, "jobType=RunNotebook", "")
	if s := awaitJob(t, a, ws.ID, nb.ID, jid); s != "Completed" {
		t.Fatalf("job status = %s", s)
	}

	run := notebookRunDetail(t, a, ws.ID, nb.ID, jid)
	if run.Status != "Completed" {
		t.Fatalf("run = %+v", run)
	}
	for _, c := range run.Cells {
		if c.Status != "Succeeded" {
			t.Fatalf("cell %d = %+v", c.Index, c)
		}
	}

	// The prelude first, then each code cell in order, and the markdown cell
	// nowhere — a notebook is not a script, and the order is the semantics.
	got := agent.statements()
	if len(got) != 3 {
		t.Fatalf("statements = %q", got)
	}
	if !strings.Contains(got[0], "def notebook_exit") {
		t.Fatalf("prelude was not installed first: %q", got[0])
	}
	if got[1] != "x = spark.range(3)" || got[2] != "SELECT 1" {
		t.Fatalf("cells ran wrong: %q", got[1:])
	}
}

// TestNotebookExitStopsTheRunAndCarriesItsValue.
//
// The agent reports every exception as a generic error, so an intentional exit
// and a real failure arrive identically. Getting this backwards would either
// mark a healthy notebook Failed, or — worse — run the cells after an exit and
// call the result Completed.
func TestNotebookExitStopsTheRunAndCarriesItsValue(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	nb := createNotebook(t, st, ws.ID, sampleNotebook)
	agent := newFakeAgent(t, a)

	// The first cell exits; the fake then answers the exit probe with a value.
	agent.reply = func(code string) map[string]any {
		if strings.Contains(code, "spark.range") {
			agent.exit = "42"
			return map[string]any{"status": "error", "ename": "Error", "evalue": "_NotebookExit: 42"}
		}
		return nil
	}

	_, jid := runJob(t, a, ws.ID, nb.ID, "jobType=RunNotebook", "")
	if s := awaitJob(t, a, ws.ID, nb.ID, jid); s != "Completed" {
		t.Fatalf("an exit is not a failure; job status = %s", s)
	}

	run := notebookRunDetail(t, a, ws.ID, nb.ID, jid)
	if run.ExitValue != "42" {
		t.Fatalf("exit value = %q", run.ExitValue)
	}
	if run.Cells[0].Status != "Succeeded" || run.Cells[0].Error != "" {
		t.Fatalf("the exiting cell should be clean: %+v", run.Cells[0])
	}
	// The SQL cell after the exit must NOT have run.
	for _, s := range agent.statements() {
		if s == "SELECT 1" {
			t.Fatal("execution continued past notebook_exit")
		}
	}
}

// TestNotebookCellFailureFailsTheJob: a genuine error is not an exit.
func TestNotebookCellFailureFailsTheJob(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	nb := createNotebook(t, st, ws.ID, sampleNotebook)
	agent := newFakeAgent(t, a)

	agent.reply = func(code string) map[string]any {
		if strings.Contains(code, "spark.range") {
			// exit stays "None" — nothing called notebook_exit.
			return map[string]any{"status": "error", "ename": "NameError", "evalue": "spark is not defined"}
		}
		return nil
	}

	_, jid := runJob(t, a, ws.ID, nb.ID, "jobType=RunNotebook", "")
	if s := awaitJob(t, a, ws.ID, nb.ID, jid); s != "Failed" {
		t.Fatalf("job status = %s", s)
	}
	run := notebookRunDetail(t, a, ws.ID, nb.ID, jid)
	if run.Cells[0].Status != "Failed" || !strings.Contains(run.Cells[0].Error, "spark is not defined") {
		t.Fatalf("cell = %+v", run.Cells[0])
	}
}

// TestNotebookWithoutAnAgentStillWaitsForACallback.
//
// The contract this feature is built on top of, not around: with no agent there
// is no engine, and a job that completed anyway would be back to reporting a
// status derived from nothing. The callback path must keep working untouched.
func TestNotebookWithoutAnAgentStillWaitsForACallback(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	nb := createNotebook(t, st, ws.ID, sampleNotebook)

	_, jid := runJob(t, a, ws.ID, nb.ID, "jobType=RunNotebook", "")
	time.Sleep(50 * time.Millisecond) // long enough for a driver to have run
	if s := jobStatus(t, a, ws.ID, nb.ID, jid); s == "Completed" || s == "Failed" {
		t.Fatalf("job reached %s with no engine attached", s)
	}
	if run := notebookRunDetail(t, a, ws.ID, nb.ID, jid); run.Status != "Pending" {
		t.Fatalf("run = %+v", run)
	}
}

// TestAnUnreachableAgentFailsTheJobRatherThanHanging.
//
// The failure mode this replaces was a notebook that ran forever. Swapping it
// for a different silent hang would be no improvement, so an agent that cannot
// be reached must land on the job, where the caller is looking.
func TestAnUnreachableAgentFailsTheJobRatherThanHanging(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	nb := createNotebook(t, st, ws.ID, sampleNotebook)

	// A server that is closed immediately: configured, and not there.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	if err := a.SetLivyAgent(url); err != nil {
		t.Fatal(err)
	}

	_, jid := runJob(t, a, ws.ID, nb.ID, "jobType=RunNotebook", "")
	if s := awaitJob(t, a, ws.ID, nb.ID, jid); s != "Failed" {
		t.Fatalf("job status = %s", s)
	}
	run := notebookRunDetail(t, a, ws.ID, nb.ID, jid)
	if !strings.Contains(run.Cells[0].Error, "unreachable") {
		t.Fatalf("the error should name the agent: %+v", run.Cells[0])
	}
}

// jsonQuote renders a Go string as a JSON string literal.
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestNotebookExitValueSurvivesQuotesAndBraces.
//
// The bug this pins: the probe used to evaluate `repr(__nb_exit__)`, and the
// agent repr()s a statement's result for display — so the value came back
// repr'd TWICE, and an exit value containing quotes arrived as \'{"a": 1}\'.
// Stripping outer quote characters cannot recover that.
//
// It survived every earlier test because the e2e notebook exits with
// str(count). "4" has no quotes, so double-repr still trimmed clean. The first
// real notebook to return JSON — a medallion step reporting its row counts —
// found it in one run.
func TestNotebookExitValueSurvivesQuotesAndBraces(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	nb := createNotebook(t, st, ws.ID, sampleNotebook)
	agent := newFakeAgent(t, a)

	const payload = `{"silver_customers": 100000, "countries": ["GB", "SG"]}`
	agent.reply = func(code string) map[string]any {
		if strings.Contains(code, "spark.range") {
			agent.exit = payload
			return map[string]any{"status": "error", "ename": "Error", "evalue": "_NotebookExit"}
		}
		return nil
	}

	_, jid := runJob(t, a, ws.ID, nb.ID, "jobType=RunNotebook", "")
	if s := awaitJob(t, a, ws.ID, nb.ID, jid); s != "Completed" {
		t.Fatalf("job status = %s", s)
	}
	run := notebookRunDetail(t, a, ws.ID, nb.ID, jid)
	if run.ExitValue != payload {
		t.Fatalf("exit value mangled:\n got %q\nwant %q", run.ExitValue, payload)
	}
	// And it must still be parseable as what it is.
	var got map[string]any
	if err := json.Unmarshal([]byte(run.ExitValue), &got); err != nil {
		t.Fatalf("exit value is not the JSON the notebook returned: %v", err)
	}
	if got["silver_customers"] != float64(100000) {
		t.Fatalf("decoded = %v", got)
	}
}

// TestNotebookPreludeBindsBothFabricSpellings runs the prelude through a real
// Python and checks that a notebook written against real Fabric can stop.
//
// Asserting on the TEXT of the prelude would be worthless here: the bug it
// pins was `try: import notebookutils` silently doing nothing when the package
// is absent, and a substring check would have passed on that code too. Only
// executing it distinguishes "the name is mentioned" from "the name resolves".
//
// Both spellings, because both appear in real Fabric notebooks — mssparkutils
// is the older one, and it is what Microsoft's `fab` was running when this
// surfaced as `NameError: name 'mssparkutils' is not defined`.
func TestNotebookPreludeBindsBothFabricSpellings(t *testing.T) {
	py, err := osexec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not on PATH; the prelude can only be checked by running it")
	}
	for _, spelling := range []string{"notebookutils", "mssparkutils"} {
		t.Run(spelling, func(t *testing.T) {
			want := "value-from-" + spelling
			probe := notebookPrelude + fmt.Sprintf(`
exited = False
try:
    %s.notebook.exit(%q)
except _NotebookExit:
    exited = True
assert exited, "exit() returned instead of raising; later cells would still run"
print(__nb_exit__)
`, spelling, want)
			out, err := osexec.Command(py, "-c", probe).CombinedOutput()
			if err != nil {
				t.Fatalf("%s: %v\n%s", spelling, err, out)
			}
			if got := strings.TrimSpace(string(out)); got != want {
				t.Fatalf("%s.notebook.exit recorded %q; want %q", spelling, got, want)
			}
		})
	}
}

// jobBodyJSON returns the whole job body, not just its status: the reconciled
// failureReason is the thing under test, and a status alone cannot show it.
func jobBodyJSON(t *testing.T, a *API, wid, iid, jid string) map[string]any {
	t.Helper()
	w := do(a.getJobInstance, admin, "GET", "", map[string]string{"wid": wid, "iid": iid, "jid": jid})
	if w.Code != 200 {
		t.Fatalf("getJobInstance = %d %s", w.Code, w.Body.Bytes())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

// TestFailingCellFailsTheJobAndNamesTheCell covers the pair of lies this
// package exists to prevent, from the caller's side rather than the driver's.
//
// A notebook whose cell raises must reach the caller as Failed, and the failure
// must say WHICH cell and WHY. Before, the job carried only "The job failed."
// and the per-cell error sat in a run detail nobody knew to ask for, so
// diagnosing meant re-running the notebook by hand outside the emulator.
func TestFailingCellFailsTheJobAndNamesTheCell(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	nb := createNotebook(t, st, ws.ID, sampleNotebook)
	agent := newFakeAgent(t, a)

	agent.reply = func(code string) map[string]any {
		if strings.Contains(code, "spark.range") {
			return map[string]any{
				"status": "error", "ename": "RuntimeError", "evalue": "boom",
				"traceback": []string{"Traceback", "RuntimeError: boom"},
			}
		}
		return nil
	}

	_, jid := runJob(t, a, ws.ID, nb.ID, "jobType=RunNotebook", "")
	if s := awaitJob(t, a, ws.ID, nb.ID, jid); s != "Failed" {
		t.Fatalf("a cell that raises must fail the job; status = %s", s)
	}

	// The reason must be actionable on its own: which cell, and the error.
	body := jobBodyJSON(t, a, ws.ID, nb.ID, jid)
	reason, _ := body["failureReason"].(map[string]any)
	if reason == nil {
		t.Fatalf("no failureReason on a failed job: %+v", body)
	}
	msg, _ := reason["message"].(string)
	if !strings.Contains(msg, "Cell 0") || !strings.Contains(msg, "boom") {
		t.Fatalf("failureReason does not name the cell and error: %q", msg)
	}
	if d, _ := reason["detail"].(string); !strings.Contains(d, "notebookRun") {
		t.Fatalf("failureReason should point at the run detail; got %q", d)
	}

	// And the detail endpoint still carries the per-cell truth.
	run := notebookRunDetail(t, a, ws.ID, nb.ID, jid)
	if run.Status != "Failed" || run.Cells[0].Status != "Failed" {
		t.Fatalf("run detail disagrees with the job: %+v", run)
	}
}

// TestJobDoesNotReportCompletedWhileCellsArePending is the invariant that would
// have caught a whole class of silent green.
//
// A job's status and the run's cells are two views of one execution. If the
// cells never ran, Completed is not a status, it is a lie — and it is the one
// that gets believed, because nobody investigates a green job. Here the run is
// saved with its cells still Pending (no engine ever reported) and the job is
// marked complete underneath it; the caller must not be told Completed.
func TestJobDoesNotReportCompletedWhileCellsArePending(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	nb := createNotebook(t, st, ws.ID, sampleNotebook)
	// No agent: nothing will drive the run, so its cells stay Pending.

	_, jid := runJob(t, a, ws.ID, nb.ID, "jobType=RunNotebook", "")

	// Force the underlying job terminal, as a clock or a stray finalisation
	// would, while the run detail still says nothing executed.
	if err := st.FinalizeJob(nb.ID, jid, ""); err != nil {
		t.Fatal(err)
	}

	body := jobBodyJSON(t, a, ws.ID, nb.ID, jid)
	if got, _ := body["status"].(string); got == "Completed" {
		t.Fatalf("job reported Completed while its cells were Pending: %+v", body)
	}
	if _, ok := body["endTimeUtc"]; ok {
		t.Fatalf("a job that has not really finished must not carry endTimeUtc: %+v", body)
	}
}
