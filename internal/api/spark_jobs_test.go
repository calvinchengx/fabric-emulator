package api

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

func sparkPart(path, body string) store.DefinitionPart {
	return store.DefinitionPart{Path: path, PayloadType: "InlineBase64", Payload: base64.StdEncoding.EncodeToString([]byte(body))}
}

func TestSparkJobDefinitionRunAndReport(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lake := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lake"}
	if err := st.CreateItem(lake, nil); err != nil {
		t.Fatal(err)
	}
	item := &store.Item{WorkspaceID: ws.ID, Type: "SparkJobDefinition", DisplayName: "job"}
	config := `{"executableFile":"main.py","arguments":["--n","3"],"defaultLakehouseArtifactId":"` + lake.ID + `","defaultLakehouseWorkspaceId":"` + ws.ID + `"}`
	if err := st.CreateItem(item, []store.DefinitionPart{sparkPart("SparkJobDefinitionV1.json", config), sparkPart("main.py", "print('done')")}); err != nil {
		t.Fatal(err)
	}
	_, jid := runJob(t, a, ws.ID, item.ID, "jobType=sparkjob", "")
	pv := map[string]string{"wid": ws.ID, "iid": item.ID, "jid": jid}
	w := do(a.getSparkJobRun, viewer, "GET", "", pv)
	if w.Code != 200 {
		t.Fatalf("get = %d %s", w.Code, w.Body.Bytes())
	}
	var run sparkJobRun
	if json.Unmarshal(w.Body.Bytes(), &run) != nil || run.Job.Source != "print('done')" || run.Binding.LakehouseName != "lake" {
		t.Fatalf("run = %+v", run)
	}
	if w := do(a.reportSparkJobRun, viewer, "POST", `{"status":"Completed"}`, pv); w.Code != 403 {
		t.Fatalf("viewer report = %d", w.Code)
	}
	if w := do(a.reportSparkJobRun, admin, "POST", `{"status":"nope"}`, pv); w.Code != 400 {
		t.Fatalf("bad result = %d", w.Code)
	}
	if w := do(a.reportSparkJobRun, admin, "POST", `{"status":"Completed","output":"done"}`, pv); w.Code != 200 {
		t.Fatalf("report = %d %s", w.Code, w.Body.Bytes())
	}
	if got := awaitJob(t, a, ws.ID, item.ID, jid); got != store.JobCompleted {
		t.Fatalf("status = %s", got)
	}
}

func TestSparkJobDefinitionValidationAndFailure(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	bad := &store.Item{WorkspaceID: ws.ID, Type: "SparkJobDefinition", DisplayName: "bad"}
	if err := st.CreateItem(bad, []store.DefinitionPart{sparkPart("SparkJobDefinitionV1.json", `{"executableFile":"missing.py"}`)}); err != nil {
		t.Fatal(err)
	}
	_, jid := runJob(t, a, ws.ID, bad.ID, "jobType=sparkjob", "")
	if got := awaitJob(t, a, ws.ID, bad.ID, jid); got != store.JobFailed {
		t.Fatalf("invalid status = %s", got)
	}

	good := &store.Item{WorkspaceID: ws.ID, Type: "SparkJobDefinition", DisplayName: "fails"}
	if err := st.CreateItem(good, []store.DefinitionPart{sparkPart("SparkJobDefinitionV1.json", `{"executableFile":"main.py"}`), sparkPart("main.py", "raise RuntimeError()")}); err != nil {
		t.Fatal(err)
	}
	_, jid = runJob(t, a, ws.ID, good.ID, "jobType=sparkjob", "")
	pv := map[string]string{"wid": ws.ID, "iid": good.ID, "jid": jid}
	if w := do(a.reportSparkJobRun, admin, "POST", `{"status":"Failed","error":"boom"}`, pv); w.Code != 200 {
		t.Fatalf("report failure = %d", w.Code)
	}
	if got := awaitJob(t, a, ws.ID, good.ID, jid); got != store.JobFailed {
		t.Fatalf("failed status = %s", got)
	}
}

// TestSparkJobStatusReflectsExecutionNotTheClock: the same guarantee notebooks
// get, for the item type with the same shape.
//
// A SparkJobDefinition job used to report "Completed" from virtual time alone,
// with its run still Pending and no engine having touched the main file. Unlike
// a notebook there is no empty case to carve out — a definition that parses
// always names an executable file — so a parsed job is always waiting on an
// engine, and only the engine's callback may finish it.
func TestSparkJobStatusReflectsExecutionNotTheClock(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lake := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lake"}
	if err := st.CreateItem(lake, nil); err != nil {
		t.Fatal(err)
	}
	item := &store.Item{WorkspaceID: ws.ID, Type: "SparkJobDefinition", DisplayName: "job"}
	config := `{"executableFile":"main.py","defaultLakehouseArtifactId":"` + lake.ID +
		`","defaultLakehouseWorkspaceId":"` + ws.ID + `"}`
	if err := st.CreateItem(item, []store.DefinitionPart{
		sparkPart("SparkJobDefinitionV1.json", config),
		sparkPart("main.py", "print('work')"),
	}); err != nil {
		t.Fatal(err)
	}

	_, jid := runJob(t, a, ws.ID, item.ID, "jobType=sparkjob", "")
	if s := jobStatus(t, a, ws.ID, item.ID, jid); s == store.JobCompleted || s == store.JobFailed {
		t.Fatalf("job reported %q before any engine ran main.py", s)
	}

	pv := map[string]string{"wid": ws.ID, "iid": item.ID, "jid": jid}
	if w := do(a.reportSparkJobRun, admin, "POST", `{"status":"Completed","output":"work"}`, pv); w.Code != 200 {
		t.Fatalf("report = %d %s", w.Code, w.Body.Bytes())
	}
	if s := awaitJob(t, a, ws.ID, item.ID, jid); s != store.JobCompleted {
		t.Fatalf("job status after a real report = %q, want Completed", s)
	}
}

// TestSparkJobThatCannotParseFailsWithoutWaiting: the deferral must not strand a
// job that will never have an engine. A definition that cannot be parsed has no
// executable file, so nothing will ever report on it — it fails now.
func TestSparkJobThatCannotParseFailsWithoutWaiting(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	item := &store.Item{WorkspaceID: ws.ID, Type: "SparkJobDefinition", DisplayName: "broken"}
	if err := st.CreateItem(item, []store.DefinitionPart{
		sparkPart("SparkJobDefinitionV1.json", `{"executableFile":""}`),
	}); err != nil {
		t.Fatal(err)
	}
	_, jid := runJob(t, a, ws.ID, item.ID, "jobType=sparkjob", "")
	if s := awaitJob(t, a, ws.ID, item.ID, jid); s != store.JobFailed {
		t.Fatalf("unparseable job = %q, want Failed — no engine will ever "+
			"report on it, so waiting would hang forever", s)
	}
}
