package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
	// exit is what repr(__nb_exit__) answers — the fake's whole memory of the
	// notebook having called notebook_exit.
	exit string
}

func newFakeAgent(t *testing.T, a *API) *fakeAgent {
	t.Helper()
	f := &fakeAgent{exit: "None"}
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
			if req.Code == "repr(__nb_exit__)" {
				writeJSON(w, 200, map[string]any{
					"status": "ok", "data": map[string]any{"text/plain": f.exit},
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
			agent.exit = "'42'"
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
