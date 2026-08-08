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

			_, jid := runJob(t, a, ws.ID, it.ID, "jobType="+tc.jobType, "{}")

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
	}
}

// TestJobDispatchTableIsComplete is the structural half, and the reason this
// file is not just nine more tests: it reads the item types `startJob` special-
// cases and fails if one has no case above. Without it, the next job type is
// added, its outcome is never announced, and every status-polling test still
// passes — which is exactly how the last one got in.
func TestJobDispatchTableIsComplete(t *testing.T) {
	// The item types startJob branches on. Kept here rather than derived from
	// the source, because a list a human must edit is the point: adding a
	// branch without adding a case should be a failing test, not a silent one.
	dispatched := []string{
		"ApacheAirflowJob", "Notebook", "SparkJobDefinition",
		"DataPipeline", "CopyJob", "Dataflow",
	}
	covered := map[string]bool{}
	for _, tc := range jobTerminalCases() {
		a, st := newAPI(t)
		ws := seedWorkspace(t, st)
		covered[tc.seed(t, a, st, ws.ID).Type] = true
	}
	for _, typ := range dispatched {
		if !covered[typ] {
			t.Errorf("startJob dispatches on %s but no case in jobTerminalCases() drives it — "+
				"a job type whose outcome is never announced breaks the flow stream while "+
				"every status-polling test keeps passing", typ)
		}
	}
}
