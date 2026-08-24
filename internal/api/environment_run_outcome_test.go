package api

// A run whose Environment was not applied must FAIL, not report Completed.
//
// #299 made a notebook apply its bound Environment, which closed the case where
// it was never applied at all. It left a narrower one open: applying was still
// fire-and-forget, so if the agent DECLINED — most realistically because another
// session already holds a different environment, which this emulator cannot
// isolate per container — the run finished Completed with the run detail still
// listing the Environment. The notebook then died on ModuleNotFoundError
// several cells later, and the only record of the real cause was a line in the
// emulator's own log.
//
// These drive the two drivers that produce a run record. The Livy session path
// is deliberately excluded: it has no run detail to misreport, and its client
// sees the failure on the next statement.

import (
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/compute"
)

func TestANotebookRunFailsWhenItsEnvironmentIsDeclined(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	env := seedEnvironment(t, st, ws.ID, "team-env", "cowsay\n")
	stub := newAgentStub(t, a)
	stub.answer(map[string]any{
		"applied": false,
		"reason":  "session sess-9 already bound environment other-env",
	}, 0)

	run := notebookRun{
		Binding: compute.Binding{WorkspaceID: ws.ID, EnvironmentID: env.ID},
		Cells: []notebookCellRun{
			{Index: 0, Kind: "code", Language: "python", Source: "import cowsay", Status: "Pending"},
		},
	}
	a.driveNotebookRun(ws.ID, "item-1", "job-1", run, nil, false, referenceRoot{})

	// The cells must never have run: a notebook without its packages produces a
	// misleading ModuleNotFoundError, which is what this replaces.
	for _, rec := range stub.recorded() {
		if rec.path == "/statements" {
			t.Fatal("the notebook ran its cells despite the Environment being declined")
		}
	}
}

func TestANotebookRunFailsWhenItsEnvironmentCannotBeRead(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	stub := newAgentStub(t, a)

	run := notebookRun{
		Binding: compute.Binding{
			WorkspaceID:   ws.ID,
			EnvironmentID: "00000000-0000-0000-0000-000000000000",
		},
		Cells: []notebookCellRun{
			{Index: 0, Kind: "code", Language: "python", Source: "import cowsay", Status: "Pending"},
		},
	}
	a.driveNotebookRun(ws.ID, "item-1", "job-1", run, nil, false, referenceRoot{})

	for _, rec := range stub.recorded() {
		if rec.path == "/statements" {
			t.Fatal("the notebook ran its cells with an unreadable Environment binding")
		}
	}
}

// The guard must not fire on the shapes that were always fine.
func TestANotebookRunProceedsWhenItsEnvironmentApplies(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	env := seedEnvironment(t, st, ws.ID, "team-env", "cowsay\n")
	stub := newAgentStub(t, a)
	stub.answer(map[string]any{"applied": true}, 0)

	run := notebookRun{
		Binding: compute.Binding{WorkspaceID: ws.ID, EnvironmentID: env.ID},
		Cells: []notebookCellRun{
			{Index: 0, Kind: "code", Language: "python", Source: "import cowsay", Status: "Pending"},
		},
	}
	a.driveNotebookRun(ws.ID, "item-1", "job-1", run, nil, false, referenceRoot{})

	ran := false
	for _, rec := range stub.recorded() {
		if rec.path == "/statements" {
			ran = true
		}
	}
	if !ran {
		t.Fatal("a notebook whose Environment applied never ran its cells")
	}
}

func TestANotebookRunWithNoEnvironmentStillRuns(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	stub := newAgentStub(t, a)

	run := notebookRun{
		Binding: compute.Binding{WorkspaceID: ws.ID},
		Cells: []notebookCellRun{
			{Index: 0, Kind: "code", Language: "python", Source: "print(1)", Status: "Pending"},
		},
	}
	a.driveNotebookRun(ws.ID, "item-1", "job-1", run, nil, false, referenceRoot{})

	ran := false
	for _, rec := range stub.recorded() {
		if rec.path == "/statements" {
			ran = true
		}
	}
	if !ran {
		t.Fatal("a notebook binding no Environment was blocked by the guard")
	}
}

// The failure has to NAME the environment and the reason. A bare "Failed"
// would leave the caller where the ModuleNotFoundError did.
func TestTheFailureNamesTheEnvironmentAndTheReason(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	env := seedEnvironment(t, st, ws.ID, "team-env", "cowsay\n")
	stub := newAgentStub(t, a)
	stub.answer(map[string]any{
		"applied": false,
		"reason":  "session sess-9 already bound environment other-env",
	}, 0)

	out := a.applyEnvironment("sess-1", ws.ID, env.ID)
	if out.OK {
		t.Fatal("declined environment reported OK")
	}
	if !strings.Contains(out.Reason, env.ID) {
		t.Fatalf("reason %q does not name the environment", out.Reason)
	}
	if !strings.Contains(out.Reason, "already bound") {
		t.Fatalf("reason %q does not carry the agent's reason", out.Reason)
	}
}
