package api

// A notebook that binds an Environment must have it APPLIED, not merely
// resolved.
//
// WHY THIS TEST EXISTS. `resolveComputeBinding` is shared by notebooks and
// Spark Job Definitions, and both stored the resolved Environment on their run.
// Only the job-definition driver and the Livy session ever handed it to the
// agent. A notebook naming an Environment in its `# META` dependencies
// therefore ran with none of its packages and died on the first import, while
// the run detail happily reported the environment it had ignored.
//
// The e2e is the real witness (it imports a package the image lacks, through a
// RunNotebook job). This is the fast one, and it fails the specific way the
// regression did: no `/environment` post before the first statement.

import (
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/compute"
)

func TestANotebookRunAppliesItsBoundEnvironment(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	env := seedEnvironment(t, st, ws.ID, "team-env", "cowsay\n")
	stub := newAgentStub(t, a)
	stub.answer(map[string]any{"applied": true}, 0)

	run := notebookRun{
		Binding: compute.Binding{
			WorkspaceID:   ws.ID,
			EnvironmentID: env.ID,
		},
		Cells: []notebookCellRun{
			{Index: 0, Kind: "code", Language: "python", Source: "import cowsay", Status: "Pending"},
		},
	}
	a.driveNotebookRun(ws.ID, "item-1", "job-1", run, nil, false, referenceRoot{})

	var envPost *agentPostRecord
	firstStatement := -1
	for i, rec := range stub.recorded() {
		if rec.path == "/environment" && envPost == nil {
			r := rec
			envPost = &r
		}
		if rec.path == "/statements" && firstStatement < 0 {
			firstStatement = i
		}
	}
	if envPost == nil {
		t.Fatal("the notebook run never applied its Environment: no /environment post")
	}
	if envPost.body["environment"] != env.ID {
		t.Fatalf("applied environment %v, want %s", envPost.body["environment"], env.ID)
	}
	// BEFORE the first statement, which is the whole point: a package installed
	// after the cell that imports it has not been supplied.
	envIndex := -1
	for i, rec := range stub.recorded() {
		if rec.path == "/environment" {
			envIndex = i
			break
		}
	}
	if firstStatement >= 0 && envIndex > firstStatement {
		t.Fatalf("the Environment was applied after the first statement (%d > %d)",
			envIndex, firstStatement)
	}
	pkgs, _ := envPost.body["packages"].([]any)
	var got []string
	for _, p := range pkgs {
		got = append(got, p.(string))
	}
	if strings.Join(got, ",") != "cowsay" {
		t.Fatalf("packages = %v, want the Environment's requirements", got)
	}
}

func TestANotebookWithNoEnvironmentPostsNone(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	stub := newAgentStub(t, a)
	stub.answer(map[string]any{"applied": true}, 0)

	run := notebookRun{
		Binding: compute.Binding{WorkspaceID: ws.ID},
		Cells: []notebookCellRun{
			{Index: 0, Kind: "code", Language: "python", Source: "1", Status: "Pending"},
		},
	}
	a.driveNotebookRun(ws.ID, "item-1", "job-2", run, nil, false, referenceRoot{})

	for _, rec := range stub.recorded() {
		if rec.path == "/environment" {
			t.Fatal("a notebook binding no Environment still posted one")
		}
	}
}
