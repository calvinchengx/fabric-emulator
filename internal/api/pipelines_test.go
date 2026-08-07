package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/calvinchengx/fabric-emulator/internal/warehouse"
	"github.com/parquet-go/parquet-go"
)

// createPipeline seeds a DataPipeline item whose definition is the given
// pipeline-content.json.
// pipelineSeq keeps each created pipeline's display name unique: item names
// are unique per (workspace, type), so tests that create several pipelines in
// one workspace must not reuse a literal name.
var pipelineSeq atomic.Int64

func createPipeline(t *testing.T, st *store.Store, wid, contentJSON string) *store.Item {
	t.Helper()
	payload := base64.StdEncoding.EncodeToString([]byte(contentJSON))
	it := &store.Item{WorkspaceID: wid, Type: "DataPipeline",
		DisplayName: fmt.Sprintf("pl-%d", pipelineSeq.Add(1))}
	parts := []store.DefinitionPart{{Path: "pipeline-content.json", Payload: payload, PayloadType: "InlineBase64"}}
	if err := st.CreateItem(it, parts); err != nil {
		t.Fatal(err)
	}
	return it
}

// runJob POSTs a job with the given query + body and returns the recorder and
// the created job id (parsed from the Location header).
func runJob(t *testing.T, a *API, wid, iid, query, body string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	r := httptest.NewRequest("POST", "/x?"+query, strings.NewReader(body))
	r.SetPathValue("wid", wid)
	r.SetPathValue("iid", iid)
	w := httptest.NewRecorder()
	a.createJobInstance(w, r, admin)
	loc := w.Header().Get("Location")
	jid := ""
	if loc != "" {
		jid = loc[strings.LastIndex(loc, "/")+1:]
	}
	return w, jid
}

func jobStatus(t *testing.T, a *API, wid, iid, jid string) string {
	t.Helper()
	w := do(a.getJobInstance, admin, "GET", "", map[string]string{"wid": wid, "iid": iid, "jid": jid})
	if w.Code != 200 {
		t.Fatalf("getJob = %d %s", w.Code, w.Body.Bytes())
	}
	var body struct{ Status string }
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return body.Status
}

func activityRuns(t *testing.T, a *API, wid, iid, jid string) (string, []map[string]any) {
	t.Helper()
	w := do(a.queryActivityRuns, admin, "POST", "", map[string]string{"wid": wid, "iid": iid, "jid": jid})
	if w.Code != 200 {
		t.Fatalf("queryactivityruns = %d %s", w.Code, w.Body.Bytes())
	}
	var body struct {
		Status string
		Value  []map[string]any
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return body.Status, body.Value
}

// TestPipelineJobSuccess drives a realistic pipeline (SetVariable → ForEach →
// IfCondition → TridentNotebook) through the real job API, asserting it runs
// to Completed, chains a real notebook job, and reports its activity runs.
func TestPipelineJobSuccess(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	nb := &store.Item{WorkspaceID: ws.ID, Type: "Notebook", DisplayName: "child"}
	if err := st.CreateItem(nb, notebookParts("")); err != nil {
		t.Fatal(err)
	}
	content := `{"properties":{
      "parameters":{"tables":{"type":"Array","defaultValue":["sales","regions"]}},
      "variables":{"processed":{"type":"Array"},"env":{"type":"String"}},
      "activities":[
        {"name":"SetEnv","type":"SetVariable","typeProperties":{
          "variableName":"env","value":"@concat('prod-',string(length(pipeline().parameters.tables)))"}},
        {"name":"Each","type":"ForEach","dependsOn":[{"activity":"SetEnv","dependencyConditions":["Succeeded"]}],
          "typeProperties":{"items":"@pipeline().parameters.tables","activities":[
            {"name":"Track","type":"AppendVariable","typeProperties":{"variableName":"processed","value":"@item()"}}
          ]}},
        {"name":"Gate","type":"IfCondition","dependsOn":[{"activity":"Each","dependencyConditions":["Succeeded"]}],
          "typeProperties":{"expression":{"value":"@greater(length(variables('processed')),1)","type":"Expression"},
            "ifTrueActivities":[{"name":"RunNb","type":"TridentNotebook","typeProperties":{"notebookId":"` + nb.ID + `"}}]}}
      ]}}`
	pl := createPipeline(t, st, ws.ID, content)

	w, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if w.Code != 202 {
		t.Fatalf("run = %d %s", w.Code, w.Body.Bytes())
	}
	if st := jobStatus(t, a, ws.ID, pl.ID, jid); st != "Completed" {
		t.Fatalf("job status = %s", st)
	}
	status, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	if status != "Succeeded" {
		t.Fatalf("pipeline status = %s", status)
	}
	byName := map[string]string{}
	nbJobs := 0
	for _, r := range runs {
		byName[r["activityName"].(string)] = r["status"].(string)
		if r["activityName"] == "RunNb" {
			nbJobs++
		}
	}
	for _, n := range []string{"SetEnv", "Each", "Gate", "RunNb"} {
		if byName[n] != "Succeeded" {
			t.Errorf("activity %s = %s", n, byName[n])
		}
	}
	// Track runs once per table (2), RunNb once.
	tracks := 0
	for _, r := range runs {
		if r["activityName"] == "Track" {
			tracks++
		}
	}
	if tracks != 2 || nbJobs != 1 {
		t.Errorf("expected 2 Track + 1 RunNb, got %d/%d", tracks, nbJobs)
	}
}

// TestPipelineJobFailure: a notebook activity referencing a missing notebook
// fails the activity and the whole job.
func TestPipelineJobFailure(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	content := `{"properties":{"activities":[
        {"name":"RunNb","type":"TridentNotebook","typeProperties":{"notebookId":"does-not-exist"}}
      ]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "")
	if st := jobStatus(t, a, ws.ID, pl.ID, jid); st != "Failed" {
		t.Fatalf("job status = %s", st)
	}
	status, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	if status != "Failed" || len(runs) != 1 || runs[0]["status"] != "Failed" {
		t.Fatalf("expected failed activity, got %s %+v", status, runs)
	}
}

// TestPipelineDataflowHonest501: a Dataflow Gen2 activity fails honestly rather
// than pretending to run the proprietary Power Query engine.
func TestPipelineDataflowNotImplemented(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	content := `{"properties":{"activities":[
        {"name":"Refresh","type":"RefreshDataflow","typeProperties":{"dataflowId":"x"}}
      ]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "")
	if st := jobStatus(t, a, ws.ID, pl.ID, jid); st != "Failed" {
		t.Fatalf("job status = %s", st)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	if len(runs) != 1 || !strings.Contains(runs[0]["error"].(string), "not implemented") {
		t.Fatalf("expected honest not-implemented error, got %+v", runs)
	}
}

// TestPipelineParameterOverride: run-time parameters override defaults.
func TestPipelineParameterOverride(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	content := `{"properties":{
      "parameters":{"greeting":{"type":"String","defaultValue":"default"}},
      "variables":{"out":{"type":"String"}},
      "activities":[
        {"name":"Set","type":"SetVariable","typeProperties":{"variableName":"out","value":"@pipeline().parameters.greeting"}}
      ]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline",
		`{"executionData":{"parameters":{"greeting":"overridden"}}}`)
	if st := jobStatus(t, a, ws.ID, pl.ID, jid); st != "Completed" {
		t.Fatalf("job status = %s", st)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	out := runs[0]["output"].(map[string]any)
	if out["value"] != "overridden" {
		t.Fatalf("param override failed: %+v", out)
	}
}

// TestPipelineNoDefinition: a DataPipeline with no stored definition fails the
// job with a definition error (not a crash).
func TestPipelineNoDefinition(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	pl := &store.Item{WorkspaceID: ws.ID, Type: "DataPipeline", DisplayName: "empty"}
	if err := st.CreateItem(pl, nil); err != nil {
		t.Fatal(err)
	}
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job status = %s, want Failed", s)
	}
	status, _ := activityRuns(t, a, ws.ID, pl.ID, jid)
	if status != "Failed" {
		t.Fatalf("pipeline run status = %s", status)
	}
}

// TestPipelineMalformedDefinition: a non-JSON definition payload fails cleanly.
func TestPipelineMalformedDefinition(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	pl := createPipeline(t, st, ws.ID, "{not valid pipeline json")
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job status = %s, want Failed", s)
	}
}

// createNamedPipeline seeds a DataPipeline with an explicit display name.
func createNamedPipeline(t *testing.T, st *store.Store, wid, name, contentJSON string) *store.Item {
	t.Helper()
	payload := base64.StdEncoding.EncodeToString([]byte(contentJSON))
	it := &store.Item{WorkspaceID: wid, Type: "DataPipeline", DisplayName: name}
	parts := []store.DefinitionPart{{Path: "pipeline-content.json", Payload: payload, PayloadType: "InlineBase64"}}
	if err := st.CreateItem(it, parts); err != nil {
		t.Fatal(err)
	}
	return it
}

// TestPipelineInvokeRealWork: a parent's Invoke pipeline activity runs the child
// pipeline for real (recursive interpretation) — the child's Copy actually moves
// bytes, and the parent reports the child's terminal status.
func TestPipelineInvokeRealWork(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	src := seedLakehouse(t, st, ws.ID, "src")
	dst := seedLakehouse(t, st, ws.ID, "dst")
	payload := []byte("id\n1\n2\n")
	seedFile(t, st, ws.ID, src.ID, "Files/in.csv", payload)

	child := createNamedPipeline(t, st, ws.ID, "child", `{"properties":{"activities":[
        {"name":"Move","type":"Copy","typeProperties":{
          "source":{"location":{"itemId":"`+src.ID+`","path":"Files/in.csv"}},
          "sink":{"location":{"itemId":"`+dst.ID+`","path":"Files/out.csv"}}}}
      ]}}`)
	parent := createNamedPipeline(t, st, ws.ID, "parent", `{"properties":{"activities":[
        {"name":"Call","type":"ExecutePipeline","typeProperties":{
          "pipeline":{"referenceName":"`+child.ID+`","type":"PipelineReference"},"waitOnCompletion":true}}
      ]}}`)

	_, jid := runJob(t, a, ws.ID, parent.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, parent.ID, jid); s != "Completed" {
		t.Fatalf("parent job = %s, want Completed", s)
	}
	// The child's Copy really ran through the storage layer.
	got, err := st.GetOneLakePath(dst.ID, "Files/out.csv")
	if err != nil || string(got.Content) != string(payload) {
		t.Fatalf("child copy effect missing: %q (err %v)", got.Content, err)
	}
	// The Invoke activity reports the child's identity + status.
	_, runs := activityRuns(t, a, ws.ID, parent.ID, jid)
	out := outputOf(runs, "Call")
	if out["status"] != "Succeeded" || out["pipelineId"] != child.ID {
		t.Fatalf("invoke output = %+v", out)
	}
}

// TestPipelineInvokeParametersFlow: parameters passed to the child reach its
// expressions — the child copies to a param-named sink path.
func TestPipelineInvokeParametersFlow(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	src := seedLakehouse(t, st, ws.ID, "src")
	dst := seedLakehouse(t, st, ws.ID, "dst")
	seedFile(t, st, ws.ID, src.ID, "Files/in", []byte("x"))

	child := createNamedPipeline(t, st, ws.ID, "child", `{"properties":{
        "parameters":{"out":{"type":"String","defaultValue":"default"}},
        "activities":[
          {"name":"Move","type":"Copy","typeProperties":{
            "source":{"location":{"itemId":"`+src.ID+`","path":"Files/in"}},
            "sink":{"location":{"itemId":"`+dst.ID+`","path":"@concat('Files/',pipeline().parameters.out)"}}}}
        ]}}`)
	parent := createNamedPipeline(t, st, ws.ID, "parent", `{"properties":{"activities":[
        {"name":"Call","type":"ExecutePipeline","typeProperties":{
          "pipeline":{"referenceName":"`+child.ID+`"},"parameters":{"out":"passed"}}}
      ]}}`)

	_, jid := runJob(t, a, ws.ID, parent.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, parent.ID, jid); s != "Completed" {
		t.Fatalf("parent job = %s, want Completed", s)
	}
	if _, err := st.GetOneLakePath(dst.ID, "Files/passed"); err != nil {
		t.Fatalf("passed parameter did not reach child: %v", err)
	}
	if _, err := st.GetOneLakePath(dst.ID, "Files/default"); err == nil {
		t.Fatalf("child used the default, not the passed parameter")
	}
}

// TestPipelineInvokeChildFailurePropagates: with waitOnCompletion (the default),
// a child failure fails the Invoke activity and the parent job.
func TestPipelineInvokeChildFailurePropagates(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	child := createNamedPipeline(t, st, ws.ID, "child", `{"properties":{"activities":[
        {"name":"RunNb","type":"TridentNotebook","typeProperties":{"notebookId":"missing"}}
      ]}}`)
	parent := createNamedPipeline(t, st, ws.ID, "parent", `{"properties":{"activities":[
        {"name":"Call","type":"ExecutePipeline","typeProperties":{"pipeline":{"referenceName":"`+child.ID+`"}}}
      ]}}`)
	_, jid := runJob(t, a, ws.ID, parent.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, parent.ID, jid); s != "Failed" {
		t.Fatalf("parent job = %s, want Failed", s)
	}
}

// TestPipelineInvokeWaitFalse: waitOnCompletion=false is fire-and-forget — a
// failing child does not fail the parent; the child's status is still reported.
func TestPipelineInvokeWaitFalse(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	child := createNamedPipeline(t, st, ws.ID, "child", `{"properties":{"activities":[
        {"name":"RunNb","type":"TridentNotebook","typeProperties":{"notebookId":"missing"}}
      ]}}`)
	parent := createNamedPipeline(t, st, ws.ID, "parent", `{"properties":{"activities":[
        {"name":"Call","type":"ExecutePipeline","typeProperties":{
          "pipeline":{"referenceName":"`+child.ID+`"},"waitOnCompletion":false}}
      ]}}`)
	_, jid := runJob(t, a, ws.ID, parent.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, parent.ID, jid); s != "Completed" {
		t.Fatalf("parent job = %s, want Completed (fire-and-forget)", s)
	}
	_, runs := activityRuns(t, a, ws.ID, parent.ID, jid)
	if out := outputOf(runs, "Call"); out["status"] != "Failed" {
		t.Fatalf("expected child status Failed reported, got %+v", out)
	}
}

// TestPipelineInvokeCycleDetected: a pipeline invoking itself fails loudly
// instead of recursing forever.
func TestPipelineInvokeCycleDetected(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	pl := createNamedPipeline(t, st, ws.ID, "loop", `{"properties":{"activities":[]}}`)
	// Rewrite its definition to invoke itself.
	self := `{"properties":{"activities":[
        {"name":"Call","type":"ExecutePipeline","typeProperties":{"pipeline":{"referenceName":"` + pl.ID + `"}}}
      ]}}`
	payload := base64.StdEncoding.EncodeToString([]byte(self))
	if err := st.SetDefinition(pl.ID, []store.DefinitionPart{{Path: "pipeline-content.json", Payload: payload, PayloadType: "InlineBase64"}}); err != nil {
		t.Fatal(err)
	}
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("self-invoking pipeline = %s, want Failed", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	if e, _ := runs[0]["error"].(string); !strings.Contains(e, "cycle") {
		t.Fatalf("expected a cycle error, got %+v", runs)
	}
}

// TestPipelineInvokeUnknownChild: referencing a non-existent pipeline fails.
func TestPipelineInvokeUnknownChild(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	parent := createNamedPipeline(t, st, ws.ID, "parent", `{"properties":{"activities":[
        {"name":"Call","type":"ExecutePipeline","typeProperties":{"pipeline":{"referenceName":"nope"}}}
      ]}}`)
	_, jid := runJob(t, a, ws.ID, parent.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, parent.ID, jid); s != "Failed" {
		t.Fatalf("unknown-child invoke = %s, want Failed", s)
	}
}

// TestPipelineInvokeByName: the child pipeline resolves by display name too.
func TestPipelineInvokeByName(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	dst := seedLakehouse(t, st, ws.ID, "dst")
	src := seedLakehouse(t, st, ws.ID, "src")
	seedFile(t, st, ws.ID, src.ID, "Files/in", []byte("hi"))
	createNamedPipeline(t, st, ws.ID, "worker", `{"properties":{"activities":[
        {"name":"Move","type":"Copy","typeProperties":{
          "source":{"location":{"itemId":"`+src.ID+`","path":"Files/in"}},
          "sink":{"location":{"itemId":"`+dst.ID+`","path":"Files/out"}}}}
      ]}}`)
	parent := createNamedPipeline(t, st, ws.ID, "parent", `{"properties":{"activities":[
        {"name":"Call","type":"ExecutePipeline","typeProperties":{"pipeline":{"referenceName":"worker"}}}
      ]}}`)
	_, jid := runJob(t, a, ws.ID, parent.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, parent.ID, jid); s != "Completed" {
		t.Fatalf("invoke-by-name = %s, want Completed", s)
	}
	if got, err := st.GetOneLakePath(dst.ID, "Files/out"); err != nil || string(got.Content) != "hi" {
		t.Fatalf("by-name child effect missing: %q (err %v)", got.Content, err)
	}
}

// TestPipelineInvokeByPipelineId: the flat `pipelineId` typeProperty (not the
// nested pipeline.referenceName) also resolves, and it accepts an expression.
func TestPipelineInvokeByPipelineId(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	dst := seedLakehouse(t, st, ws.ID, "dst")
	src := seedLakehouse(t, st, ws.ID, "src")
	seedFile(t, st, ws.ID, src.ID, "Files/in", []byte("z"))
	child := createNamedPipeline(t, st, ws.ID, "child", `{"properties":{"activities":[
        {"name":"Move","type":"Copy","typeProperties":{
          "source":{"location":{"itemId":"`+src.ID+`","path":"Files/in"}},
          "sink":{"location":{"itemId":"`+dst.ID+`","path":"Files/out"}}}}
      ]}}`)
	parent := createNamedPipeline(t, st, ws.ID, "parent", `{"properties":{
        "parameters":{"target":{"type":"String","defaultValue":"`+child.ID+`"}},
        "activities":[
          {"name":"Call","type":"ExecutePipeline","typeProperties":{"pipelineId":"@pipeline().parameters.target"}}
        ]}}`)
	_, jid := runJob(t, a, ws.ID, parent.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, parent.ID, jid); s != "Completed" {
		t.Fatalf("invoke-by-pipelineId = %s, want Completed", s)
	}
	if _, err := st.GetOneLakePath(dst.ID, "Files/out"); err != nil {
		t.Fatalf("child effect missing: %v", err)
	}
}

// TestPipelineInvokeBadParameters: non-object parameters fail the activity
// loudly instead of silently ignoring them.
func TestPipelineInvokeBadParameters(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	child := createNamedPipeline(t, st, ws.ID, "child", `{"properties":{"activities":[]}}`)
	parent := createNamedPipeline(t, st, ws.ID, "parent", `{"properties":{"activities":[
        {"name":"Call","type":"ExecutePipeline","typeProperties":{
          "pipeline":{"referenceName":"`+child.ID+`"},"parameters":["not","an","object"]}}
      ]}}`)
	_, jid := runJob(t, a, ws.ID, parent.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, parent.ID, jid); s != "Failed" {
		t.Fatalf("bad-parameters invoke = %s, want Failed", s)
	}
}

// TestPipelineInvokeUnknownWorkspace: a pipeline reference in an unknown
// workspace fails the activity.
func TestPipelineInvokeUnknownWorkspace(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	parent := createNamedPipeline(t, st, ws.ID, "parent", `{"properties":{"activities":[
        {"name":"Call","type":"ExecutePipeline","typeProperties":{
          "pipeline":{"referenceName":"child","workspaceId":"no-such-ws"}}}
      ]}}`)
	_, jid := runJob(t, a, ws.ID, parent.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, parent.ID, jid); s != "Failed" {
		t.Fatalf("unknown-workspace invoke = %s, want Failed", s)
	}
}

// TestPipelineRetryPolicyJob: an activity policy.retry drives real re-runs; a
// TridentNotebook to a missing notebook fails, retries, then fails the job.
func TestPipelineRetryPolicyJob(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	content := `{"properties":{"activities":[
        {"name":"RunNb","type":"TridentNotebook","policy":{"retry":2},"typeProperties":{"notebookId":"missing"}}
      ]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job status = %s, want Failed", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	// Recorded once (not 3×), with the retry count.
	if len(runs) != 1 {
		t.Fatalf("expected 1 activity record after retries, got %d", len(runs))
	}
	if r, ok := runs[0]["retryAttempt"].(float64); !ok || r != 2 {
		t.Fatalf("retryAttempt = %v, want 2", runs[0]["retryAttempt"])
	}
}

// seedLakehouse creates a Lakehouse item, optionally seeding a OneLake file.
func seedLakehouse(t *testing.T, st *store.Store, wid, name string) *store.Item {
	t.Helper()
	it := &store.Item{WorkspaceID: wid, Type: "Lakehouse", DisplayName: name}
	if err := st.CreateItem(it, nil); err != nil {
		t.Fatal(err)
	}
	return it
}

func seedFile(t *testing.T, st *store.Store, wid, itemID, rel string, content []byte) {
	t.Helper()
	if err := st.CreateOneLakePath(&store.OneLakePath{WorkspaceID: wid, ItemID: itemID, RelPath: rel, Content: content}, false); err != nil {
		t.Fatal(err)
	}
}

// TestPipelineCopyActivityRealBytes: a Copy activity moves real bytes from one
// lakehouse OneLake path to another, with an expression-resolved sink path.
func TestPipelineCopyActivityRealBytes(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	src := seedLakehouse(t, st, ws.ID, "src")
	dst := seedLakehouse(t, st, ws.ID, "dst")
	payload := []byte("id,name\n1,alice\n2,bob\n")
	seedFile(t, st, ws.ID, src.ID, "Files/in.csv", payload)

	content := `{"properties":{
      "parameters":{"out":{"type":"String","defaultValue":"out.csv"}},
      "activities":[
        {"name":"Move","type":"Copy","typeProperties":{
          "source":{"location":{"itemId":"` + src.ID + `","path":"Files/in.csv"}},
          "sink":{"location":{"itemId":"` + dst.ID + `","path":"@concat('Files/',pipeline().parameters.out)"}}
        }}
      ]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		t.Fatalf("job status = %s", s)
	}

	// The bytes really landed at the sink, identical to the source.
	got, err := st.GetOneLakePath(dst.ID, "Files/out.csv")
	if err != nil {
		t.Fatalf("sink file missing: %v", err)
	}
	if string(got.Content) != string(payload) {
		t.Fatalf("sink content = %q, want %q", got.Content, payload)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	out := runs[0]["output"].(map[string]any)
	if out["filesWritten"].(float64) != 1 || int(out["dataWritten"].(float64)) != len(payload) {
		t.Fatalf("copy output = %+v", out)
	}
	lineage := out["lineage"].(map[string]any)
	if lineage["sourceItemId"] != src.ID || lineage["targetPath"] != "Files/out.csv" {
		t.Fatalf("activity lineage = %+v", lineage)
	}
	w := do(a.listLineage, viewer, "GET", "", map[string]string{"wid": ws.ID})
	if w.Code != http.StatusOK {
		t.Fatalf("viewer lineage = %d %s", w.Code, w.Body.Bytes())
	}
	var page struct {
		Value []store.LineageEdge `json:"value"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Value) != 1 || page.Value[0].JobID != jid || page.Value[0].ActivityName != "Move" {
		t.Fatalf("lineage page = %+v", page.Value)
	}
	if w := do(a.listLineage, nobody, "GET", "", map[string]string{"wid": ws.ID}); w.Code != http.StatusForbidden {
		t.Fatalf("ungranted lineage = %d", w.Code)
	}
}

// TestPipelineCopyDirectory: copying a directory moves the whole subtree,
// preserving relative structure.
func TestPipelineCopyDirectory(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	src := seedLakehouse(t, st, ws.ID, "src")
	dst := seedLakehouse(t, st, ws.ID, "dst")
	seedFile(t, st, ws.ID, src.ID, "Files/in/a.txt", []byte("A"))
	seedFile(t, st, ws.ID, src.ID, "Files/in/sub/b.txt", []byte("BB"))
	// The parent directory is deliberately implicit, as it is for Delta data
	// written through ADLS clients; the nonempty prefix still copies recursively.

	content := `{"properties":{"activities":[
        {"name":"Move","type":"Copy","typeProperties":{
          "source":{"location":{"itemId":"` + src.ID + `","path":"Files/in"}},
          "sink":{"location":{"itemId":"` + dst.ID + `","path":"Files/out"}}
        }}
      ]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		t.Fatalf("job status = %s", s)
	}
	for rel, want := range map[string]string{"Files/out/a.txt": "A", "Files/out/sub/b.txt": "BB"} {
		got, err := st.GetOneLakePath(dst.ID, rel)
		if err != nil || string(got.Content) != want {
			t.Fatalf("%s = %q (err %v), want %q", rel, got.Content, err, want)
		}
	}
}

// TestPipelineCopyByName: source/sink resolve by workspace + item *name*
// (not just GUID), and an unknown workspace fails the activity.
func TestPipelineCopyByName(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st) // DisplayName "w"
	src := seedLakehouse(t, st, ws.ID, "src")
	dst := seedLakehouse(t, st, ws.ID, "dst")
	seedFile(t, st, ws.ID, src.ID, "Files/a", []byte("hi"))

	// Reference workspace by name "w" and items by "name.Lakehouse".
	ok := `{"properties":{"activities":[
        {"name":"Move","type":"Copy","typeProperties":{
          "source":{"location":{"workspaceId":"w","itemId":"src.Lakehouse","path":"Files/a"}},
          "sink":{"location":{"workspaceId":"w","itemId":"dst.Lakehouse","path":"Files/a"}}
        }}]}}`
	pl := createPipeline(t, st, ws.ID, ok)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		t.Fatalf("by-name copy = %s", s)
	}
	got, err := st.GetOneLakePath(dst.ID, "Files/a")
	if err != nil || string(got.Content) != "hi" {
		t.Fatalf("by-name sink = %q (err %v)", got.Content, err)
	}

	// Unknown workspace → fail.
	bad := `{"properties":{"activities":[
        {"name":"Move","type":"Copy","typeProperties":{
          "source":{"location":{"workspaceId":"nope","itemId":"src.Lakehouse","path":"Files/a"}},
          "sink":{"location":{"itemId":"dst.Lakehouse","path":"Files/a"}}
        }}]}}`
	pl2 := createPipeline(t, st, ws.ID, bad)
	_, jid2 := runJob(t, a, ws.ID, pl2.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl2.ID, jid2); s != "Failed" {
		t.Fatalf("unknown-workspace copy = %s, want Failed", s)
	}
}

// TestPipelineCopyFailures: missing source path and missing itemId fail the
// activity (and the job) rather than silently "succeeding".
func TestPipelineCopyFailures(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	src := seedLakehouse(t, st, ws.ID, "src")
	dst := seedLakehouse(t, st, ws.ID, "dst")

	// Source file does not exist.
	c1 := `{"properties":{"activities":[
        {"name":"Move","type":"Copy","typeProperties":{
          "source":{"location":{"itemId":"` + src.ID + `","path":"Files/nope.csv"}},
          "sink":{"location":{"itemId":"` + dst.ID + `","path":"Files/x"}}
        }}]}}`
	pl := createPipeline(t, st, ws.ID, c1)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("missing-source job = %s, want Failed", s)
	}

	// Sink missing itemId.
	c2 := `{"properties":{"activities":[
        {"name":"Move","type":"Copy","typeProperties":{
          "source":{"location":{"itemId":"` + src.ID + `","path":"Files/in"}},
          "sink":{"location":{"path":"Files/x"}}
        }}]}}`
	seedFile(t, st, ws.ID, src.ID, "Files/in", []byte("x"))
	pl2 := createPipeline(t, st, ws.ID, c2)
	_, jid2 := runJob(t, a, ws.ID, pl2.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl2.ID, jid2); s != "Failed" {
		t.Fatalf("missing-itemId job = %s, want Failed", s)
	}
}

// TestQueryActivityRunsMissing: a non-pipeline job has no activity-run detail.
func TestQueryActivityRunsMissing(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	nb := &store.Item{WorkspaceID: ws.ID, Type: "Notebook", DisplayName: "nb"}
	if err := st.CreateItem(nb, nil); err != nil {
		t.Fatal(err)
	}
	_, jid := runJob(t, a, ws.ID, nb.ID, "jobType=RunNotebook", "")
	w := do(a.queryActivityRuns, admin, "POST", "",
		map[string]string{"wid": ws.ID, "iid": nb.ID, "jid": jid})
	if w.Code != 404 {
		t.Fatalf("expected 404 for non-pipeline job, got %d", w.Code)
	}
}

// outputOf finds a named activity's output object in a run list.
func outputOf(runs []map[string]any, name string) map[string]any {
	for _, r := range runs {
		if r["activityName"] == name {
			if o, ok := r["output"].(map[string]any); ok {
				return o
			}
		}
	}
	return nil
}

// TestPipelineLookupCSV: a Lookup reads real rows from a CSV in OneLake and its
// first row flows into a downstream expression.
func TestPipelineLookupCSV(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	src := seedLakehouse(t, st, ws.ID, "src")
	seedFile(t, st, ws.ID, src.ID, "Files/ref.csv", []byte("id,name\n1,alice\n2,bob\n"))

	content := `{"properties":{
      "variables":{"who":{"type":"String"}},
      "activities":[
        {"name":"Lk","type":"Lookup","typeProperties":{
          "source":{"location":{"itemId":"` + src.ID + `","path":"Files/ref.csv"}}}},
        {"name":"Use","type":"SetVariable","dependsOn":[{"activity":"Lk","dependencyConditions":["Succeeded"]}],
          "typeProperties":{"variableName":"who","value":"@activity('Lk').output.firstRow.name"}}
      ]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		t.Fatalf("job status = %s", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	lk := outputOf(runs, "Lk")
	if lk["count"].(float64) != 2 {
		t.Fatalf("lookup count = %v", lk["count"])
	}
	if lk["firstRow"].(map[string]any)["name"] != "alice" {
		t.Fatalf("firstRow = %+v", lk["firstRow"])
	}
	// The value really flowed into the downstream SetVariable.
	if outputOf(runs, "Use")["value"] != "alice" {
		t.Fatalf("downstream variable = %+v", outputOf(runs, "Use"))
	}
}

// TestPipelineLookupJSONAllRows: firstRowOnly=false returns the whole array.
func TestPipelineLookupJSONAllRows(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	src := seedLakehouse(t, st, ws.ID, "src")
	seedFile(t, st, ws.ID, src.ID, "Files/data.json", []byte(`[{"k":1},{"k":2},{"k":3}]`))

	content := `{"properties":{"activities":[
        {"name":"Lk","type":"Lookup","typeProperties":{
          "firstRowOnly":false,
          "source":{"location":{"itemId":"` + src.ID + `","path":"Files/data.json"}}}}
      ]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		t.Fatalf("job status = %s", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	lk := outputOf(runs, "Lk")
	if lk["count"].(float64) != 3 || len(lk["value"].([]any)) != 3 {
		t.Fatalf("json lookup = %+v", lk)
	}
}

// TestPipelineLookupMissing: a missing source fails the activity (loudly).
func TestPipelineLookupMissing(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	src := seedLakehouse(t, st, ws.ID, "src")
	content := `{"properties":{"activities":[
        {"name":"Lk","type":"Lookup","typeProperties":{
          "source":{"location":{"itemId":"` + src.ID + `","path":"Files/none.csv"}}}}
      ]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("missing lookup source = %s, want Failed", s)
	}
}

// TestPipelineGetMetadataFile: stats a real file (exists/size/type/name).
func TestPipelineGetMetadataFile(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	src := seedLakehouse(t, st, ws.ID, "src")
	seedFile(t, st, ws.ID, src.ID, "Files/a.bin", []byte("hello world"))

	content := `{"properties":{"activities":[
        {"name":"Meta","type":"GetMetadata","typeProperties":{
          "fieldList":["exists","size","itemType","itemName"],
          "dataset":{"location":{"itemId":"` + src.ID + `","path":"Files/a.bin"}}}}
      ]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		t.Fatalf("job status = %s", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	m := outputOf(runs, "Meta")
	if m["exists"] != true || m["itemType"] != "File" || m["itemName"] != "a.bin" || m["size"].(float64) != 11 {
		t.Fatalf("metadata = %+v", m)
	}
}

// TestPipelineGetMetadataDir: childItems lists a directory's entries.
func TestPipelineGetMetadataDir(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	src := seedLakehouse(t, st, ws.ID, "src")
	if err := st.CreateOneLakePath(&store.OneLakePath{WorkspaceID: ws.ID, ItemID: src.ID, RelPath: "Files/d", IsDir: true, Content: []byte{}}, false); err != nil {
		t.Fatal(err)
	}
	seedFile(t, st, ws.ID, src.ID, "Files/d/a.txt", []byte("A"))
	seedFile(t, st, ws.ID, src.ID, "Files/d/b.txt", []byte("B"))

	content := `{"properties":{"activities":[
        {"name":"Meta","type":"GetMetadata","typeProperties":{
          "fieldList":["itemType","childItems"],
          "dataset":{"location":{"itemId":"` + src.ID + `","path":"Files/d"}}}}
      ]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		t.Fatalf("job status = %s", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	m := outputOf(runs, "Meta")
	if m["itemType"] != "Folder" {
		t.Fatalf("itemType = %v", m["itemType"])
	}
	names := map[string]bool{}
	for _, ci := range m["childItems"].([]any) {
		names[ci.(map[string]any)["name"].(string)] = true
	}
	if !names["a.txt"] || !names["b.txt"] {
		t.Fatalf("childItems = %+v", m["childItems"])
	}
}

// TestPipelineGetMetadataMissing: a missing path reports exists:false, not error.
func TestPipelineGetMetadataMissing(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	src := seedLakehouse(t, st, ws.ID, "src")
	content := `{"properties":{"activities":[
        {"name":"Meta","type":"GetMetadata","typeProperties":{
          "fieldList":["exists"],
          "dataset":{"location":{"itemId":"` + src.ID + `","path":"Files/nope"}}}}
      ]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		t.Fatalf("job status = %s", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	if outputOf(runs, "Meta")["exists"] != false {
		t.Fatalf("missing path metadata = %+v", outputOf(runs, "Meta"))
	}
}

// TestPipelineLookupOnDirectory: a Lookup pointed at a directory fails loudly.
func TestPipelineLookupOnDirectory(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	src := seedLakehouse(t, st, ws.ID, "src")
	if err := st.CreateOneLakePath(&store.OneLakePath{WorkspaceID: ws.ID, ItemID: src.ID, RelPath: "Files/d", IsDir: true, Content: []byte{}}, false); err != nil {
		t.Fatal(err)
	}
	content := `{"properties":{"activities":[
        {"name":"Lk","type":"Lookup","typeProperties":{
          "source":{"location":{"itemId":"` + src.ID + `","path":"Files/d"}}}}
      ]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("lookup on directory = %s, want Failed", s)
	}
}

// TestPipelineGetMetadataDefaultFields: with no fieldList, GetMetadata returns
// the default field set (including lastModified, set by the store on write).
func TestPipelineGetMetadataDefaultFields(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	src := seedLakehouse(t, st, ws.ID, "src")
	seedFile(t, st, ws.ID, src.ID, "Files/a.bin", []byte("data"))
	content := `{"properties":{"activities":[
        {"name":"Meta","type":"GetMetadata","typeProperties":{
          "dataset":{"location":{"itemId":"` + src.ID + `","path":"Files/a.bin"}}}}
      ]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		t.Fatalf("job status = %s", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	m := outputOf(runs, "Meta")
	if m["exists"] != true || m["itemName"] != "a.bin" || m["size"].(float64) != 4 {
		t.Fatalf("default metadata = %+v", m)
	}
	if _, ok := m["lastModified"]; !ok {
		t.Fatalf("expected lastModified in default fields: %+v", m)
	}
}

// TestPipelineGetMetadataNoLocation: a GetMetadata with no location fails.
func TestPipelineGetMetadataNoLocation(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	content := `{"properties":{"activities":[
        {"name":"Meta","type":"GetMetadata","typeProperties":{"fieldList":["exists"]}}
      ]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("no-location getMetadata = %s, want Failed", s)
	}
}

// TestPipelineLookupNoSource: a Lookup with no source/dataset fails loudly.
func TestPipelineLookupNoSource(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	content := `{"properties":{"activities":[
        {"name":"Lk","type":"Lookup","typeProperties":{"firstRowOnly":true}}
      ]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("no-source lookup = %s, want Failed", s)
	}
}

// TestPipelineLookupEmptyCSV: firstRowOnly over a header-only CSV yields an
// empty first row and count 0 (no crash).
func TestPipelineLookupEmptyCSV(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	src := seedLakehouse(t, st, ws.ID, "src")
	seedFile(t, st, ws.ID, src.ID, "Files/h.csv", []byte("a,b\n"))
	content := `{"properties":{"activities":[
        {"name":"Lk","type":"Lookup","typeProperties":{
          "source":{"location":{"itemId":"` + src.ID + `","path":"Files/h.csv"}}}}
      ]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		t.Fatalf("job status = %s", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	if outputOf(runs, "Lk")["count"].(float64) != 0 {
		t.Fatalf("empty csv count = %+v", outputOf(runs, "Lk"))
	}
}

// lookupRow is the fixture schema for the Delta/Parquet Lookup tests.
type lookupRow struct {
	Region string `parquet:"region"`
	Amount int64  `parquet:"amount"`
}

func seedParquetBytes(t *testing.T, rows []lookupRow) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := parquet.NewGenericWriter[lookupRow](&buf)
	if _, err := w.Write(rows); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// seedDeltaTable writes a minimal Delta table (one Parquet file + a
// single-commit _delta_log) under Tables/<name> — the same shape the
// warehouse's own Delta reader is proven against.
func seedDeltaTable(t *testing.T, st *store.Store, wid, itemID, name string, rows []lookupRow) {
	t.Helper()
	seedFile(t, st, wid, itemID, "Tables/"+name+"/part-0.parquet", seedParquetBytes(t, rows))
	seedFile(t, st, wid, itemID, "Tables/"+name+"/_delta_log/00000000000000000000.json",
		[]byte(`{"add":{"path":"part-0.parquet"}}`))
}

// TestPipelineLookupDelta: Lookup auto-detects a bare Tables/<name> root as a
// Delta table (no format hint needed) and reads its real rows.
func TestPipelineLookupDelta(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	src := seedLakehouse(t, st, ws.ID, "src")
	seedDeltaTable(t, st, ws.ID, src.ID, "sales", []lookupRow{{"us", 80}, {"eu", 60}})

	content := `{"properties":{"activities":[
        {"name":"Lk","type":"Lookup","typeProperties":{
          "firstRowOnly":false,
          "source":{"location":{"itemId":"` + src.ID + `","path":"Tables/sales"}}}}
      ]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		t.Fatalf("job status = %s", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	lk := outputOf(runs, "Lk")
	if lk["count"].(float64) != 2 {
		t.Fatalf("delta lookup count = %v", lk["count"])
	}
	rows := lk["value"].([]any)
	first := rows[0].(map[string]any)
	if first["region"] != "us" {
		t.Fatalf("first row region = %v, want us", first["region"])
	}
	// amount is a real numeric column value, not a stringified CSV cell — it
	// round-trips through queryActivityRuns' JSON as float64, like every other
	// number in these activity-output assertions.
	if amt, ok := first["amount"].(float64); !ok || amt != 80 {
		t.Fatalf("first row amount = %v (%T), want numeric 80", first["amount"], first["amount"])
	}
}

// TestPipelineLookupDeltaExplicitFormat: an explicit format:"Delta" hint works
// the same as the path-shape inference.
func TestPipelineLookupDeltaExplicitFormat(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	src := seedLakehouse(t, st, ws.ID, "src")
	seedDeltaTable(t, st, ws.ID, src.ID, "sales", []lookupRow{{"us", 80}})

	content := `{"properties":{"activities":[
        {"name":"Lk","type":"Lookup","typeProperties":{
          "format":{"type":"DeltaFormat"},
          "source":{"location":{"itemId":"` + src.ID + `","path":"Tables/sales"}}}}
      ]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		t.Fatalf("job status = %s", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	if outputOf(runs, "Lk")["count"].(float64) != 1 {
		t.Fatalf("explicit-format delta lookup = %+v", outputOf(runs, "Lk"))
	}
}

// TestPipelineLookupDeltaMissingTable: a Tables/<name> root with no Delta
// commits fails the activity (loudly), not a silent empty result.
func TestPipelineLookupDeltaMissingTable(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	src := seedLakehouse(t, st, ws.ID, "src")
	content := `{"properties":{"activities":[
        {"name":"Lk","type":"Lookup","typeProperties":{
          "source":{"location":{"itemId":"` + src.ID + `","path":"Tables/nope"}}}}
      ]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("missing delta table = %s, want Failed", s)
	}
}

// TestPipelineLookupParquetFile: a standalone .parquet file (not a Delta
// table) is read directly, detected by extension.
func TestPipelineLookupParquetFile(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	src := seedLakehouse(t, st, ws.ID, "src")
	seedFile(t, st, ws.ID, src.ID, "Files/snap.parquet", seedParquetBytes(t, []lookupRow{{"us", 5}, {"eu", 9}}))

	content := `{"properties":{"activities":[
        {"name":"Lk","type":"Lookup","typeProperties":{
          "source":{"location":{"itemId":"` + src.ID + `","path":"Files/snap.parquet"}}}}
      ]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		t.Fatalf("job status = %s", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	if outputOf(runs, "Lk")["firstRow"].(map[string]any)["region"] != "us" {
		t.Fatalf("parquet lookup firstRow = %+v", outputOf(runs, "Lk"))
	}
}

// TestPipelineLookupNumericFlowsThroughAnExpression.
//
// THE GAP THIS CLOSES. Every Lookup test above asserts a STRING — `region`,
// `name`. The fixtures have carried a numeric column the whole time
// (`lookupRow.Amount`) and nothing ever read it, so nothing exercised the
// coercion an expression applies to it.
//
// That mattered, because `toNumber` listed only float64 and int. Values from a
// CSV or JSON Lookup are JSON-decoded and arrive as float64, which it handled;
// values from a PARQUET or DELTA Lookup arrive as the warehouse reader's own
// types — int64 for a bigint, int32 for a Delta int — and every one of those
// coerced to 0. So `@add(activity('Lk').output.firstRow.amount, 1)` returned 1
// rather than 6, on every build up to v0.16.0, silently.
//
// The string assertions could not see it: a string flows through untouched. It
// is the same map-vs-route gap as the nested-column bug, one system over — the
// value was carried correctly and destroyed one stage later, by the consumer of
// it rather than the producer.
//
// Reported by contoso-data-platform, who were NOT affected and said why that
// does not count as reassurance: they orchestrate with a plain Python runner
// and have no DataPipeline items at all, so the evaluator was never exercised
// on any build. Anyone using DataPipelines — the more Fabric-native choice —
// was exposed, and nothing raises.
func TestPipelineLookupNumericFlowsThroughAnExpression(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	src := seedLakehouse(t, st, ws.ID, "src")
	seedFile(t, st, ws.ID, src.ID, "Files/snap.parquet",
		seedParquetBytes(t, []lookupRow{{"us", 5}, {"eu", 9}}))

	content := `{"properties":{"activities":[
        {"name":"Lk","type":"Lookup","typeProperties":{
          "source":{"location":{"itemId":"` + src.ID + `","path":"Files/snap.parquet"}}}},
        {"name":"Sum","type":"SetVariable","dependsOn":[{"activity":"Lk","dependencyConditions":["Succeeded"]}],
          "typeProperties":{"variableName":"total","value":"@string(add(activity('Lk').output.firstRow.amount,1))"}},
        {"name":"Branch","type":"SetVariable","dependsOn":[{"activity":"Lk","dependencyConditions":["Succeeded"]}],
          "typeProperties":{"variableName":"nonzero","value":"@string(bool(activity('Lk').output.firstRow.amount))"}}
      ]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		t.Fatalf("job status = %s", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)

	// 6, not 1. `1` is what an int64 coercing to 0 produces, and it is a
	// plausible-looking number — there is no loud half here either.
	if got := outputOf(runs, "Sum")["value"]; got != "6" {
		t.Errorf("add(amount,1) = %v; want \"6\" — a bigint column from a "+
			"parquet Lookup must not coerce to 0", got)
	}
	// The branch form, where the cost is a wrong path rather than a wrong
	// number: a non-zero amount must be true.
	if got := outputOf(runs, "Branch")["value"]; got != "true" {
		t.Errorf("bool(amount) = %v; want \"true\" — a non-zero bigint reading "+
			"as false takes the wrong branch", got)
	}
}

// TestPipelineScriptNoBackend: a Script activity fails loudly (not silently
// stubbed) when no warehouse SQL backend is attached — the honest 🔴, not a
// pretend success.
func TestPipelineScriptNoBackend(t *testing.T) {
	a, st := newAPI(t) // a.SQLDB is nil: no --warehouse-sql-url in this test process
	ws := seedWorkspace(t, st)
	wh := &store.Item{WorkspaceID: ws.ID, Type: "Warehouse", DisplayName: "wh"}
	if err := st.CreateItem(wh, nil); err != nil {
		t.Fatal(err)
	}
	content := `{"properties":{"activities":[
        {"name":"Sc","type":"Script","typeProperties":{
          "database":{"itemId":"` + wh.ID + `"},
          "scripts":[{"type":"Query","text":"SELECT 1"}]}}
      ]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("script with no SQL backend = %s, want Failed", s)
	}
}

// TestPipelineStoredProcedureNoBackend: same honest-failure contract for
// SqlServerStoredProcedure.
func TestPipelineStoredProcedureNoBackend(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	wh := &store.Item{WorkspaceID: ws.ID, Type: "Warehouse", DisplayName: "wh"}
	if err := st.CreateItem(wh, nil); err != nil {
		t.Fatal(err)
	}
	content := `{"properties":{"activities":[
        {"name":"Sp","type":"SqlServerStoredProcedure","typeProperties":{
          "database":{"itemId":"` + wh.ID + `"},
          "storedProcedureName":"dbo.DoesNotMatter"}}
      ]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("stored procedure with no SQL backend = %s, want Failed", s)
	}
}

// TestPipelineScriptNoDatabaseRef: a Script activity with no database
// reference fails loudly.
func TestPipelineScriptNoDatabaseRef(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	content := `{"properties":{"activities":[
        {"name":"Sc","type":"Script","typeProperties":{
          "scripts":[{"type":"Query","text":"SELECT 1"}]}}
      ]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("script with no database ref = %s, want Failed", s)
	}
}

// TestCopyAcceptsFabricWireShape: a Copy authored in Fabric — `type`
// discriminators, `datasetSettings` with a `linkedService`, `rootFolder` +
// `table` addressing — runs unchanged. Before this the emulator read only its
// own simplified `location` shape and silently ignored every Fabric field, so a
// real pipeline JSON failed with "a OneLake location is required".
func TestCopyAcceptsFabricWireShape(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	src := seedLakehouse(t, st, ws.ID, "src")
	dst := seedLakehouse(t, st, ws.ID, "dst")
	payload := []byte("parquet-ish bytes")
	seedFile(t, st, ws.ID, src.ID, "Tables/orders/part-0.parquet", payload)

	content := `{"properties":{"activities":[
      {"name":"Ingest","type":"Copy","typeProperties":{
        "source":{"type":"LakehouseTableSource",
          "datasetSettings":{"type":"LakehouseTable",
            "typeProperties":{"table":"orders","schema":"dbo"},
            "linkedService":{"properties":{"type":"Lakehouse",
              "typeProperties":{"workspaceId":"` + ws.ID + `","artifactId":"` + src.ID + `"}}}}},
        "sink":{"type":"LakehouseTableSink","tableActionOption":"Overwrite",
          "datasetSettings":{"type":"LakehouseTable",
            "typeProperties":{"table":"bronze_orders"},
            "linkedService":{"properties":{"type":"Lakehouse",
              "typeProperties":{"workspaceId":"` + ws.ID + `","artifactId":"` + dst.ID + `"}}}}}
      }}]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		t.Fatalf("job status = %s", s)
	}
	got, err := st.GetOneLakePath(dst.ID, "Tables/bronze_orders/part-0.parquet")
	if err != nil {
		t.Fatalf("sink table file missing: %v", err)
	}
	if string(got.Content) != string(payload) {
		t.Fatalf("sink content = %q", got.Content)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	lineage := runs[0]["output"].(map[string]any)["lineage"].(map[string]any)
	if lineage["sourcePath"] != "Tables/orders" || lineage["targetPath"] != "Tables/bronze_orders" {
		t.Fatalf("lineage paths = %+v", lineage)
	}
}

// TestCopyFilesRootFolderAddressing: Fabric's Files-area addressing
// (rootFolder + folderPath + fileName) resolves under Files/.
func TestCopyFilesRootFolderAddressing(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lh := seedLakehouse(t, st, ws.ID, "lake")
	payload := []byte("id,name\n1,ada\n")
	seedFile(t, st, ws.ID, lh.ID, "Files/landing/day1/customers.csv", payload)

	content := `{"properties":{"activities":[
      {"name":"Land","type":"Copy","typeProperties":{
        "source":{"type":"DelimitedTextSource","rootFolder":"Files",
          "folderPath":"landing/day1","fileName":"customers.csv",
          "datasetSettings":{"linkedService":{"properties":{"typeProperties":{"artifactId":"` + lh.ID + `"}}}}},
        "sink":{"type":"DelimitedTextSink","rootFolder":"Files",
          "folderPath":"raw","fileName":"customers.csv",
          "datasetSettings":{"linkedService":{"properties":{"typeProperties":{"artifactId":"` + lh.ID + `"}}}}}
      }}]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		t.Fatalf("job status = %s", s)
	}
	if _, err := st.GetOneLakePath(lh.ID, "Files/raw/customers.csv"); err != nil {
		t.Fatalf("sink file missing: %v", err)
	}
}

// TestCopyRejectsUnsupportedLoudly: the emulator must refuse what it cannot
// honour by name, never accept the payload and quietly do something else.
func TestCopyRejectsUnsupportedLoudly(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lh := seedLakehouse(t, st, ws.ID, "lake")
	seedFile(t, st, ws.ID, lh.ID, "Files/in.csv", []byte("x"))
	loc := `"datasetSettings":{"linkedService":{"properties":{"typeProperties":{"artifactId":"` + lh.ID + `"}}}}`

	for _, tc := range []struct{ name, source string }{
		{"external connector", `{"type":"AzureBlobStorageSource","rootFolder":"Files","fileName":"in.csv",` + loc + `}`},
		{"t-sql query", `{"type":"LakehouseTableSource","sqlReaderQuery":"SELECT 1","table":"orders",` + loc + `}`},
		{"wildcards", `{"type":"DelimitedTextSource","rootFolder":"Files","wildcardFileName":"*.csv",` + loc + `}`},
		{"time travel", `{"type":"LakehouseTableSource","table":"orders","versionAsOf":3,` + loc + `}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := `{"properties":{"activities":[
              {"name":"Bad","type":"Copy","typeProperties":{
                "source":` + tc.source + `,
                "sink":{"type":"BinarySink","rootFolder":"Files","fileName":"out.csv",` + loc + `}
              }}]}}`
			pl := createPipeline(t, st, ws.ID, content)
			_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
			if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Failed" {
				t.Fatalf("unsupported %s: job status = %s, want Failed", tc.name, s)
			}
		})
	}
}

// TestCopyRejectsUnhonourableSinkSemantics: the copy moves whole files, so it
// is an overwrite by construction. Accepting tableActionOption=Append would
// silently REPLACE the target's data instead of adding to it — data loss
// dressed as success — and MergeFiles/FlattenHierarchy would reshape the output
// we do not reshape. Overwrite and PreserveHierarchy are honoured for real.
func TestCopyRejectsUnhonourableSinkSemantics(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lh := seedLakehouse(t, st, ws.ID, "lake")
	seedFile(t, st, ws.ID, lh.ID, "Tables/orders/part-0.parquet", []byte("rows"))
	loc := `"datasetSettings":{"linkedService":{"properties":{"typeProperties":{"artifactId":"` + lh.ID + `"}}}}`

	for _, tc := range []struct {
		name, sinkOpts string
		wantFailed     bool
	}{
		{"append", `"tableActionOption":"Append",`, false}, // real since A3
		{"upsert", `"tableActionOption":"Upsert",`, true},
		{"merge files", `"copyBehavior":"MergeFiles",`, true},
		{"flatten hierarchy", `"copyBehavior":"FlattenHierarchy",`, true},
		{"overwrite", `"tableActionOption":"Overwrite",`, false},
		{"preserve hierarchy", `"copyBehavior":"PreserveHierarchy",`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dst := seedLakehouse(t, st, ws.ID, "sink-"+strings.ReplaceAll(tc.name, " ", "-"))
			content := `{"properties":{"activities":[
              {"name":"C","type":"Copy","typeProperties":{
                "source":{"type":"LakehouseTableSource","table":"orders",` + loc + `},
                "sink":{"type":"LakehouseTableSink",` + tc.sinkOpts + `"table":"bronze",
                  "datasetSettings":{"linkedService":{"properties":{"typeProperties":{"artifactId":"` + dst.ID + `"}}}}}
              }}]}}`
			pl := createPipeline(t, st, ws.ID, content)
			_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
			s := jobStatus(t, a, ws.ID, pl.ID, jid)
			if tc.wantFailed && s != "Failed" {
				t.Fatalf("%s: status = %s, want Failed (it would silently do something else)", tc.name, s)
			}
			if !tc.wantFailed && s != "Completed" {
				t.Fatalf("%s: status = %s, want Completed", tc.name, s)
			}
		})
	}
}

// TestPipelineNotebookActivityStartsRealRun: a notebook invoked from a pipeline
// must actually start a run — the Go parser produces cells and an engine has
// something to execute and report against. It previously fabricated a
// "Completed" job with no run behind it, leaving nothing for lineage or the
// notebookRunResult callback to attach to.
func TestPipelineNotebookActivityStartsRealRun(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	nb := &store.Item{WorkspaceID: ws.ID, Type: "Notebook", DisplayName: "etl"}
	body := "# Fabric notebook source\n\n# CELL ********************\n\nprint('hello')\n"
	if err := st.CreateItem(nb, []store.DefinitionPart{{
		Path: "notebook-content.py", PayloadType: "InlineBase64",
		Payload: base64.StdEncoding.EncodeToString([]byte(body))}}); err != nil {
		t.Fatal(err)
	}
	content := `{"properties":{"activities":[
      {"name":"RunEtl","type":"TridentNotebook","typeProperties":{"notebookId":"` + nb.ID + `"}}]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		t.Fatalf("pipeline status = %s", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	out := runs[0]["output"].(map[string]any)
	nbJob, _ := out["jobInstanceId"].(string)
	if nbJob == "" {
		t.Fatalf("activity output has no jobInstanceId: %+v", out)
	}
	// The run exists and the Go parser really produced cells.
	status, runJSON, err := st.GetNotebookRun(nbJob)
	if err != nil {
		t.Fatalf("no notebook run recorded for the pipeline-invoked notebook: %v", err)
	}
	if status != "Pending" {
		t.Fatalf("run status = %q, want Pending (no engine has reported yet)", status)
	}
	if !strings.Contains(runJSON, "hello") {
		t.Fatalf("parsed cells missing the notebook source: %s", runJSON)
	}
	if out["status"] != "Pending" {
		t.Fatalf("activity status = %v, want Pending — the run has not been executed", out["status"])
	}
}

// TestCopyPathAddressingVariants: the addressing styles Fabric emits all map to
// the right OneLake path — schema-qualified tables, an explicit Files/ prefix,
// a bare folder with no file name, and the error when Tables has no table.
func TestCopyPathAddressingVariants(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lh := seedLakehouse(t, st, ws.ID, "lake")
	loc := `"datasetSettings":{"linkedService":{"properties":{"typeProperties":{"artifactId":"` + lh.ID + `"}}}}`

	for _, tc := range []struct {
		name, source, seed, want string
		fail                     bool
	}{
		{name: "schema-qualified table",
			source: `{"type":"LakehouseTableSource","table":"orders","schema":"sales",` + loc + `}`,
			seed:   "Tables/sales/orders/part-0.parquet", want: "Tables/sales/orders/part-0.parquet"},
		{name: "folderPath already rooted at Files",
			source: `{"type":"BinarySource","folderPath":"Files/raw","fileName":"a.bin",` + loc + `}`,
			seed:   "Files/raw/a.bin", want: "Files/raw/a.bin"},
		{name: "rootFolder Tables without a table name",
			source: `{"type":"LakehouseTableSource","rootFolder":"Tables",` + loc + `}`, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dst := seedLakehouse(t, st, ws.ID, "sink-"+strings.ReplaceAll(tc.name, " ", "-"))
			if tc.seed != "" {
				seedFile(t, st, ws.ID, lh.ID, tc.seed, []byte("payload"))
			}
			content := `{"properties":{"activities":[
              {"name":"C","type":"Copy","typeProperties":{
                "source":` + tc.source + `,
                "sink":{"type":"BinarySink","path":"Files/out",
                  "datasetSettings":{"linkedService":{"properties":{"typeProperties":{"artifactId":"` + dst.ID + `"}}}}}
              }}]}}`
			pl := createPipeline(t, st, ws.ID, content)
			_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
			s := jobStatus(t, a, ws.ID, pl.ID, jid)
			if tc.fail {
				if s != "Failed" {
					t.Fatalf("status = %s, want Failed", s)
				}
				return
			}
			if s != "Completed" {
				t.Fatalf("status = %s, want Completed", s)
			}
			_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
			lineage := runs[0]["output"].(map[string]any)["lineage"].(map[string]any)
			if got := lineage["sourcePath"]; !strings.HasPrefix(tc.want, got.(string)) {
				t.Fatalf("sourcePath = %v, want a prefix of %q", got, tc.want)
			}
		})
	}
}

// TestCopyCsvIntoDeltaTableAppends: the medallion's landing->bronze hop as a
// real Copy — a CSV under Files/ committed into Tables/<name> as Delta, with
// Append accumulating across runs instead of clobbering.
func TestCopyCsvIntoDeltaTableAppends(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lh := seedLakehouse(t, st, ws.ID, "lake")
	seedFile(t, st, ws.ID, lh.ID, "Files/landing/day1.csv", []byte("id,name\n1,ada\n2,grace\n"))
	seedFile(t, st, ws.ID, lh.ID, "Files/landing/day2.csv", []byte("id,name\n3,alan\n"))
	loc := `"datasetSettings":{"linkedService":{"properties":{"typeProperties":{"artifactId":"` + lh.ID + `"}}}}`

	ingest := func(file, action string) {
		content := `{"properties":{"activities":[
          {"name":"Ingest","type":"Copy","typeProperties":{
            "source":{"type":"DelimitedTextSource","rootFolder":"Files","folderPath":"landing","fileName":"` + file + `",` + loc + `},
            "sink":{"type":"LakehouseTableSink","tableActionOption":"` + action + `","table":"bronze_orders",` + loc + `}
          }}]}}`
		pl := createPipeline(t, st, ws.ID, content)
		_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
		if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Completed" {
			t.Fatalf("%s %s: status = %s", file, action, s)
		}
		_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
		if _, ok := runs[0]["output"].(map[string]any)["rowsCopied"]; !ok {
			t.Fatalf("table copy should report rowsCopied: %+v", runs[0]["output"])
		}
	}

	ingest("day1.csv", "Overwrite")
	tbl, err := warehouse.ReadDeltaTable(st, lh.ID, "bronze_orders")
	if err != nil || len(tbl.Rows) != 2 {
		t.Fatalf("after first ingest: %v rows=%v", err, tbl)
	}

	ingest("day2.csv", "Append")
	tbl, err = warehouse.ReadDeltaTable(st, lh.ID, "bronze_orders")
	if err != nil {
		t.Fatal(err)
	}
	if len(tbl.Rows) != 3 {
		t.Fatalf("Append should accumulate: got %d rows %v", len(tbl.Rows), tbl.Rows)
	}

	// Overwrite really replaces, it does not pile up.
	ingest("day2.csv", "Overwrite")
	tbl, err = warehouse.ReadDeltaTable(st, lh.ID, "bronze_orders")
	if err != nil || len(tbl.Rows) != 1 {
		t.Fatalf("Overwrite should replace: %v rows=%v", err, tbl)
	}
}

// TestCopySinkActionDefaultsToOverwrite.
//
// The sink's tableActionOption decides whether a Copy appends or overwrites,
// and every way of NOT saying so has to mean overwrite. Three of those ways
// had never been exercised: no sink at all, a sink without the option, and an
// expression that resolves to nothing.
//
// Defaulting the wrong way is not a crash, it is a table that doubles on every
// run — which looks like a source problem for as long as it takes someone to
// count rows.
func TestCopySinkActionDefaultsToOverwrite(t *testing.T) {
	e := &pipelineExecutor{}
	keep := func(raw json.RawMessage) (any, error) { return string(raw), nil }

	for _, tc := range []struct {
		name string
		tp   map[string]json.RawMessage
		want string
	}{
		{"no sink at all", map[string]json.RawMessage{}, ""},
		{"a sink that is not an object",
			map[string]json.RawMessage{"sink": json.RawMessage(`"nonsense"`)}, ""},
		{"a sink with no tableActionOption",
			map[string]json.RawMessage{"sink": json.RawMessage(`{"other":1}`)}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := e.copySinkAction(tc.tp, keep)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("action = %q, want %q (empty means Overwrite)", got, tc.want)
			}
			if strings.EqualFold(got, "Append") {
				t.Error("an unstated action resolved to Append — a table that " +
					"doubles every run")
			}
		})
	}

	// A stated Append is still honoured, so the default is a default and not a
	// hard-coded answer.
	got, err := e.copySinkAction(map[string]json.RawMessage{
		"sink": json.RawMessage(`{"tableActionOption":"Append"}`)}, keep)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "Append") {
		t.Errorf("a stated Append resolved to %q", got)
	}

	// And a resolver that fails propagates, rather than silently overwriting.
	boom := func(json.RawMessage) (any, error) { return nil, fmt.Errorf("unresolved") }
	if _, err := e.copySinkAction(map[string]json.RawMessage{
		"sink": json.RawMessage(`{"tableActionOption":"@pipeline().x"}`)}, boom); err == nil {
		t.Error("an unresolvable tableActionOption was swallowed; a Copy would " +
			"then overwrite on a expression the author expected to append")
	}
}

// TestCopyIntoTableFailsOnAMalformedSourceRatherThanFallingBack.
//
// copyIntoTable has two ways of not producing a table, and they must not be
// confused. A source that is not a table at all — a directory, a format this
// does not parse — falls back to the opaque byte copy, "rather than failing a
// Copy that used to work". But a source that CLAIMS to be a table and cannot be
// parsed is an error: falling back there would write the raw bytes into
// Tables/<name>, leaving something that is not a Delta table where a reader
// expects one, and reporting success.
//
// The parse-failure arm had never executed.
func TestCopyIntoTableFailsOnAMalformedSourceRatherThanFallingBack(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lh := seedLakehouse(t, st, ws.ID, "lake")
	// A CSV whose rows disagree with its header — encoding/csv rejects it.
	seedFile(t, st, ws.ID, lh.ID, "Files/landing/bad.csv",
		[]byte("id,name\n1,ada,surplus\n"))
	loc := `"datasetSettings":{"linkedService":{"properties":{"typeProperties":{"artifactId":"` + lh.ID + `"}}}}`

	content := `{"properties":{"activities":[
      {"name":"Ingest","type":"Copy","typeProperties":{
        "source":{"type":"DelimitedTextSource","rootFolder":"Files","folderPath":"landing","fileName":"bad.csv",` + loc + `},
        "sink":{"type":"LakehouseTableSink","tableActionOption":"Overwrite","table":"bronze_bad",` + loc + `}
      }}]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")

	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("status = %s; a source that claims to be a table and will not "+
			"parse must FAIL, not fall back to copying raw bytes into Tables/", s)
	}
	// And nothing was written where a Delta table belongs.
	if _, err := warehouse.ReadDeltaTable(st, lh.ID, "bronze_bad"); err == nil {
		t.Error("a failed table copy left something readable at Tables/bronze_bad")
	}
}

// TestPipelineDeleteFile: a Delete activity really removes the file — the
// storage layer stops serving it, and the count is the activity's product.
func TestPipelineDeleteFile(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lh := seedLakehouse(t, st, ws.ID, "lake")
	seedFile(t, st, ws.ID, lh.ID, "Files/tmp/junk.csv", []byte("x\n1\n"))

	content := `{"properties":{"activities":[
        {"name":"Del","type":"Delete","typeProperties":{
          "location":{"itemId":"` + lh.ID + `","path":"Files/tmp/junk.csv"}}}
      ]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		t.Fatalf("job status = %s", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	if got := outputOf(runs, "Del")["filesDeleted"].(float64); got != 1 {
		t.Fatalf("filesDeleted = %v, want 1", got)
	}
	if _, err := st.GetOneLakePath(lh.ID, "Files/tmp/junk.csv"); err == nil {
		t.Fatal("file still exists after Delete — the activity reported work it did not do")
	}
}

// TestPipelineDeleteRecursiveVsFlat: recursive removes the subtree and counts
// every file; non-recursive on a directory removes only direct-child files and
// leaves subdirectories standing.
func TestPipelineDeleteRecursiveVsFlat(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lh := seedLakehouse(t, st, ws.ID, "lake")
	for _, f := range []string{"Files/d/a.txt", "Files/d/b.txt", "Files/d/sub/c.txt"} {
		seedFile(t, st, ws.ID, lh.ID, f, []byte("x"))
	}

	content := `{"properties":{"activities":[
        {"name":"Flat","type":"Delete","typeProperties":{
          "location":{"itemId":"` + lh.ID + `","path":"Files/d"}}}
      ]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		t.Fatalf("job status = %s", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	if got := outputOf(runs, "Flat")["filesDeleted"].(float64); got != 2 {
		t.Fatalf("flat filesDeleted = %v, want 2 (a.txt, b.txt — not sub/c.txt)", got)
	}
	if _, err := st.GetOneLakePath(lh.ID, "Files/d/sub/c.txt"); err != nil {
		t.Fatal("non-recursive Delete descended into a subdirectory")
	}

	content2 := `{"properties":{"activities":[
        {"name":"Rec","type":"Delete","typeProperties":{"recursive":true,
          "location":{"itemId":"` + lh.ID + `","path":"Files/d"}}}
      ]}}`
	pl2 := createPipeline(t, st, ws.ID, content2)
	_, jid2 := runJob(t, a, ws.ID, pl2.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl2.ID, jid2); s != "Completed" {
		t.Fatalf("job status = %s", s)
	}
	_, runs2 := activityRuns(t, a, ws.ID, pl2.ID, jid2)
	if got := outputOf(runs2, "Rec")["filesDeleted"].(float64); got != 1 {
		t.Fatalf("recursive filesDeleted = %v, want 1 (only sub/c.txt remained)", got)
	}
	if _, err := st.GetOneLakePath(lh.ID, "Files/d"); err == nil {
		t.Fatal("directory still exists after recursive Delete")
	}
}

// TestPipelineDeleteMissingFailsLoudly: a Delete against a path that does not
// exist is an error naming the path — a zero-count success against a typo'd
// path would claim work that never happened. (Held to the loud side; the real
// oracle is unmeasured, and the comment on deleteActivity says so.)
func TestPipelineDeleteMissingFailsLoudly(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lh := seedLakehouse(t, st, ws.ID, "lake")

	content := `{"properties":{"activities":[
        {"name":"Del","type":"Delete","typeProperties":{
          "location":{"itemId":"` + lh.ID + `","path":"Files/nope.csv"}}}
      ]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job status = %s, want Failed", s)
	}
}

// TestPipelineWebHookRefusesLoudly: WebHook used to silently alias Web, so a
// webhook pipeline "worked" locally while its defining half — call back and
// PARK — never executed. Until pipelines run async it is refused with the
// reason and the supported alternative, not aliased.
func TestPipelineWebHookRefusesLoudly(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	content := `{"properties":{"activities":[
        {"name":"Hook","type":"WebHook","typeProperties":{"url":"http://x/","method":"POST"}}
      ]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job status = %s, want Failed (loud refusal, not a silent Web alias)", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	for _, r := range runs {
		if r["activityName"] == "Hook" {
			if e, _ := r["error"].(string); !strings.Contains(e, "callback") {
				t.Fatalf("refusal must name the missing callback semantics, got: %q", e)
			}
		}
	}
}
