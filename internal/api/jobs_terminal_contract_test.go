package api

import (
	"testing"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// The flow stream's terminal-event contract, checked STRUCTURALLY rather than
// one job type at a time.
//
// `startJob` used to dispatch on (item type, job type) in one place and decide
// whether an outcome was already known in another (`terminalStatusOf`). The two
// had to agree about every job type, and when they did not the failure was
// SILENT: the job's status converged perfectly well (the clock, or a
// FinalizeJob call) while nothing ever announced it on the flow stream. Every
// test that polls a job status passed; the Flow view showed a job running
// forever. That cost this repo real time once, and the fix then was a bus
// assertion for the ONE type that had drifted — which left the next one to be
// found the same way.
//
// WRITING THIS TEST FOUND THE NEXT ONE. An **Apache Airflow job that could not
// start** — no `dagId`, or no Airflow attached — was finalised as Failed and
// announced to nobody, because the "failed to even start" branch listed
// Notebook and SparkJobDefinition and not it. So the second list is gone:
// `startJob` now records what it settled, where it settles it, and the deferred
// announcement reads that. A variable set at the decision cannot drift from it.
//
// THE CONTRACT, stated so it can be argued with: a job that reaches a terminal
// status must have a terminal event published for it, EXCEPT where its
// completion is derived from the virtual clock rather than from anything the
// emulator did. A clock-derived completion has no moment at which the emulator
// learns the outcome, so the stream says nothing further about it — that is the
// documented design, and the cases below encode it as an expectation rather
// than leaving it as an untested hole.
//
// Subscribe BEFORE acting and then read, never `Replay()` afterwards: the
// dispatcher is asynchronous and a replay races it.

// jobTerminalCase is one dispatched (item type, job type) pair.
type jobTerminalCase struct {
	name      string
	jobType   string
	body      string // the job's executionData payload; "{}" when it needs none
	seed      func(t *testing.T, a *API, st *store.Store, wsID string) *store.Item
	wantEvent string // the terminal status expected on the stream
	// clockDerived marks the deliberate exception: the job finishes because
	// virtual time passed, so there is no instant at which to announce it.
	clockDerived bool
	// awaitsEngine marks the third state — the job has NOT finished and must
	// not be announced as though it had. It is the opposite failure from the
	// one above and just as silent: a premature terminal event tells the Flow
	// view a run is over while an engine is still working on it.
	awaitsEngine bool
	why          string
}

func awaitTerminalEvent(t *testing.T, ch <-chan store.Event, jid string) string {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Kind == store.KindJob && ev.JobID == jid &&
				(ev.Status == store.JobCompleted || ev.Status == store.JobFailed) {
				return ev.Status
			}
		case <-deadline:
			return ""
		}
	}
}

// TestEveryDispatchedJobTypeAnnouncesItsOutcome walks the dispatch table.
// Adding a job type to `startJob` without adding it here fails
// TestJobDispatchTableIsComplete below, so the pair cannot drift apart quietly.
func TestEveryDispatchedJobTypeAnnouncesItsOutcome(t *testing.T) {
	for _, tc := range jobTerminalCases() {
		t.Run(tc.name, func(t *testing.T) {
			a, st := newAPI(t)
			ws := seedWorkspace(t, st)
			it := tc.seed(t, a, st, ws.ID)

			// Before acting, not after.
			sub := st.Subscribe()
			defer sub.Close()

			body := tc.body
			if body == "" {
				body = "{}"
			}
			_, jid := runJob(t, a, ws.ID, it.ID, "jobType="+tc.jobType, body)

			if tc.awaitsEngine {
				if got := awaitTerminalEvent(t, sub.C, jid); got != "" {
					t.Fatalf("announced %q for a job no engine has finished — %s", got, tc.why)
				}
				if s := jobStatus(t, a, ws.ID, it.ID, jid); s != "InProgress" {
					t.Fatalf("job = %s, want InProgress: %s", s, tc.why)
				}
				return
			}

			status := awaitJob(t, a, ws.ID, it.ID, jid)
			got := awaitTerminalEvent(t, sub.C, jid)
			if tc.clockDerived {
				if got != "" {
					t.Fatalf("a clock-derived job announced %q — if this is now a real "+
						"moment the emulator knows about, move it out of the exception "+
						"list rather than widening it: %s", got, tc.why)
				}
				return
			}
			if got == "" {
				t.Fatalf("job reached %s but NOTHING was announced on the flow stream — "+
					"the status converged while the terminal-event contract broke, which is "+
					"the exact failure this test exists to catch", status)
			}
			if got != tc.wantEvent {
				t.Fatalf("announced %q, want %q (job status was %s)", got, tc.wantEvent, status)
			}
			// The stream must agree with the job, or a client that trusts one
			// and a client that trusts the other disagree about the same run.
			if got != status {
				t.Fatalf("the stream says %q and the job says %q for the same run", got, status)
			}
		})
	}
}

func jobTerminalCases() []jobTerminalCase {
	return []jobTerminalCase{
		{
			name:      "CopyJob/Execute",
			jobType:   "Execute",
			wantEvent: store.JobCompleted,
			seed: func(t *testing.T, a *API, st *store.Store, ws string) *store.Item {
				src := seedLakehouse(t, st, ws, "src")
				dst := seedLakehouse(t, st, ws, "dst")
				seedFile(t, st, ws, src.ID, "Tables/orders/part-0.parquet", []byte("x"))
				return createCopyJob(t, st, ws, lakehouseBatchDef(ws, src.ID, dst.ID, ""))
			},
		},
		{
			name:      "CopyJob/Execute refused",
			jobType:   "Execute",
			wantEvent: store.JobFailed,
			seed: func(t *testing.T, a *API, st *store.Store, ws string) *store.Item {
				return createCopyJob(t, st, ws, `{"properties":{"jobMode":"CDC"},"activities":[]}`)
			},
		},
		{
			name:      "Dataflow/Refresh",
			jobType:   "Refresh",
			wantEvent: store.JobFailed,
			seed: func(t *testing.T, a *API, st *store.Store, ws string) *store.Item {
				it := &store.Item{WorkspaceID: ws, Type: "Dataflow", DisplayName: "mashup"}
				if err := st.CreateItem(it, nil); err != nil {
					t.Fatal(err)
				}
				return it
			},
		},
		{
			name:      "DataPipeline/Pipeline",
			jobType:   "Pipeline",
			wantEvent: store.JobCompleted,
			seed: func(t *testing.T, a *API, st *store.Store, ws string) *store.Item {
				return createPipeline(t, st, ws, `{"properties":{"activities":[
                  {"name":"V","type":"SetVariable","typeProperties":{
                    "variableName":"v","value":"x"}}]}}`)
			},
		},
		{
			name:      "DataPipeline/Pipeline failing",
			jobType:   "Pipeline",
			wantEvent: store.JobFailed,
			seed: func(t *testing.T, a *API, st *store.Store, ws string) *store.Item {
				return createPipeline(t, st, ws, `{"properties":{"activities":[
                  {"name":"Nope","type":"AzureMLExecutePipeline","typeProperties":{
                    "mlPipelineId":"p1"}}]}}`)
			},
		},
		{
			name:      "SparkJobDefinition/sparkjob refused",
			jobType:   "sparkjob",
			wantEvent: store.JobFailed,
			seed: func(t *testing.T, a *API, st *store.Store, ws string) *store.Item {
				it := &store.Item{WorkspaceID: ws, Type: "SparkJobDefinition", DisplayName: "bad"}
				if err := st.CreateItem(it, nil); err != nil {
					t.Fatal(err)
				}
				return it
			},
		},
		{
			name:      "ApacheAirflowJob/Run with no dagId",
			jobType:   "Run",
			wantEvent: store.JobFailed,
			seed: func(t *testing.T, a *API, st *store.Store, ws string) *store.Item {
				it := &store.Item{WorkspaceID: ws, Type: "ApacheAirflowJob", DisplayName: "air"}
				if err := st.CreateItem(it, nil); err != nil {
					t.Fatal(err)
				}
				return it
			},
		},
		{
			name:         "a generic item's job",
			jobType:      "DefaultJob",
			clockDerived: true,
			why: "a Lakehouse job has no execution behind it; its status is virtual time " +
				"passing, and there is no instant at which the emulator learns an outcome",
			seed: func(t *testing.T, a *API, st *store.Store, ws string) *store.Item {
				return seedLakehouse(t, st, ws, "plain")
			},
		},
		{
			name:         "Notebook/RunNotebook with no engine attached",
			jobType:      "RunNotebook",
			awaitsEngine: true,
			why: "cells are outstanding and no Spark agent is configured, so only an " +
				"engine callback can finish this — the job stays open, which is the honest " +
				"answer when there is nothing to run it",
			seed: func(t *testing.T, a *API, st *store.Store, ws string) *store.Item {
				return createNotebook(t, st, ws, sampleNotebook)
			},
		},
		{
			name:         "SparkJobDefinition/sparkjob with no engine attached",
			jobType:      "sparkjob",
			awaitsEngine: true,
			why: "a parsed definition means there is a main file for an engine to run, " +
				"so the clock must not finish it either",
			seed: func(t *testing.T, a *API, st *store.Store, ws string) *store.Item {
				lake := seedLakehouse(t, st, ws, "lake")
				it := &store.Item{WorkspaceID: ws, Type: "SparkJobDefinition", DisplayName: "job"}
				config := `{"executableFile":"main.py","defaultLakehouseArtifactId":"` +
					lake.ID + `","defaultLakehouseWorkspaceId":"` + ws + `"}`
				if err := st.CreateItem(it, []store.DefinitionPart{
					sparkPart("SparkJobDefinitionV1.json", config),
					sparkPart("main.py", "print('done')"),
				}); err != nil {
					t.Fatal(err)
				}
				return it
			},
		},
		{
			// The SECOND spelling. Microsoft's own readback example says
			// "CopyJob" where the docs say "Execute", so both dispatch — and a
			// spelling that dispatches but is not covered here is a pair that
			// could stop announcing with nothing failing.
			name:      "CopyJob/CopyJob (the readback spelling)",
			jobType:   "CopyJob",
			wantEvent: store.JobCompleted,
			seed: func(t *testing.T, a *API, st *store.Store, ws string) *store.Item {
				src := seedLakehouse(t, st, ws, "src")
				dst := seedLakehouse(t, st, ws, "dst")
				seedFile(t, st, ws, src.ID, "Tables/orders/part-0.parquet", []byte("x"))
				return createCopyJob(t, st, ws, lakehouseBatchDef(ws, src.ID, dst.ID, ""))
			},
		},
		{
			name:      "Dataflow/Publish",
			jobType:   "Publish",
			wantEvent: store.JobFailed,
			seed: func(t *testing.T, a *API, st *store.Store, ws string) *store.Item {
				it := &store.Item{WorkspaceID: ws, Type: "Dataflow", DisplayName: "mashup"}
				if err := st.CreateItem(it, nil); err != nil {
					t.Fatal(err)
				}
				return it
			},
		},
		{
			// The engine-attached path, which is where publishJobOutcome
			// actually fires. Without it the table proved only that jobs which
			// never reach an engine are announced — the easy half.
			name:      "Notebook/RunNotebook driven by the agent",
			jobType:   "RunNotebook",
			wantEvent: store.JobCompleted,
			seed: func(t *testing.T, a *API, st *store.Store, ws string) *store.Item {
				newFakeAgent(t, a)
				return createNotebook(t, st, ws, sampleNotebook)
			},
		},
		{
			name:      "Notebook/RunNotebook that cannot start",
			jobType:   "RunNotebook",
			wantEvent: store.JobFailed,
			seed: func(t *testing.T, a *API, st *store.Store, ws string) *store.Item {
				// No definition at all: parseNotebookRun refuses rather than
				// reporting a fast success.
				it := &store.Item{WorkspaceID: ws, Type: "Notebook", DisplayName: "empty"}
				if err := st.CreateItem(it, nil); err != nil {
					t.Fatal(err)
				}
				return it
			},
		},
		{
			name:      "SparkJobDefinition/sparkjob driven by the agent",
			jobType:   "sparkjob",
			wantEvent: store.JobCompleted,
			seed: func(t *testing.T, a *API, st *store.Store, ws string) *store.Item {
				newFakeAgent(t, a)
				lake := seedLakehouse(t, st, ws, "lake")
				it := &store.Item{WorkspaceID: ws, Type: "SparkJobDefinition", DisplayName: "job"}
				config := `{"executableFile":"main.py","defaultLakehouseArtifactId":"` +
					lake.ID + `","defaultLakehouseWorkspaceId":"` + ws + `"}`
				if err := st.CreateItem(it, []store.DefinitionPart{
					sparkPart("SparkJobDefinitionV1.json", config),
					sparkPart("main.py", "print('done')"),
				}); err != nil {
					t.Fatal(err)
				}
				return it
			},
		},
		{
			name:      "ApacheAirflowJob/Run against a runtime",
			jobType:   "Run",
			body:      `{"executionData":{"dagId":"hello","conf":{"answer":42}}}`,
			wantEvent: store.JobCompleted,
			seed: func(t *testing.T, a *API, st *store.Store, ws string) *store.Item {
				a.Airflow = &airflowWitness{}
				it := &store.Item{WorkspaceID: ws, Type: "ApacheAirflowJob", DisplayName: "air"}
				if err := st.CreateItem(it, nil); err != nil {
					t.Fatal(err)
				}
				// A DAG to sync: without one the run fails for its own reason
				// and this case would silently become a second failure test.
				if err := st.CreateOneLakePath(&store.OneLakePath{
					WorkspaceID: ws, ItemID: it.ID,
					RelPath: "Files/dags/hello.py", Content: []byte("# dag"),
				}, false); err != nil {
					t.Fatal(err)
				}
				return it
			},
		},
		{
			// The other start-failure branch: a dagId given, but nothing to run
			// it. Both branches set the outcome, so both must announce it.
			name:      "ApacheAirflowJob/Run with no runtime attached",
			jobType:   "Run",
			body:      `{"executionData":{"dagId":"hello"}}`,
			wantEvent: store.JobFailed,
			seed: func(t *testing.T, a *API, st *store.Store, ws string) *store.Item {
				it := &store.Item{WorkspaceID: ws, Type: "ApacheAirflowJob", DisplayName: "air"}
				if err := st.CreateItem(it, nil); err != nil {
					t.Fatal(err)
				}
				return it
			},
		},
	}
}

// TestJobDispatchTableIsComplete guards the PAIR space, not the type space.
// An earlier draft checked item types only, which would have passed while
// CopyJob's second spelling ("CopyJob" as well as "Execute") or Dataflow's
// `Publish` went uncovered — and an uncovered pair is one that can stop
// announcing with nothing failing. Every (item type, job type) `startJob`
// branches on must appear above, so adding a branch without adding a case is a
// failing test rather than a silent hole.
func TestJobDispatchTableIsComplete(t *testing.T) {
	// Maintained by hand on purpose: the point is that extending startJob
	// forces a deliberate edit here.
	dispatched := [][2]string{
		{"ApacheAirflowJob", "Run"},
		{"Notebook", "RunNotebook"},
		{"SparkJobDefinition", "sparkjob"},
		{"DataPipeline", "Pipeline"},
		{"CopyJob", "Execute"},
		{"CopyJob", "CopyJob"},
		{"Dataflow", "Refresh"},
		{"Dataflow", "Publish"},
	}
	type coverage struct{ settledHere, finishedByEngine bool }
	seen := map[[2]string]bool{}
	byType := map[string]*coverage{}
	for _, tc := range jobTerminalCases() {
		a, st := newAPI(t)
		ws := seedWorkspace(t, st)
		typ := tc.seed(t, a, st, ws.ID).Type
		seen[[2]string{typ, tc.jobType}] = true
		if byType[typ] == nil {
			byType[typ] = &coverage{}
		}
		switch tc.wantEvent {
		case store.JobFailed:
			byType[typ].settledHere = true
		case store.JobCompleted:
			byType[typ].finishedByEngine = true
		}
	}
	for _, pair := range dispatched {
		if !seen[pair] {
			t.Errorf("startJob dispatches on (%s, %s) but no case above drives it — a job "+
				"whose outcome is never announced breaks the flow stream while every "+
				"status-polling test keeps passing", pair[0], pair[1])
		}
	}

	// Both halves of each engine-backed type: the path that ends inside
	// startJob AND the path an engine finishes later. Covering only the first
	// would prove announcement for the jobs that never reach an engine, which
	// is the easy half and not where publishJobOutcome lives.
	for _, typ := range []string{"Notebook", "SparkJobDefinition", "ApacheAirflowJob"} {
		c := byType[typ]
		if c == nil || !c.settledHere || !c.finishedByEngine {
			t.Errorf("%s is covered for only one of its two announcement paths "+
				"(settled inside startJob: %v, finished by an engine: %v)",
				typ, c != nil && c.settledHere, c != nil && c.finishedByEngine)
		}
	}
}
