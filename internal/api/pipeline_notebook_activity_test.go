package api

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/pipeline"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// seedWorkspaceNamed is seedWorkspace with a caller-chosen display name, which
// a cross-workspace test needs: seedWorkspace always uses "w", and the second
// call collides on the unique display name.
func seedWorkspaceNamed(t *testing.T, st *store.Store, name string) *store.Workspace {
	t.Helper()
	ws := &store.Workspace{DisplayName: name}
	if err := st.CreateWorkspace(ws, store.Principal{ID: admin.ID, Type: admin.Type}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateRoleAssignment(&store.RoleAssignment{
		WorkspaceID: ws.ID, Principal: store.Principal{ID: viewer.ID, Type: "User"}, Role: store.RoleViewer,
	}); err != nil {
		t.Fatal(err)
	}
	return ws
}

// seedNotebookIn creates a Notebook item in a given workspace.
func seedNotebookIn(t *testing.T, st *store.Store, wid, name, src string) *store.Item {
	t.Helper()
	nb := &store.Item{WorkspaceID: wid, Type: "Notebook", DisplayName: name}
	if err := st.CreateItem(nb, notebookParts(src)); err != nil {
		t.Fatal(err)
	}
	return nb
}

// notebookActivityPipeline wraps one TridentNotebook activity whose
// typeProperties are supplied verbatim.
func notebookActivityPipeline(tp string) string {
	return `{"properties":{"activities":[
      {"name":"RunNb","type":"TridentNotebook","typeProperties":{` + tp + `}}]}}`
}

// TestNotebookActivityCrossWorkspace: workspaceId names the workspace holding
// the notebook, and Fabric marks it required alongside notebookId because the
// notebook need not live beside the pipeline. Ignoring it made every legitimate
// cross-workspace activity fail as though the notebookId were wrong.
func TestNotebookActivityCrossWorkspace(t *testing.T) {
	a, st := newAPI(t)
	pipeWS := seedWorkspaceNamed(t, st, "pipelines")
	nbWS := seedWorkspaceNamed(t, st, "notebooks")
	nb := seedNotebookIn(t, st, nbWS.ID, "etl", "")

	pl := createPipeline(t, st, pipeWS.ID, notebookActivityPipeline(
		`"notebookId":"`+nb.ID+`","workspaceId":"`+nbWS.ID+`"`))
	_, jid := runJob(t, a, pipeWS.ID, pl.ID, "jobType=Pipeline", "{}")

	if s := awaitJob(t, a, pipeWS.ID, pl.ID, jid); s != "Completed" {
		_, runs := activityRuns(t, a, pipeWS.ID, pl.ID, jid)
		t.Fatalf("cross-workspace notebook activity = %s; runs=%+v", s, runs)
	}
	_, runs := activityRuns(t, a, pipeWS.ID, pl.ID, jid)
	out, _ := runs[0]["output"].(map[string]any)
	if out["notebookId"] != nb.ID {
		t.Errorf("output notebookId = %v, want the notebook in the OTHER workspace %s", out["notebookId"], nb.ID)
	}
}

// TestNotebookActivityWorkspaceIdZeroGUID: a same-workspace activity carries the
// zero GUID in Git. Looking that up literally finds no workspace, so it must
// resolve to the pipeline's own.
func TestNotebookActivityWorkspaceIdZeroGUID(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	nb := seedNotebookIn(t, st, ws.ID, "etl", "")

	pl := createPipeline(t, st, ws.ID, notebookActivityPipeline(
		`"notebookId":"`+nb.ID+`","workspaceId":"00000000-0000-0000-0000-000000000000"`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")

	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
		t.Fatalf("zero-GUID workspaceId = %s, want Completed; runs=%+v", s, runs)
	}
}

// TestNotebookActivityWorkspaceIdOmitted: absent, it defaults to the pipeline's
// workspace, which is the single-workspace shape most pipelines have.
func TestNotebookActivityWorkspaceIdOmitted(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	nb := seedNotebookIn(t, st, ws.ID, "etl", "")

	pl := createPipeline(t, st, ws.ID, notebookActivityPipeline(`"notebookId":"`+nb.ID+`"`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")

	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		t.Fatalf("omitted workspaceId = %s, want Completed", s)
	}
}

// TestNotebookActivityWorkspaceIdWrong: when the named workspace does not hold
// the notebook, the error must name that workspace. Blaming "this workspace"
// points the reader at the pipeline's own, which is not where it looked.
func TestNotebookActivityWorkspaceIdWrong(t *testing.T) {
	a, st := newAPI(t)
	pipeWS := seedWorkspaceNamed(t, st, "pipelines")
	nbWS := seedWorkspaceNamed(t, st, "notebooks")
	elsewhere := seedWorkspaceNamed(t, st, "elsewhere")
	nb := seedNotebookIn(t, st, nbWS.ID, "etl", "")

	pl := createPipeline(t, st, pipeWS.ID, notebookActivityPipeline(
		`"notebookId":"`+nb.ID+`","workspaceId":"`+elsewhere.ID+`"`))
	_, jid := runJob(t, a, pipeWS.ID, pl.ID, "jobType=Pipeline", "{}")

	if s := awaitJob(t, a, pipeWS.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("notebook missing from the named workspace = %s, want Failed", s)
	}
	_, runs := activityRuns(t, a, pipeWS.ID, pl.ID, jid)
	e, _ := runs[0]["error"].(string)
	if !strings.Contains(e, elsewhere.ID) {
		t.Errorf("error %q does not name the workspace that was searched (%s)", e, elsewhere.ID)
	}
}

// TestNotebookActivityRejectsComplexParameters: Fabric supports "simple types
// such as int, float, bool, and string" and states "complex types such as list
// and dict aren't yet supported". Accepting one would make the emulator MORE
// permissive than Fabric, so a pipeline would pass here and fail in production.
func TestNotebookActivityRejectsComplexParameters(t *testing.T) {
	for _, tc := range []struct{ name, param string }{
		{"list", `"p":{"value":["a","b"],"type":"array"}`},
		{"dict", `"p":{"value":{"k":"v"},"type":"object"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, st := newAPI(t)
			ws := seedWorkspace(t, st)
			nb := seedNotebookIn(t, st, ws.ID, "etl", "")

			pl := createPipeline(t, st, ws.ID, notebookActivityPipeline(
				`"notebookId":"`+nb.ID+`","parameters":{`+tc.param+`}`))
			_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")

			if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
				t.Fatalf("a %s parameter = %s, want Failed (Fabric rejects it)", tc.name, s)
			}
			_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
			e, _ := runs[0]["error"].(string)
			if !strings.Contains(e, "list and dict are not supported") {
				t.Errorf("error %q does not explain the Fabric limitation", e)
			}
		})
	}
}

// TestNotebookActivityAcceptsSimpleParameters: the four types Fabric documents
// must all pass, including the falsy ones a naive emptiness check would drop.
func TestNotebookActivityAcceptsSimpleParameters(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	nb := seedNotebookIn(t, st, ws.ID, "etl", "")

	pl := createPipeline(t, st, ws.ID, notebookActivityPipeline(
		`"notebookId":"`+nb.ID+`","parameters":{
           "s":{"value":"text","type":"string"},
           "i":{"value":0,"type":"int"},
           "f":{"value":1.5,"type":"float"},
           "b":{"value":false,"type":"bool"}}`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")

	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
		t.Fatalf("simple parameters = %s, want Completed; runs=%+v", s, runs)
	}
}

// TestNotebookActivityOutputShape: Fabric publishes no schema for the activity
// output, so the Synapse ancestor's sample is the reference. A pipeline reads
// these by name, so a missing field is a broken expression at run time.
func TestNotebookActivityOutputShape(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	nb := seedNotebookIn(t, st, ws.ID, "etl", "")

	pl := createPipeline(t, st, ws.ID, notebookActivityPipeline(`"notebookId":"`+nb.ID+`"`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	awaitJob(t, a, ws.ID, pl.ID, jid) // async: the run detail exists once the job is terminal
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)

	out, ok := runs[0]["output"].(map[string]any)
	if !ok {
		t.Fatalf("no activity output: %+v", runs[0])
	}
	for _, k := range []string{"status", "notebookId", "jobInstanceId", "result"} {
		if _, present := out[k]; !present {
			t.Errorf("activity output has no %q: %+v", k, out)
		}
	}
	result, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("output.result is not an object: %+v", out["result"])
	}
	// runId/runStatus/exitCode are the documented Synapse names; exitValue is
	// the name Fabric's portal and Synapse's prose use, and real pipelines are
	// written against it. Both spellings must resolve.
	for _, k := range []string{"runId", "runStatus", "exitCode", "exitValue"} {
		if _, present := result[k]; !present {
			t.Errorf("output.result has no %q: %+v", k, result)
		}
	}
	if result["runId"] != out["jobInstanceId"] {
		t.Errorf("result.runId %v and jobInstanceId %v disagree; they name one run",
			result["runId"], out["jobInstanceId"])
	}
	// No engine ran this notebook, so claiming a Spark session would invent one.
	if _, present := result["sessionId"]; present {
		t.Errorf("output.result reports a sessionId for a run no engine executed: %+v", result)
	}
}

// --- notebookActivity, extracted from Execute --------------------------------
//
// Splitting the 134-line notebook arm out of Execute's switch turned one
// misleading number into two honest ones: Execute went 78.8% -> 100% (the
// dispatch really is fully exercised) and the notebook logic appeared at 69.5%,
// which is what the arm had always been. These close the reachable half of that
// gap. They use a bare executor and a resolver that fails on demand, because
// every path below is reached BEFORE the store is touched.

// TestNotebookActivityWorkspaceIDResolveFailurePropagates.
//
// workspaceId is an expression like any other, and an unresolvable one must
// fail the activity. Swallowing it would fall back to the pipeline's own
// workspace and report "no notebook %q in this workspace" -- blaming the id for
// a property that was supplied, correct, and simply not evaluated. That is the
// exact misdiagnosis the comment above this code was written to prevent.
func TestNotebookActivityWorkspaceIDResolveFailurePropagates(t *testing.T) {
	e := &pipelineExecutor{wid: "ws-1"}
	resolve := func(raw json.RawMessage) (any, error) {
		if strings.Contains(string(raw), "nb-1") {
			return "nb-1", nil
		}
		return nil, fmt.Errorf("unresolved")
	}
	_, err := e.notebookActivity(
		pipeline.Activity{Name: "RunNb", Type: "TridentNotebook"},
		map[string]json.RawMessage{
			"notebookId":  json.RawMessage(`"nb-1"`),
			"workspaceId": json.RawMessage(`"@pipeline().parameters.ws"`),
		}, resolve)
	if err == nil {
		t.Fatal("an unresolvable workspaceId was swallowed")
	}
	if !strings.Contains(err.Error(), "workspaceId") {
		t.Errorf("error = %q, want it to name workspaceId so the author knows "+
			"which property failed", err)
	}
}

// TestNotebookActivityRequiresANotebookID pins the guard that runs first.
// `nil` and empty are distinct failures upstream but the same failure here,
// and both must refuse rather than look up the empty id.
func TestNotebookActivityRequiresANotebookID(t *testing.T) {
	e := &pipelineExecutor{wid: "ws-1"}
	for name, resolve := range map[string]func(json.RawMessage) (any, error){
		"nil":     func(json.RawMessage) (any, error) { return nil, nil },
		"empty":   func(json.RawMessage) (any, error) { return "", nil },
		"failing": func(json.RawMessage) (any, error) { return nil, fmt.Errorf("boom") },
	} {
		t.Run(name, func(t *testing.T) {
			_, err := e.notebookActivity(
				pipeline.Activity{Name: "RunNb", Type: "TridentNotebook"},
				map[string]json.RawMessage{"notebookId": json.RawMessage(`""`)}, resolve)
			if err == nil {
				t.Fatal("a missing notebookId was accepted")
			}
			if !strings.Contains(err.Error(), "notebookId is required") {
				t.Errorf("error = %q, want it to name notebookId", err)
			}
		})
	}
}

// TestNotebookActivityReportsWhatTheEngineDid drives the branch that only
// exists when an engine is attached: `runsNotebooksItself()`, i.e. a Livy agent
// is configured, so the run is executed rather than parked at Pending.
//
// Without an agent the activity returns Pending and the whole block below is
// skipped -- which is why it stayed uncovered while every other notebook test
// passed. An agent stub is enough: the branch is about whether the emulator
// OWNS the execution, not about Spark actually running.
//
// The assertion is on the ACTIVITY's verdict, not on the run's. Either outcome
// exercises the block; what must not happen is an activity reporting success
// for a run that did not complete, which is the failure this whole file exists
// to prevent (see the twelve activity types that "succeeded having run
// nothing").
func TestNotebookActivityReportsWhatTheEngineDid(t *testing.T) {
	a, st := newAPI(t)
	newAgentStub(t, a)
	if !a.runsNotebooksItself() {
		t.Fatal("the stub did not attach: this test would silently assert the " +
			"no-engine path instead of the engine one")
	}
	ws := seedWorkspace(t, st)
	nb := seedNotebookIn(t, st, ws.ID, "child", "print('hi')\n")
	content := `{"properties":{"activities":[
        {"name":"RunNb","type":"TridentNotebook","typeProperties":{"notebookId":"` + nb.ID + `"}}
      ]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "")
	jobStatus := awaitJob(t, a, ws.ID, pl.ID, jid)

	status, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	if len(runs) != 1 {
		t.Fatalf("expected one activity run, got %+v", runs)
	}
	// The activity's verdict and the job's must agree. A Succeeded activity
	// under a Failed job is precisely the false green this code guards against.
	//
	// They agree in MEANING, not in spelling: a job reports Completed/Failed and
	// an activity Succeeded/Failed, so comparing the strings asserts they differ
	// -- which this test did on its first run, and reported as a disagreement
	// when there was none.
	jobOK := jobStatus == "Completed" || jobStatus == "Succeeded"
	actOK := runs[0]["status"] == "Succeeded"
	if jobOK != actOK {
		t.Errorf("job %s but activity %v -- an activity must not disagree with "+
			"the job it is the only member of", jobStatus, runs[0]["status"])
	}
	if status == "" {
		t.Error("activity runs reported no status at all")
	}
}
