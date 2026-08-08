package api

import (
	"fmt"

	"github.com/calvinchengx/fabric-emulator/internal/notebook"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// The portal's read-only notebook view, server side. docs/44 ranks it the gap
// after the lakehouse browser, and docs/14 D3 already promised its shape: the
// stored definition rendered, the documented job API triggered, and no editor.

// NotebookCells parses a Notebook item's stored definition into its cells.
//
// Parsed HERE rather than in the browser, for the reason portalmodels.go gives
// for semantic models: a definition is base64 parts in Fabric's cell-delimiter
// format, and a second parser written in TypeScript would drift from
// internal/notebook the moment either changed. The view's whole claim is "this
// is what an engine would execute", so it asks the code the engine asks —
// notebook.Parse, the same call parseNotebookRun makes.
//
// Every cell is returned, markdown included. parseNotebookRun keeps only the
// code cells because an engine executes only those; a reader needs the prose
// that explains them, and dropping it would render a notebook nobody wrote.
func (a *API) NotebookCells(itemID string) ([]notebook.Cell, error) {
	it, err := a.Store.GetItemByID(itemID)
	if err != nil || it.Type != "Notebook" {
		return nil, fmt.Errorf("no notebook with id %q", itemID)
	}
	def, err := a.notebookContent(itemID)
	if err != nil {
		// Not an internal error: an item created without a definition is a
		// real, reachable state (fabric-cicd creates then updates). The view
		// says so rather than showing an empty notebook, which would be
		// indistinguishable from one whose cells are all blank.
		return nil, fmt.Errorf("this notebook has no definition yet")
	}
	return notebook.Parse(def), nil
}

// RunsNotebooksItself reports whether a run started now would EXECUTE, or park
// waiting for an engine that is not in this stack.
//
// The portal shows this before offering the button. Without a Spark agent,
// startJob deliberately parks a RunNotebook job with cells at CompleteAt =
// MaxInt64 so the clock cannot green it — correct, and indistinguishable at a
// glance from a run that is merely slow. A button that silently produces a job
// which never finishes is the same lie in a new place, so the view names the
// condition up front instead.
func (a *API) RunsNotebooksItself() bool { return a.runsNotebooksItself() }

// StartNotebookRunUnauthenticated starts a RunNotebook job for the portal's run
// button and returns the job instance.
//
// WHY THIS EXISTS BESIDE createJobInstance. The REST route requires a
// Contributor bearer token, which is right for the wire and wrong for the
// portal: the portal is deliberately unauthenticated, and making a run button
// mint tokens would smuggle a credential flow into a surface whose premise is
// "local state, no principal" — the same reasoning QueryModelUnauthenticated
// records for the DAX box.
//
// Unlike that one, this is NOT a read, and the difference is the point. It
// calls the same startJob every other caller reaches, so the parse, the
// cells-outstanding rule, the fault injection and the flow-bus publication all
// happen identically. The portal gets no shortcut and no second execution path;
// it gets the documented job, started without a token. What comes back is a job
// instance the existing /portal/jobs and flow views already understand.
func (a *API) StartNotebookRunUnauthenticated(itemID string) (*store.JobInstance, error) {
	it, err := a.Store.GetItemByID(itemID)
	if err != nil || it.Type != "Notebook" {
		return nil, fmt.Errorf("no notebook with id %q", itemID)
	}
	return a.startJob(it.WorkspaceID, it, "RunNotebook", store.InvokeManual, nil)
}
