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

	// The job is now really Completed *because of the report* — this assertion
	// used to pass without any report at all, since the clock completed the job
	// on its own. It is load-bearing only because a job with cells outstanding
	// no longer has a clock-derived completion; see
	// TestNotebookJobStatusReflectsExecutionNotTheClock.
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

// TestNotebookRunNoDefinition: a Notebook with no content FAILS the job.
//
// It used to complete. A job whose item was never given a definition reported
// `Completed`, with no cells parsed, no engine asked and nothing posted to
// notebookRunResult — indistinguishable from a notebook that ran. That is the
// lie ce219af removed for cells-outstanding, with the count at zero.
// DataPipeline has always failed here (TestPipelineNoDefinition).
func TestNotebookRunNoDefinition(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	nb := &store.Item{WorkspaceID: ws.ID, Type: "Notebook", DisplayName: "empty"}
	if err := st.CreateItem(nb, nil); err != nil {
		t.Fatal(err)
	}
	_, jid := runJob(t, a, ws.ID, nb.ID, "jobType=RunNotebook", "")
	if s := jobStatus(t, a, ws.ID, nb.ID, jid); s != "Failed" {
		t.Fatalf("job status = %s, want Failed", s)
	}
	// Terminal, so notebookutils.notebook.run still polls to an end — and the
	// detail agrees with the job rather than contradicting it.
	run := notebookRunDetail(t, a, ws.ID, nb.ID, jid)
	if run.Status != "Failed" || len(run.Cells) != 0 {
		t.Fatalf("empty notebook run = %+v", run)
	}
}

// TestNotebookRunDetailNeverContradictsItsJob.
//
// A definition that parses to nothing executable is a real run with no work in
// it, and the clock completes the job. The run detail must say so too: a job
// reading `Completed` while its own detail reads `Pending` is two views of one
// execution disagreeing, and whichever a caller happens to read is the one that
// misleads them. This is how a notebook that never ran looked healthy in the
// portal while the endpoint that would have shown the truth said nothing.
func TestNotebookRunDetailNeverContradictsItsJob(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	nb := &store.Item{WorkspaceID: ws.ID, Type: "Notebook", DisplayName: "markdown-only"}
	// A definition with no code in it at all.
	body := "# Fabric notebook source\n\n# MARKDOWN ********************\n# MAGIC ## nothing to run\n"
	if err := st.CreateItem(nb, []store.DefinitionPart{{
		Path: "notebook-content.py", Payload: base64.StdEncoding.EncodeToString([]byte(body)),
	}}); err != nil {
		t.Fatal(err)
	}
	_, jid := runJob(t, a, ws.ID, nb.ID, "jobType=RunNotebook", "")
	status := jobStatus(t, a, ws.ID, nb.ID, jid)
	run := notebookRunDetail(t, a, ws.ID, nb.ID, jid)
	if len(run.Cells) != 0 {
		t.Fatalf("markdown-only notebook parsed %d code cell(s)", len(run.Cells))
	}
	if status != run.Status {
		t.Fatalf("job says %q, its run detail says %q", status, run.Status)
	}
	// Equality alone would pass if BOTH said Pending, which is the bug wearing
	// a different face: the pair must agree AND be terminal.
	if status != "Completed" {
		t.Fatalf("a run with nothing to execute = %s, want Completed", status)
	}
}

func TestNotebookRunResolvesLakehouseAndEnvironment(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	envWS := &store.Workspace{DisplayName: "environment-workspace"}
	if err := st.CreateWorkspace(envWS, store.Principal{ID: admin.ID, Type: admin.Type}); err != nil {
		t.Fatal(err)
	}
	lake := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "sales"}
	if err := st.CreateItem(lake, nil); err != nil {
		t.Fatal(err)
	}
	env := &store.Item{WorkspaceID: envWS.ID, Type: "Environment", DisplayName: "runtime"}
	if err := st.CreateItem(env, []store.DefinitionPart{
		{Path: "requirements.txt", PayloadType: "InlineBase64", Payload: base64.StdEncoding.EncodeToString([]byte("pandas==2.2.3\n"))},
		{Path: "Spark.json", PayloadType: "InlineBase64", Payload: base64.StdEncoding.EncodeToString([]byte(`{"sparkProperties":{"spark.sql.shuffle.partitions":4}}`))},
	}); err != nil {
		t.Fatal(err)
	}
	source := `# Fabric notebook source
# METADATA ********************
# META {
# META   "dependencies": {
# META     "lakehouse": {"default_lakehouse":"` + lake.ID + `", "default_lakehouse_workspace_id":"` + ws.ID + `"},
# META     "environment": {"environmentId":"` + env.ID + `", "workspaceId":"` + envWS.ID + `"}
# META   }
# META }
# CELL ********************
spark.sql("select 1")
`
	nb := createNotebook(t, st, ws.ID, source)
	_, jid := runJob(t, a, ws.ID, nb.ID, "jobType=RunNotebook", "")
	run := notebookRunDetail(t, a, ws.ID, nb.ID, jid)
	if run.Binding.LakehouseID != lake.ID || run.Binding.LakehouseName != "sales" || run.Binding.EnvironmentID != env.ID {
		t.Fatalf("binding = %+v", run.Binding)
	}
	if run.Binding.EnvironmentWorkspaceID != envWS.ID {
		t.Fatalf("environment workspace = %+v", run.Binding)
	}
	if len(run.Environment.PythonPackages) != 1 || run.Environment.SparkConfig["spark.sql.shuffle.partitions"] != "4" {
		t.Fatalf("environment = %+v", run.Environment)
	}
}

func TestNotebookRunRejectsMissingBinding(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	source := `# META {"dependencies":{"lakehouse":{"default_lakehouse":"missing"}}}
# CELL ********************
print(1)`
	nb := createNotebook(t, st, ws.ID, source)
	_, jid := runJob(t, a, ws.ID, nb.ID, "jobType=RunNotebook", "")
	if got := jobStatus(t, a, ws.ID, nb.ID, jid); got != store.JobFailed {
		t.Fatalf("status = %s", got)
	}
	if run := notebookRunDetail(t, a, ws.ID, nb.ID, jid); run.Status != "Failed" {
		t.Fatalf("run = %+v", run)
	}
}

// edgesFor lists the lineage edges a workspace holds.
func edgesFor(t *testing.T, a *API, wid string) []store.LineageEdge {
	t.Helper()
	w := do(a.listLineage, admin, "GET", "", map[string]string{"wid": wid})
	if w.Code != 200 {
		t.Fatalf("lineage = %d %s", w.Code, w.Body.Bytes())
	}
	var page struct {
		Value []store.LineageEdge `json:"value"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	return page.Value
}

// TestNotebookLineageFromEngineReport: the engine reports what each cell read
// and wrote, and the emulator records exact edges — never parsed from code.
// A cell reading two tables into one records both inputs (fan-in).
func TestNotebookLineageFromEngineReport(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	nb := createNotebook(t, st, ws.ID, sampleNotebook)
	lake := seedLakehouse(t, st, ws.ID, "lake")
	_, jid := runJob(t, a, ws.ID, nb.ID, "jobType=RunNotebook", "")

	result := `{"exitValue":"ok","cells":[
      {"index":0,"status":"Succeeded",
       "reads":[{"itemId":"` + lake.ID + `","path":"Tables/bronze_orders"},
                {"itemId":"` + lake.ID + `","path":"Tables/bronze_customers"}],
       "writes":[{"itemId":"` + lake.ID + `","path":"Tables/silver_orders"}]},
      {"index":1,"status":"Succeeded",
       "reads":[{"itemId":"` + lake.ID + `","path":"Tables/silver_orders"}]}]}`
	if w := do(a.reportNotebookRun, admin, "POST", result, map[string]string{"wid": ws.ID, "iid": nb.ID, "jid": jid}); w.Code != 200 {
		t.Fatalf("report = %d %s", w.Code, w.Body.Bytes())
	}

	edges := edgesFor(t, a, ws.ID)
	if len(edges) != 2 {
		t.Fatalf("want 2 fan-in edges (cell 1 reads only, so no edge), got %d: %+v", len(edges), edges)
	}
	srcs := map[string]bool{}
	for _, e := range edges {
		if e.Producer != store.ProducerNotebook {
			t.Fatalf("producer = %q, want %q", e.Producer, store.ProducerNotebook)
		}
		if e.ActivityName != "cell[0]" {
			t.Fatalf("activity = %q, want cell[0]", e.ActivityName)
		}
		if e.TargetPath != "Tables/silver_orders" || e.JobID != jid {
			t.Fatalf("edge = %+v", e)
		}
		srcs[e.SourcePath] = true
	}
	if !srcs["Tables/bronze_orders"] || !srcs["Tables/bronze_customers"] {
		t.Fatalf("both inputs should be recorded: %+v", srcs)
	}
}

// TestNotebookLineageSkipsIncompleteAndReadOnly: a read with no write moves
// nothing, and a half-specified reference is not an exact fact — neither
// becomes an edge.
func TestNotebookLineageSkipsIncompleteAndReadOnly(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	nb := createNotebook(t, st, ws.ID, sampleNotebook)
	lake := seedLakehouse(t, st, ws.ID, "lake")
	_, jid := runJob(t, a, ws.ID, nb.ID, "jobType=RunNotebook", "")

	result := `{"cells":[
      {"index":0,"status":"Succeeded","reads":[{"itemId":"` + lake.ID + `","path":"Tables/a"}]},
      {"index":1,"status":"Succeeded","writes":[{"itemId":"` + lake.ID + `","path":"Tables/b"}]},
      {"index":2,"status":"Succeeded",
       "reads":[{"itemId":"","path":"Tables/a"}],
       "writes":[{"itemId":"` + lake.ID + `","path":"Tables/c"}]}]}`
	if w := do(a.reportNotebookRun, admin, "POST", result, map[string]string{"wid": ws.ID, "iid": nb.ID, "jid": jid}); w.Code != 200 {
		t.Fatalf("report = %d %s", w.Code, w.Body.Bytes())
	}
	if edges := edgesFor(t, a, ws.ID); len(edges) != 0 {
		t.Fatalf("no edge should be recorded, got %+v", edges)
	}
}

// TestNotebookLineagePerCellKey: the same (source,target) pair touched by two
// cells stays two edges — the per-cell activity name is what keeps the store's
// unique key from collapsing them.
func TestNotebookLineagePerCellKey(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	nb := createNotebook(t, st, ws.ID, sampleNotebook)
	lake := seedLakehouse(t, st, ws.ID, "lake")
	_, jid := runJob(t, a, ws.ID, nb.ID, "jobType=RunNotebook", "")

	pair := `"reads":[{"itemId":"` + lake.ID + `","path":"Tables/a"}],"writes":[{"itemId":"` + lake.ID + `","path":"Tables/b"}]`
	result := `{"cells":[
      {"index":0,"status":"Succeeded",` + pair + `},
      {"index":1,"status":"Succeeded",` + pair + `}]}`
	if w := do(a.reportNotebookRun, admin, "POST", result, map[string]string{"wid": ws.ID, "iid": nb.ID, "jid": jid}); w.Code != 200 {
		t.Fatalf("report = %d", w.Code)
	}
	edges := edgesFor(t, a, ws.ID)
	if len(edges) != 2 {
		t.Fatalf("per-cell naming should keep both, got %d: %+v", len(edges), edges)
	}
	if edges[0].ActivityName == edges[1].ActivityName {
		t.Fatalf("activity names should differ: %+v", edges)
	}
}

// TestCopyLineageKeepsProducerCopy: existing Copy edges keep the default
// producer, so a catalog can still tell the two mechanisms apart.
func TestCopyLineageKeepsProducerCopy(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	src := seedLakehouse(t, st, ws.ID, "src")
	dst := seedLakehouse(t, st, ws.ID, "dst")
	seedFile(t, st, ws.ID, src.ID, "Files/in.csv", []byte("a\n1\n"))
	content := `{"properties":{"activities":[
      {"name":"Move","type":"Copy","typeProperties":{
        "source":{"location":{"itemId":"` + src.ID + `","path":"Files/in.csv"}},
        "sink":{"location":{"itemId":"` + dst.ID + `","path":"Files/out.csv"}}}}]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		t.Fatalf("status = %s", s)
	}
	edges := edgesFor(t, a, ws.ID)
	if len(edges) != 1 || edges[0].Producer != store.ProducerCopy {
		t.Fatalf("copy edge producer = %+v", edges)
	}
}

// TestNotebookObservedLineage: the emulator's own data plane saw the I/O and
// attributed it to the cell that made it — evidence, not a self-report. Reads
// and writes pair WITHIN a cell, so a notebook whose cells touch different
// tables does not produce a cross-product across the whole run.
func TestNotebookObservedLineage(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	nb := createNotebook(t, st, ws.ID, sampleNotebook)
	lake := seedLakehouse(t, st, ws.ID, "lake")
	_, jid := runJob(t, a, ws.ID, nb.ID, "jobType=RunNotebook", "")

	// What the storage layer observed: cell 0 bronze->silver, cell 1 silver->gold.
	for _, ac := range []store.NotebookAccess{
		{JobID: jid, CellIndex: 0, ItemID: lake.ID, Path: "Tables/bronze", Direction: store.AccessRead},
		{JobID: jid, CellIndex: 0, ItemID: lake.ID, Path: "Tables/silver", Direction: store.AccessWrite},
		{JobID: jid, CellIndex: 1, ItemID: lake.ID, Path: "Tables/silver", Direction: store.AccessRead},
		{JobID: jid, CellIndex: 1, ItemID: lake.ID, Path: "Tables/gold", Direction: store.AccessWrite},
	} {
		if err := st.RecordNotebookAccess(&ac); err != nil {
			t.Fatal(err)
		}
	}
	if w := do(a.reportNotebookRun, admin, "POST", `{"cells":[]}`, map[string]string{"wid": ws.ID, "iid": nb.ID, "jid": jid}); w.Code != 200 {
		t.Fatalf("report = %d %s", w.Code, w.Body.Bytes())
	}

	edges := edgesFor(t, a, ws.ID)
	if len(edges) != 2 {
		t.Fatalf("want exactly 2 per-cell edges (not a 2x2 cross-product), got %d: %+v", len(edges), edges)
	}
	seen := map[string]string{}
	for _, e := range edges {
		if e.Producer != store.ProducerNotebookObserved {
			t.Fatalf("producer = %q, want %q", e.Producer, store.ProducerNotebookObserved)
		}
		seen[e.SourcePath] = e.TargetPath
	}
	if seen["Tables/bronze"] != "Tables/silver" || seen["Tables/silver"] != "Tables/gold" {
		t.Fatalf("edges did not follow the cells: %+v", seen)
	}
}

// TestObservedLineageIgnoresSelfEdge: reading a table to rewrite it in place is
// not an edge to itself.
func TestObservedLineageIgnoresSelfEdge(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	nb := createNotebook(t, st, ws.ID, sampleNotebook)
	lake := seedLakehouse(t, st, ws.ID, "lake")
	_, jid := runJob(t, a, ws.ID, nb.ID, "jobType=RunNotebook", "")
	for _, d := range []string{store.AccessRead, store.AccessWrite} {
		if err := st.RecordNotebookAccess(&store.NotebookAccess{
			JobID: jid, CellIndex: 0, ItemID: lake.ID, Path: "Tables/t", Direction: d}); err != nil {
			t.Fatal(err)
		}
	}
	if w := do(a.reportNotebookRun, admin, "POST", `{"cells":[]}`, map[string]string{"wid": ws.ID, "iid": nb.ID, "jid": jid}); w.Code != 200 {
		t.Fatalf("report = %d", w.Code)
	}
	if edges := edgesFor(t, a, ws.ID); len(edges) != 0 {
		t.Fatalf("self edge should not be recorded: %+v", edges)
	}
}

// TestNotebookJobStatusReflectsExecutionNotTheClock: a RunNotebook job with
// cells outstanding must NOT report a terminal status until an engine has
// actually executed them.
//
// This is the defect it guards. A job's status was derived purely from virtual
// time, so a notebook job read "Completed" the instant its completion time
// passed — every cell still Pending, no engine having run a line. Callers
// reasonably read a completed job as "the notebook ran", and were wrong.
//
// The fix is not to make the clock slower; it is to take the clock off this
// job entirely. Only the engine's callback can finish it.
func TestNotebookJobStatusReflectsExecutionNotTheClock(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	nb := createNotebook(t, st, ws.ID, sampleNotebook)

	_, jid := runJob(t, a, ws.ID, nb.ID, "jobType=RunNotebook", "")

	// Nothing has executed, so nothing terminal may be claimed.
	if s := jobStatus(t, a, ws.ID, nb.ID, jid); s == "Completed" || s == "Failed" {
		t.Fatalf("job reported %q with every cell Pending and no engine", s)
	}
	run := notebookRunDetail(t, a, ws.ID, nb.ID, jid)
	if run.Status != "Pending" || len(run.Cells) == 0 {
		t.Fatalf("run should be Pending with cells parsed: %+v", run)
	}

	// The engine reports: NOW the job is terminal, and terminal because of the
	// report rather than because time passed.
	result := `{"exitValue":"3","cells":[
      {"index":0,"status":"Succeeded","output":"ok"},
      {"index":1,"status":"Succeeded","output":"1"}]}`
	w := do(a.reportNotebookRun, admin, "POST", result, map[string]string{"wid": ws.ID, "iid": nb.ID, "jid": jid})
	if w.Code != 200 {
		t.Fatalf("report = %d %s", w.Code, w.Body.Bytes())
	}
	if s := jobStatus(t, a, ws.ID, nb.ID, jid); s != "Completed" {
		t.Fatalf("job status after a real report = %q, want Completed", s)
	}
}

// TestNotebookJobWithNothingToExecuteStillCompletes: the fix must not strand a
// notebook that has no cells at all.
//
// A run with nothing executable is not waiting on an engine — there is no work
// outstanding — so it completes now. This is the boundary of the rule above,
// and it is load-bearing: `notebookutils.notebook.run` polls to a terminal
// status, and a definition-less notebook must still reach one rather than hang
// until the caller's timeout.
func TestNotebookJobWithNothingToExecuteStillCompletes(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	nb := &store.Item{WorkspaceID: ws.ID, Type: "Notebook", DisplayName: "empty"}
	// A definition that PARSES to nothing executable. The no-definition case is
	// a different thing and now fails (TestNotebookRunNoDefinition): an item
	// nobody gave content to has not "run with nothing to do", it was never
	// runnable. This is the genuine empty case, and it still must not hang.
	if err := st.CreateItem(nb, notebookParts("# Fabric notebook source\n\n# MARKDOWN ********************\n# MAGIC ## no code\n")); err != nil {
		t.Fatal(err)
	}
	_, jid := runJob(t, a, ws.ID, nb.ID, "jobType=RunNotebook", "")

	run := notebookRunDetail(t, a, ws.ID, nb.ID, jid)
	if len(run.Cells) != 0 {
		t.Fatalf("expected no executable cells, got %+v", run.Cells)
	}
	if s := jobStatus(t, a, ws.ID, nb.ID, jid); s != "Completed" {
		t.Fatalf("job with no cells = %q, want Completed — there is nothing "+
			"for an engine to do, so waiting for one would hang forever", s)
	}
}
