package server

import (
	"encoding/json"
	"net/http"

	"github.com/calvinchengx/fabric-emulator/internal/notebook"
)

// The portal's notebook view. docs/44 ranks this the gap after the lakehouse
// browser, and docs/14 D3 fixes its shape: render the stored definition, offer
// the DOCUMENTED job API, and build no editor. Read-only plus one button is the
// whole surface — a cell the portal could edit would be a client
// re-implementation of authoring, and users would develop against a path no
// real Fabric client takes.

// portalNotebook is one row of the notebook list.
type portalNotebook struct {
	ItemID      string `json:"itemId"`
	Name        string `json:"name"`
	WorkspaceID string `json:"workspaceId"`
	Workspace   string `json:"workspace"`
	// Cells is the parsed cell count, or 0 when there is no definition.
	// HasDefinition distinguishes the two: a notebook with no definition is not
	// a notebook with no cells, and a RunNotebook job tells them apart by
	// failing NotebookDefinitionInvalid rather than completing.
	Cells         int  `json:"cells"`
	CodeCells     int  `json:"codeCells"`
	HasDefinition bool `json:"hasDefinition"`
}

func (s *Server) portalNotebooks(w http.ResponseWriter, r *http.Request) {
	workspaces, err := s.Store.ListAllWorkspaces()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]string{"message": err.Error()}})
		return
	}
	out := make([]portalNotebook, 0)
	for _, ws := range workspaces {
		items, err := s.Store.ListItems(ws.ID, "Notebook")
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]string{"message": err.Error()}})
			return
		}
		for _, it := range items {
			row := portalNotebook{
				ItemID: it.ID, Name: it.DisplayName,
				WorkspaceID: ws.ID, Workspace: ws.DisplayName,
			}
			// A definition-less notebook still LISTS. It is a real state —
			// fabric-cicd creates the item and updates the definition after —
			// and hiding it would make the list disagree with the item list it
			// is a view of.
			if cells, err := s.API.NotebookCells(it.ID); err == nil {
				row.HasDefinition = true
				row.Cells = len(cells)
				for _, c := range cells {
					if c.Kind == notebook.Code {
						row.CodeCells++
					}
				}
			}
			out = append(out, row)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"notebooks": out})
}

// portalNotebookDetail renders one notebook's cells, plus whether a run started
// now would actually execute.
func (s *Server) portalNotebookDetail(w http.ResponseWriter, r *http.Request) {
	itemID := r.PathValue("id")
	it, err := s.Store.GetItemByID(itemID)
	if err != nil || it.Type != "Notebook" {
		writeJSONError(w, http.StatusNotFound, "NotebookNotFound", "No notebook with that id.")
		return
	}
	out := map[string]any{
		"itemId": it.ID, "name": it.DisplayName, "workspaceId": it.WorkspaceID,
		// Named on the wire so the view can state the consequence BEFORE the
		// button is pressed, rather than leaving a parked job to explain it.
		"runsHere": s.API.RunsNotebooksItself(),
	}
	if ws, err := s.Store.GetWorkspace(it.WorkspaceID); err == nil {
		out["workspace"] = ws.DisplayName
	}
	cells, err := s.API.NotebookCells(itemID)
	if err != nil {
		// Not a server error and not an empty notebook: no definition yet.
		// Rendering zero cells here would be indistinguishable from a notebook
		// whose cells are all blank, and only one of those can be run.
		out["readable"] = false
		out["message"] = err.Error()
		out["cells"] = []notebook.Cell{}
		writeJSON(w, http.StatusOK, out)
		return
	}
	out["readable"] = true
	out["cells"] = cells
	writeJSON(w, http.StatusOK, out)
}

// portalNotebookRun starts a RunNotebook job — the documented API, without a
// token. See API.StartNotebookRunUnauthenticated for why that is the shape.
//
// The only mutating portal route besides the model query box, and deliberately
// so: it starts the job real clients start, then hands the caller a job id and
// gets out of the way. Progress belongs to the Jobs and Flow views, which
// already read the same job.
func (s *Server) portalNotebookRun(w http.ResponseWriter, r *http.Request) {
	j, err := s.API.StartNotebookRunUnauthenticated(r.PathValue("id"))
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "NotebookNotFound", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"jobId": j.ID, "status": j.StatusAt(s.Clock.Now()), "runsHere": s.API.RunsNotebooksItself(),
	})
}

// portalNotebookRunDetail returns the per-cell record for a RunNotebook job.
//
// This is what makes the view honest about a run rather than only about a
// definition: the job's own status is one word, while the run record names
// which cell is Pending, which Failed and what it printed. The engine writes it
// through notebookRunResult; the portal only reads it back.
func (s *Server) portalNotebookRunDetail(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("jid")
	_, runJSON, err := s.Store.GetNotebookRun(jobID)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "NotebookRunNotFound", "This job has no notebook run detail.")
		return
	}
	var run any
	if err := json.Unmarshal([]byte(runJSON), &run); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, run)
}
