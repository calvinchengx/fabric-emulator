package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// waitForJobStatus polls until a job reaches a terminal state. The drive runs
// in a goroutine, so a bare read after POST races it.
func waitForJobStatus(t *testing.T, st *store.Store, iid, jid string) *store.JobInstance {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		j, err := st.GetJobInstance(iid, jid)
		if err == nil {
			if s := j.StatusAt(st.Now()); s == store.JobCompleted || s == store.JobFailed {
				return j
			}
		}
		select {
		case <-deadline:
			if j, err2 := st.GetJobInstance(iid, jid); err2 == nil {
				t.Fatalf("job never reached a terminal state; last status %q", j.StatusAt(st.Now()))
			}
			t.Fatalf("job never readable: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func seedSparkJob(t *testing.T, st *store.Store, wid, config, source string) *store.Item {
	t.Helper()
	item := &store.Item{WorkspaceID: wid, Type: "SparkJobDefinition", DisplayName: "job"}
	if err := st.CreateItem(item, []store.DefinitionPart{
		sparkPart("SparkJobDefinitionV1.json", config),
		sparkPart("main.py", source),
	}); err != nil {
		t.Fatal(err)
	}
	return item
}

// THE BUG THIS PINS. Before sparkjobdrive.go the emulator resolved a Spark Job
// Definition, served it at GET …/sparkJobRun, and waited: nothing ran it, and
// jobs.go parks every SJD that parses at CompleteAt=MaxInt64 so the clock never
// finishes one either. The only thing that ever executed a Spark job was a
// client that fetched the source and exec'd it itself — which is what
// e2e/notebook-run did, making the suite the engine rather than the witness.
// Real Fabric runs a submitted job on its own pool.
func TestSparkJobIsExecutedByTheEmulatorWhenAnAgentIsConfigured(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	stub := newAgentStub(t, a)
	stub.answer(map[string]any{"status": "ok", "data": map[string]any{"text/plain": "sjd-result=5\n"}}, 0)

	item := seedSparkJob(t, st, ws.ID,
		`{"executableFile":"main.py","arguments":["--increment","2"]}`,
		"print('sjd-result=5')")
	_, jid := runJob(t, a, ws.ID, item.ID, "jobType=sparkjob", "")

	j := waitForJobStatus(t, st, item.ID, jid)
	if j.StatusAt(st.Now()) != store.JobCompleted {
		t.Fatalf("emulator-driven Spark job did not complete: %+v", j)
	}

	// The source must have reached the agent, and argv must carry the main file
	// then the definition's arguments — the contract a spark-submit'd script
	// reads. Without the prelude a job using sys.argv sees the emulator's own.
	var sawSource, sawArgv bool
	for _, p := range stub.recorded() {
		code, _ := p.body["code"].(string)
		if code == "print('sjd-result=5')" {
			sawSource = true
		}
		if code == "import sys\nsys.argv = [\"main.py\",\"--increment\",\"2\"]\n" {
			sawArgv = true
		}
	}
	if !sawSource || !sawArgv {
		t.Fatalf("source sent=%v argv sent=%v; posts=%+v", sawSource, sawArgv, stub.recorded())
	}

	_, raw, err := st.GetNotebookRun(jid)
	if err != nil {
		t.Fatal(err)
	}
	var run sparkJobRun
	if json.Unmarshal([]byte(raw), &run) != nil || run.Status != "Completed" || run.Output == "" {
		t.Fatalf("stored run did not carry the engine's output: %+v", run)
	}
}

// A driven job must announce its terminal state on the flow bus, not merely
// converge in the store. Subscribing BEFORE the POST and draining after is the
// only sound order: the drive is asynchronous, so a Replay() taken straight
// after the call races it and can read an empty stream that is not empty.
func TestADrivenSparkJobPublishesItsTerminalEvent(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	stub := newAgentStub(t, a)
	stub.answer(map[string]any{"status": "ok", "data": map[string]any{"text/plain": "ok\n"}}, 0)

	sub := st.Subscribe()
	defer sub.Close()

	item := seedSparkJob(t, st, ws.ID, `{"executableFile":"main.py"}`, "print('ok')")
	_, jid := runJob(t, a, ws.ID, item.ID, "jobType=sparkjob", "")
	waitForJobStatus(t, st, item.ID, jid)

	var terminal string
	for _, ev := range drainEvents(t, sub.C) {
		if ev.JobID == jid && (ev.Status == store.JobCompleted || ev.Status == store.JobFailed) {
			terminal = ev.Status
		}
	}
	if terminal != store.JobCompleted {
		t.Fatalf("no terminal event for a driven Spark job; got %q", terminal)
	}
}

// A failing job must fail the JOB, not just carry an error in its run record.
func TestASparkJobThatRaisesFailsTheJob(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	stub := newAgentStub(t, a)
	stub.answer(map[string]any{
		"status": "error", "ename": "RuntimeError", "evalue": "boom",
	}, 0)

	item := seedSparkJob(t, st, ws.ID, `{"executableFile":"main.py"}`, "raise RuntimeError('boom')")
	_, jid := runJob(t, a, ws.ID, item.ID, "jobType=sparkjob", "")

	j := waitForJobStatus(t, st, item.ID, jid)
	if j.StatusAt(st.Now()) != store.JobFailed {
		t.Fatalf("a raising Spark job should fail its job: %+v", j)
	}
	_, raw, _ := st.GetNotebookRun(jid)
	var run sparkJobRun
	_ = json.Unmarshal([]byte(raw), &run)
	if run.Error == "" {
		t.Fatalf("failure carried no error message: %+v", run)
	}
}

// An unreachable agent must finalise the job, not leave it parked forever.
// CompleteAt=MaxInt64 means nothing else can ever finish it.
func TestAnUnreachableAgentFailsTheSparkJobRatherThanParkingIt(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	stub := newAgentStub(t, a)
	stub.answer(map[string]any{"error": "down"}, 500)

	item := seedSparkJob(t, st, ws.ID, `{"executableFile":"main.py"}`, "print('never runs')")
	_, jid := runJob(t, a, ws.ID, item.ID, "jobType=sparkjob", "")

	if j := waitForJobStatus(t, st, item.ID, jid); j.StatusAt(st.Now()) != store.JobFailed {
		t.Fatalf("unreachable agent should fail the job: %+v", j)
	}
}

// With NO agent the original contract stands: the emulator serves the job and
// waits for an external engine's callback. Regressing this would break every
// consumer that runs Spark jobs on its own compute.
func TestWithoutAnAgentASparkJobWaitsForItsCallback(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)

	item := seedSparkJob(t, st, ws.ID, `{"executableFile":"main.py"}`, "print('external')")
	_, jid := runJob(t, a, ws.ID, item.ID, "jobType=sparkjob", "")

	time.Sleep(100 * time.Millisecond)
	j, err := st.GetJobInstance(item.ID, jid)
	if err != nil {
		t.Fatal(err)
	}
	if s := j.StatusAt(st.Now()); s == store.JobCompleted || s == store.JobFailed {
		t.Fatalf("with no agent the job must stay open for a callback; got %q", j.StatusAt(st.Now()))
	}

	pv := map[string]string{"wid": ws.ID, "iid": item.ID, "jid": jid}
	if w := do(a.reportSparkJobRun, admin, "POST", `{"status":"Completed","output":"done"}`, pv); w.Code != 200 {
		t.Fatalf("callback report = %d %s", w.Code, w.Body.Bytes())
	}
	if j := waitForJobStatus(t, st, item.ID, jid); j.StatusAt(st.Now()) != store.JobCompleted {
		t.Fatalf("callback did not complete the job: %+v", j)
	}
}
