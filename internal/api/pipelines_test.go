package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// createPipeline seeds a DataPipeline item whose definition is the given
// pipeline-content.json.
func createPipeline(t *testing.T, st *store.Store, wid, contentJSON string) *store.Item {
	t.Helper()
	payload := base64.StdEncoding.EncodeToString([]byte(contentJSON))
	it := &store.Item{WorkspaceID: wid, Type: "DataPipeline", DisplayName: "pl"}
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
	if err := st.CreateItem(nb, nil); err != nil {
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
	// The directory row (IsDir) is what makes the copy recurse the subtree.
	if err := st.CreateOneLakePath(&store.OneLakePath{WorkspaceID: ws.ID, ItemID: src.ID, RelPath: "Files/in", IsDir: true, Content: []byte{}}, false); err != nil {
		t.Fatal(err)
	}

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
