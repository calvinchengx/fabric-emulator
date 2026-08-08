package server_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// The portal's notebook view. docs/44 ranks it after the lakehouse browser;
// docs/14 D3 fixes its shape — the stored definition rendered, the documented
// job API triggered, no editor.

const nbSource = `# MARKDOWN ********************

# Bronze to silver

# PARAMETERS CELL ********************

P_date = "2026-01-01"

# CELL ********************

spark.sql("SELECT 1")
`

// setNotebook creates a Notebook item and gives it a definition the way
// fabric-cicd does — create first, definition second — so the no-definition
// window these tests assert on is the real one rather than a contrived state.
func (f *fixture) setNotebook(t *testing.T, wsID, name, content string) string {
	t.Helper()
	id := f.createItemNow(t, wsID, "Notebook", name)
	if content == "" {
		return id
	}
	err := f.srv.API.Store.SetDefinition(id, []store.DefinitionPart{{
		Path: "notebook-content.py", PayloadType: "InlineBase64",
		Payload: base64.StdEncoding.EncodeToString([]byte(content)),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// portalPost is portalJSON's mutating twin — the portal's run route is a POST,
// and it carries no token by design.
func (f *fixture) portalPost(t *testing.T, path string, out any) int {
	t.Helper()
	resp, err := http.Post(f.fabric.URL+path, "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
	}
	return resp.StatusCode
}

func (f *fixture) notebookWorkspace(t *testing.T, name string) string {
	t.Helper()
	var ws struct {
		ID string `json:"id"`
	}
	f.mustStatus(f.call("POST", "/v1/workspaces", f.token,
		map[string]any{"displayName": name}, &ws), 201, "create workspace")
	return ws.ID
}

type notebookList struct {
	Notebooks []struct {
		ItemID        string `json:"itemId"`
		Name          string `json:"name"`
		Workspace     string `json:"workspace"`
		Cells         int    `json:"cells"`
		CodeCells     int    `json:"codeCells"`
		HasDefinition bool   `json:"hasDefinition"`
	} `json:"notebooks"`
}

type notebookDetail struct {
	ItemID   string `json:"itemId"`
	Name     string `json:"name"`
	Readable bool   `json:"readable"`
	Message  string `json:"message"`
	RunsHere bool   `json:"runsHere"`
	Cells    []struct {
		Index      int    `json:"index"`
		Kind       string `json:"kind"`
		Source     string `json:"source"`
		Parameters bool   `json:"parameters"`
	} `json:"cells"`
}

func TestPortalNotebooksListsCellCounts(t *testing.T) {
	f := newFixture(t)
	ws := f.notebookWorkspace(t, "nb-ws")
	f.setNotebook(t, ws, "bronze", nbSource)

	var got notebookList
	if code := f.portalJSON(t, "/_emulator/portal/notebooks", &got); code != 200 {
		t.Fatalf("portal/notebooks = %d", code)
	}
	if len(got.Notebooks) != 1 {
		t.Fatalf("want 1 notebook, got %+v", got.Notebooks)
	}
	nb := got.Notebooks[0]
	if nb.Name != "bronze" || nb.Workspace != "nb-ws" {
		t.Fatalf("identity wrong: %+v", nb)
	}
	// Three cells parsed, two of them executable. Counting only code cells here
	// would under-report a notebook that is mostly prose; counting every cell as
	// code would over-promise what a run will execute.
	if nb.Cells != 3 || nb.CodeCells != 2 {
		t.Fatalf("cells=%d codeCells=%d, want 3 and 2", nb.Cells, nb.CodeCells)
	}
	if !nb.HasDefinition {
		t.Fatal("hasDefinition must be true for a notebook with a definition")
	}
}

func TestPortalNotebooksListsADefinitionlessNotebook(t *testing.T) {
	// Created-but-not-yet-defined is a real state: fabric-cicd creates the item
	// and updates the definition afterwards. Hiding it would make this list
	// disagree with the item list it is a view of — and "0 cells" alone cannot
	// distinguish it from an empty notebook, which is why the flag exists.
	f := newFixture(t)
	ws := f.notebookWorkspace(t, "empty-ws")
	f.setNotebook(t, ws, "fresh", "")

	var got notebookList
	f.portalJSON(t, "/_emulator/portal/notebooks", &got)
	if len(got.Notebooks) != 1 {
		t.Fatalf("a definition-less notebook must still list: %+v", got.Notebooks)
	}
	if got.Notebooks[0].HasDefinition {
		t.Fatal("hasDefinition must be false when no definition was ever set")
	}
}

func TestPortalNotebookDetailKeepsMarkdownCells(t *testing.T) {
	// parseNotebookRun drops markdown because an ENGINE executes only code.
	// A reader needs the prose that explains the code, so this view must not
	// inherit that filter — dropping it renders a notebook nobody wrote.
	f := newFixture(t)
	ws := f.notebookWorkspace(t, "md-ws")
	id := f.setNotebook(t, ws, "bronze", nbSource)

	var got notebookDetail
	if code := f.portalJSON(t, "/_emulator/portal/notebooks/"+id, &got); code != 200 {
		t.Fatalf("detail = %d", code)
	}
	if !got.Readable || len(got.Cells) != 3 {
		t.Fatalf("want 3 readable cells, got readable=%v %+v", got.Readable, got.Cells)
	}
	if got.Cells[0].Kind != "markdown" {
		t.Fatalf("first cell kind = %q, want markdown", got.Cells[0].Kind)
	}
	// The PARAMETERS cell is marked, not merely present: it is where a caller's
	// overrides land, and a view that renders it as an ordinary code cell hides
	// the one thing that makes it special.
	if !got.Cells[1].Parameters {
		t.Fatalf("the parameters cell must be marked: %+v", got.Cells[1])
	}
}

func TestPortalNotebookDetailSaysWhenThereIsNoDefinition(t *testing.T) {
	// Zero cells and no definition are different facts, and only one of them
	// can be run. Rendering an empty cell list for both would make a notebook
	// that cannot run look like one with nothing to do.
	f := newFixture(t)
	ws := f.notebookWorkspace(t, "nodef-ws")
	id := f.setNotebook(t, ws, "fresh", "")

	var got notebookDetail
	if code := f.portalJSON(t, "/_emulator/portal/notebooks/"+id, &got); code != 200 {
		t.Fatalf("detail = %d", code)
	}
	if got.Readable {
		t.Fatal("a notebook with no definition is not readable")
	}
	if got.Message == "" {
		t.Fatal("the reason must be stated, not left to an empty cell list")
	}
}

func TestPortalNotebookDetailRefusesANonNotebook(t *testing.T) {
	f := newFixture(t)
	ws := f.notebookWorkspace(t, "mixed-ws")
	lake := f.createItemNow(t, ws, "Lakehouse", "lake")

	var got notebookDetail
	if code := f.portalJSON(t, "/_emulator/portal/notebooks/"+lake, &got); code != http.StatusNotFound {
		t.Fatalf("a Lakehouse id must 404 on the notebook route, got %d", code)
	}
}

func TestPortalNotebookRunStartsTheDocumentedJob(t *testing.T) {
	// The portal must not grow a second execution path. This asserts the run
	// button produces a job the REST surface already lists — same job type,
	// same instance — rather than something only the portal understands.
	f := newFixture(t)
	ws := f.notebookWorkspace(t, "run-ws")
	id := f.setNotebook(t, ws, "bronze", nbSource)

	var started struct {
		JobID    string `json:"jobId"`
		Status   string `json:"status"`
		RunsHere bool   `json:"runsHere"`
	}
	if code := f.portalPost(t, "/_emulator/portal/notebooks/"+id+"/run", &started); code != http.StatusAccepted {
		t.Fatalf("run = %d, want 202", code)
	}
	if started.JobID == "" {
		t.Fatal("a run must return the job id it started")
	}

	// The SAME job, over the authenticated /v1 surface a real client uses.
	var job struct {
		JobType string `json:"jobType"`
		Status  string `json:"status"`
	}
	f.mustStatus(f.call("GET",
		"/v1/workspaces/"+ws+"/items/"+id+"/jobs/instances/"+started.JobID, f.token, nil, &job),
		200, "job instance over /v1")
	if job.JobType != "RunNotebook" {
		t.Fatalf("jobType = %q, want RunNotebook", job.JobType)
	}

	// AND it must not be green. With no Spark agent the job parks waiting for
	// an engine callback (startJob sets CompleteAt = MaxInt64 when cells are
	// outstanding). A run button that reported Completed here would be the
	// exact lie that rule exists to prevent.
	if started.RunsHere {
		t.Fatal("no spark agent is wired in this fixture; runsHere must be false")
	}
	if job.Status == "Completed" {
		t.Fatal("a notebook with cells and no engine must not complete on the clock")
	}
}

func TestPortalNotebookRunDetailReportsEachCell(t *testing.T) {
	// The job's own status is one word. The run record is what names which cell
	// is Pending — the difference between "something is happening" and knowing
	// where a run actually is.
	f := newFixture(t)
	ws := f.notebookWorkspace(t, "detail-ws")
	id := f.setNotebook(t, ws, "bronze", nbSource)

	var started struct {
		JobID string `json:"jobId"`
	}
	f.portalPost(t, "/_emulator/portal/notebooks/"+id+"/run", &started)

	var run struct {
		Status string `json:"status"`
		Cells  []struct {
			Index  int    `json:"index"`
			Status string `json:"status"`
			Source string `json:"source"`
		} `json:"cells"`
	}
	if code := f.portalJSON(t, "/_emulator/portal/notebooks/runs/"+started.JobID, &run); code != 200 {
		t.Fatalf("run detail = %d", code)
	}
	// The two CODE cells, re-sequenced 0..n — markdown leaves no gap, because
	// an engine iterates this list by index.
	if len(run.Cells) != 2 {
		t.Fatalf("want 2 code cells in the run record, got %+v", run.Cells)
	}
	if run.Cells[0].Status != "Pending" {
		t.Fatalf("cell 0 status = %q, want Pending with no engine", run.Cells[0].Status)
	}
}

func TestPortalNotebookRunRefusesAnUnknownItem(t *testing.T) {
	f := newFixture(t)
	var out map[string]any
	if code := f.portalPost(t, "/_emulator/portal/notebooks/no-such-id/run", &out); code != http.StatusNotFound {
		t.Fatalf("running an unknown notebook = %d, want 404", code)
	}
}

func TestPortalNotebookRunDetailUnknownJob(t *testing.T) {
	f := newFixture(t)
	var out map[string]any
	if code := f.portalJSON(t, "/_emulator/portal/notebooks/runs/no-such-job", &out); code != http.StatusNotFound {
		t.Fatalf("unknown run detail = %d, want 404", code)
	}
}
