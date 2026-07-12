package api

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

const sampleNotebook = `# Fabric notebook source

# CELL ********************
x = spark.range(3)

# MARKDOWN ********************
# MAGIC %md
# MAGIC ## done

# CELL ********************
# MAGIC %%sql
# MAGIC SELECT 1
`

func createNotebook(t *testing.T, st *store.Store, wid, content string) *store.Item {
	t.Helper()
	it := &store.Item{WorkspaceID: wid, Type: "Notebook", DisplayName: "nb"}
	parts := []store.DefinitionPart{{
		Path: "notebook-content.py", PayloadType: "InlineBase64",
		Payload: base64.StdEncoding.EncodeToString([]byte(content)),
	}}
	if err := st.CreateItem(it, parts); err != nil {
		t.Fatal(err)
	}
	return it
}

func notebookRunDetail(t *testing.T, a *API, wid, iid, jid string) notebookRun {
	t.Helper()
	w := do(a.getNotebookRun, admin, "GET", "", map[string]string{"wid": wid, "iid": iid, "jid": jid})
	if w.Code != 200 {
		t.Fatalf("getNotebookRun = %d %s", w.Code, w.Body.Bytes())
	}
	var run notebookRun
	if err := json.Unmarshal(w.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	return run
}

// TestNotebookRunParseAndReport: creating a RunNotebook job parses the notebook
// into code cells (Pending); an engine reports results, finalising the run and
// the job to Completed with the exit value.
func TestNotebookRunParseAndReport(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	nb := createNotebook(t, st, ws.ID, sampleNotebook)

	_, jid := runJob(t, a, ws.ID, nb.ID, "jobType=RunNotebook", "")

	// The emulator parsed the notebook: two code cells, both Pending, markdown dropped.
	run := notebookRunDetail(t, a, ws.ID, nb.ID, jid)
	if run.Status != "Pending" || len(run.Cells) != 2 {
		t.Fatalf("parsed run = %+v", run)
	}
	if run.Cells[0].Language != "python" || run.Cells[1].Language != "sql" {
		t.Fatalf("cell languages: %+v", run.Cells)
	}
	if run.Cells[0].Status != "Pending" {
		t.Fatalf("cell should be Pending pre-execution: %+v", run.Cells[0])
	}

	// An engine reports real results.
	result := `{"exitValue":"3","cells":[
      {"index":0,"status":"Succeeded","output":"DataFrame[id: bigint]"},
      {"index":1,"status":"Succeeded","output":"1"}]}`
	w := do(a.reportNotebookRun, admin, "POST", result, map[string]string{"wid": ws.ID, "iid": nb.ID, "jid": jid})
	if w.Code != 200 {
		t.Fatalf("report = %d %s", w.Code, w.Body.Bytes())
	}

	// The job is now really Completed (not clock-derived) with the run detail.
	if s := jobStatus(t, a, ws.ID, nb.ID, jid); s != "Completed" {
		t.Fatalf("job status = %s", s)
	}
	run = notebookRunDetail(t, a, ws.ID, nb.ID, jid)
	if run.Status != "Completed" || run.ExitValue != "3" {
		t.Fatalf("final run = %+v", run)
	}
	if run.Cells[1].Output != "1" || run.Cells[0].Status != "Succeeded" {
		t.Fatalf("cell results not merged: %+v", run.Cells)
	}
}

// TestNotebookRunFailure: a failed cell fails the run and the job.
func TestNotebookRunFailure(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	nb := createNotebook(t, st, ws.ID, sampleNotebook)
	_, jid := runJob(t, a, ws.ID, nb.ID, "jobType=RunNotebook", "")

	result := `{"cells":[{"index":0,"status":"Failed","error":"NameError: spark not defined"}]}`
	if w := do(a.reportNotebookRun, admin, "POST", result, map[string]string{"wid": ws.ID, "iid": nb.ID, "jid": jid}); w.Code != 200 {
		t.Fatalf("report = %d %s", w.Code, w.Body.Bytes())
	}
	if s := jobStatus(t, a, ws.ID, nb.ID, jid); s != "Failed" {
		t.Fatalf("job status = %s; want Failed", s)
	}
	run := notebookRunDetail(t, a, ws.ID, nb.ID, jid)
	if run.Status != "Failed" || run.Cells[0].Error == "" {
		t.Fatalf("run = %+v", run)
	}
}

// TestNotebookRunRBACAndScope: viewer reads but cannot report; non-notebook and
// unknown jobs 404.
func TestNotebookRunRBACAndScope(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	nb := createNotebook(t, st, ws.ID, sampleNotebook)
	_, jid := runJob(t, a, ws.ID, nb.ID, "jobType=RunNotebook", "")

	if w := do(a.getNotebookRun, viewer, "GET", "", map[string]string{"wid": ws.ID, "iid": nb.ID, "jid": jid}); w.Code != 200 {
		t.Fatalf("viewer read = %d", w.Code)
	}
	if w := do(a.reportNotebookRun, viewer, "POST", "{}", map[string]string{"wid": ws.ID, "iid": nb.ID, "jid": jid}); w.Code != 403 {
		t.Fatalf("viewer report = %d; want 403", w.Code)
	}
	// A generic (non-RunNotebook) job has no run detail.
	other := &store.Item{WorkspaceID: ws.ID, Type: "Notebook", DisplayName: "nb2"}
	if err := st.CreateItem(other, nil); err != nil {
		t.Fatal(err)
	}
	_, jid2 := runJob(t, a, ws.ID, other.ID, "jobType=Something", "")
	if w := do(a.getNotebookRun, admin, "GET", "", map[string]string{"wid": ws.ID, "iid": other.ID, "jid": jid2}); w.Code != 404 {
		t.Fatalf("non-notebook run = %d; want 404", w.Code)
	}
}

// TestNotebookRunNoDefinition: a Notebook with no content still records an
// (empty) run rather than crashing.
func TestNotebookRunNoDefinition(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	nb := &store.Item{WorkspaceID: ws.ID, Type: "Notebook", DisplayName: "empty"}
	if err := st.CreateItem(nb, nil); err != nil {
		t.Fatal(err)
	}
	_, jid := runJob(t, a, ws.ID, nb.ID, "jobType=RunNotebook", "")
	run := notebookRunDetail(t, a, ws.ID, nb.ID, jid)
	if run.Status != "Pending" || len(run.Cells) != 0 {
		t.Fatalf("empty notebook run = %+v", run)
	}
}
