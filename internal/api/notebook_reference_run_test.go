package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// Fabric's reference-run rule, from the notebookutils reference: a referenced
// child notebook runs "only if they use the same lakehouse as the parent,
// inherit the parent's lakehouse, or neither defines one. The execution is
// blocked if the child specifies a different lakehouse than the parent."
//
// A REFUSAL, and the asymmetry is the whole feature: a test that only checks
// the allowed cases would pass against code that never blocks anything.

// referenceRun submits a RunNotebook job as a reference run from a parent bound
// to parentLakehouse, optionally setting the bypass flag.
func referenceRun(t *testing.T, a *API, wid, iid, parentLakehouse string, bypass bool) string {
	t.Helper()
	exec := map[string]any{}
	if parentLakehouse != "" {
		exec["parentLakehouseId"] = parentLakehouse
	}
	if bypass {
		exec["useRootDefaultLakehouse"] = true
	}
	body, err := json.Marshal(map[string]any{"executionData": exec})
	if err != nil {
		t.Fatal(err)
	}
	_, jid := runJob(t, a, wid, iid, "jobType=RunNotebook", string(body))
	return jid
}

// notebookBoundTo creates a notebook whose default lakehouse is lakehouseID
// (empty for a notebook that declares none), with one executable cell.
func notebookBoundTo(t *testing.T, st *store.Store, wid, lakehouseID string) *store.Item {
	t.Helper()
	source := "# Fabric notebook source\n"
	if lakehouseID != "" {
		source += `# METADATA ********************
# META {
# META   "dependencies": {
# META     "lakehouse": {"default_lakehouse":"` + lakehouseID + `", "default_lakehouse_workspace_id":"` + wid + `"}
# META   }
# META }
`
	}
	source += "# CELL ********************\nprint(1)\n"
	return createNotebook(t, st, wid, source)
}

func newLakehouse(t *testing.T, st *store.Store, wid, name string) *store.Item {
	t.Helper()
	lake := &store.Item{WorkspaceID: wid, Type: "Lakehouse", DisplayName: name}
	if err := st.CreateItem(lake, nil); err != nil {
		t.Fatal(err)
	}
	return lake
}

func TestReferenceRunBlocksAChildOnADifferentLakehouse(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	parentLake := newLakehouse(t, st, ws.ID, "parent-lake")
	childLake := newLakehouse(t, st, ws.ID, "child-lake")
	nb := notebookBoundTo(t, st, ws.ID, childLake.ID)

	jid := referenceRun(t, a, ws.ID, nb.ID, parentLake.ID, false)
	if got := awaitJob(t, a, ws.ID, nb.ID, jid); got != store.JobFailed {
		t.Fatalf("a child on a different lakehouse must be blocked, got %s", got)
	}
}

func TestReferenceRunBlockNamesTheCauseAndTheWayOut(t *testing.T) {
	// The shim surfaces this text as the exception a user reads. "The job
	// failed." for a refusal with a specific, fixable cause is indistinguishable
	// from a cell that threw.
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	parentLake := newLakehouse(t, st, ws.ID, "parent-lake")
	childLake := newLakehouse(t, st, ws.ID, "child-lake")
	nb := notebookBoundTo(t, st, ws.ID, childLake.ID)

	jid := referenceRun(t, a, ws.ID, nb.ID, parentLake.ID, false)
	w := do(a.getJobInstance, admin, "GET", "", map[string]string{"wid": ws.ID, "iid": nb.ID, "jid": jid})
	var body struct {
		FailureReason struct {
			ErrorCode string `json:"errorCode"`
			Message   string `json:"message"`
		} `json:"failureReason"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.FailureReason.ErrorCode != "NotebookLakehouseMismatch" {
		t.Fatalf("errorCode = %q", body.FailureReason.ErrorCode)
	}
	if !strings.Contains(body.FailureReason.Message, "useRootDefaultLakehouse") {
		t.Fatalf("message must name the bypass, got %q", body.FailureReason.Message)
	}
}

func TestReferenceRunAllowsTheSameLakehouse(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lake := newLakehouse(t, st, ws.ID, "shared-lake")
	nb := notebookBoundTo(t, st, ws.ID, lake.ID)

	jid := referenceRun(t, a, ws.ID, nb.ID, lake.ID, false)
	if got := jobStatus(t, a, ws.ID, nb.ID, jid); got == store.JobFailed {
		t.Fatalf("same lakehouse must be allowed, got %s", got)
	}
}

func TestReferenceRunAllowsAChildThatInheritsByDeclaringNone(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	parentLake := newLakehouse(t, st, ws.ID, "parent-lake")
	nb := notebookBoundTo(t, st, ws.ID, "")

	jid := referenceRun(t, a, ws.ID, nb.ID, parentLake.ID, false)
	if got := jobStatus(t, a, ws.ID, nb.ID, jid); got == store.JobFailed {
		t.Fatalf("a child declaring no lakehouse inherits, got %s", got)
	}
}

func TestUseRootDefaultLakehouseBypassesTheBlock(t *testing.T) {
	// The flag's ONLY job. Without this assertion the guard could reject every
	// mismatch unconditionally and every other test here would still pass.
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	parentLake := newLakehouse(t, st, ws.ID, "parent-lake")
	childLake := newLakehouse(t, st, ws.ID, "child-lake")
	nb := notebookBoundTo(t, st, ws.ID, childLake.ID)

	jid := referenceRun(t, a, ws.ID, nb.ID, parentLake.ID, true)
	if got := jobStatus(t, a, ws.ID, nb.ID, jid); got == store.JobFailed {
		t.Fatalf("useRootDefaultLakehouse must bypass the check, got %s", got)
	}
}

func TestADirectJobSubmissionIsNotSubjectToTheRule(t *testing.T) {
	// No parentLakehouseId means this is not a reference run. Applying the rule
	// to every job would break running a notebook on its own.
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	childLake := newLakehouse(t, st, ws.ID, "child-lake")
	nb := notebookBoundTo(t, st, ws.ID, childLake.ID)

	jid := referenceRun(t, a, ws.ID, nb.ID, "", false)
	if got := jobStatus(t, a, ws.ID, nb.ID, jid); got == store.JobFailed {
		t.Fatalf("a direct submission must not be blocked, got %s", got)
	}
}

func TestReferenceRunLakehouseCodeUnit(t *testing.T) {
	cases := []struct {
		name  string
		exec  map[string]any
		child string
		want  string
	}{
		{"not a reference run", map[string]any{}, "child", ""},
		{"same lakehouse", map[string]any{"parentLakehouseId": "a"}, "a", ""},
		{"child inherits", map[string]any{"parentLakehouseId": "a"}, "", ""},
		{"different", map[string]any{"parentLakehouseId": "a"}, "b", "NotebookLakehouseMismatch"},
		{"bypassed", map[string]any{"parentLakehouseId": "a", "useRootDefaultLakehouse": true}, "b", ""},
		{"bypass false still blocks", map[string]any{"parentLakehouseId": "a", "useRootDefaultLakehouse": false}, "b", "NotebookLakehouseMismatch"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := referenceRunLakehouseCode(c.exec, c.child); got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}

// `builtin/` means the ROOT notebook's resource folder, never the running
// one, so the root has to reach the child's statements. Nothing sent one
// before, and `notebookutils.nbResPath` then resolved to the child's own
// folder — a notebook reading different files depending on how it was started.
func TestTheRootNotebookReachesTheChildsStatements(t *testing.T) {
	root := referenceRootOf(map[string]any{
		"rootNotebookId": "nb-root", "rootWorkspaceId": "ws-root",
	})
	m := notebookStatement("s", "code", "", "ws-child", "nb-child", "job", notebookRun{}, false, nil, root)
	if m["rootNotebookId"] != "nb-root" || m["rootWorkspaceId"] != "ws-root" {
		t.Fatalf("root did not reach the statement: %v", m)
	}
	// ...and the running notebook is still reported as itself. Overwriting
	// `notebookId` with the root would hide which notebook is executing.
	if m["notebookId"] != "nb-child" {
		t.Fatalf("the running notebook was replaced by the root: %v", m)
	}
}

// A DIRECT submission is its own root, so it sends none — the shim falls back
// to the current notebook. Emitting an empty key instead would make every
// direct run look like a reference run with a missing root.
func TestADirectSubmissionCarriesNoRoot(t *testing.T) {
	m := notebookStatement("s", "code", "", "ws", "nb", "job", notebookRun{}, false, nil,
		referenceRootOf(map[string]any{}))
	if _, ok := m["rootNotebookId"]; ok {
		t.Fatalf("a direct submission claimed a root: %v", m)
	}
	if _, ok := m["rootWorkspaceId"]; ok {
		t.Fatalf("a direct submission claimed a root workspace: %v", m)
	}
}

// Every documented typed collection routes, and an undocumented one does not.
//
// 25 of Fabric's documented collections answered 404 while the GENERIC item
// surface created every one of their types happily: `typedCollections` was a
// fixed map over a surface that already handled all of them, so an unlisted
// segment simply never registered and net/http answered 404. A client written
// from Microsoft's reference then got a 404 at the exact URL that reference
// prints — indistinguishable from a typo in its own URL, which is the wrong
// shape for a project where everything else fails loudly and says why.
//
// The negative control is the half that makes this an assertion: a segment
// nobody documents must still 404, or "routes" would mean "the mux matches
// anything" and the test would pass over a catch-all.
func TestEveryTypedCollectionRoutesAndNothingElseDoes(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	mux := http.NewServeMux()
	a.registerTyped(mux)

	if len(typedCollections) < 50 {
		t.Fatalf("only %d typed collections registered; the map lost entries",
			len(typedCollections))
	}
	for collection, itemType := range typedCollections {
		for _, verb := range []string{"GET", "POST"} {
			url := "/v1/workspaces/" + ws.ID + "/" + collection
			_, pattern := mux.Handler(httptest.NewRequest(verb, url, nil))
			if pattern == "" {
				t.Errorf("%s %s (%s) does not route; Fabric serves it",
					verb, collection, itemType)
			}
		}
	}
	// ...and the definition routes the per-item-type reference prints.
	for _, suffix := range []string{"/getDefinition", "/updateDefinition"} {
		url := "/v1/workspaces/" + ws.ID + "/dataAgents/some-id" + suffix
		if _, pattern := mux.Handler(httptest.NewRequest("POST", url, nil)); pattern == "" {
			t.Errorf("POST dataAgents%s does not route", suffix)
		}
	}
	// THE NEGATIVE CONTROL.
	url := "/v1/workspaces/" + ws.ID + "/notAFabricCollection"
	if _, pattern := mux.Handler(httptest.NewRequest("GET", url, nil)); pattern != "" {
		t.Fatalf("an undocumented collection routed to %q; the mux matches anything",
			pattern)
	}
}
