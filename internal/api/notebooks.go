package api

// Notebook cell execution. A RunNotebook job on a Notebook item is *parsed* by
// the emulator (real Go parser: `notebook-content.py` → ordered cells), and its
// cells are executed by a real Spark engine — the emulator owns the parse, the
// run record, and the job's terminal status; Spark owns the compute.
//
// Real Fabric runs a notebook on a Spark pool that reports back to the service.
// The emulator mirrors that: creating the job records a run (cells Pending);
// an execution engine (the Spark runner in the e2e) POSTs per-cell results to
// the runner callback, which finalises the run and the job's status. Without an
// engine the cells stay Pending — honestly "parsed, not executed" — while the
// job still completes on the clock, as before.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/notebook"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// definitionPart decodes one named part (by path) from an item's definition,
// falling back to the sole part when there's exactly one.
func (a *API) definitionPart(itemID, path string) ([]byte, error) {
	parts, err := a.Store.GetDefinition(itemID)
	if err != nil {
		return nil, err
	}
	var payload string
	for _, p := range parts {
		if p.Path == path {
			payload = p.Payload
			break
		}
	}
	if payload == "" && len(parts) == 1 {
		payload = parts[0].Payload
	}
	if payload == "" {
		return nil, fmt.Errorf("no %s in definition", path)
	}
	return base64.StdEncoding.DecodeString(payload)
}

// notebookCellRun is one cell's parsed source plus its execution result.
type notebookCellRun struct {
	Index    int    `json:"index"`
	Kind     string `json:"kind"`
	Language string `json:"language,omitempty"`
	Source   string `json:"source"`
	Status   string `json:"status"` // Pending | Succeeded | Failed | Skipped
	Output   string `json:"output,omitempty"`
	Error    string `json:"error,omitempty"`
}

// notebookRun is the whole run: overall status, the exit value, and per-cell detail.
type notebookRun struct {
	Status    string            `json:"status"` // Pending | Completed | Failed
	ExitValue string            `json:"exitValue,omitempty"`
	Cells     []notebookCellRun `json:"cells"`
}

// startNotebookRun parses a Notebook item's definition and records a Pending
// run for the job, so the cells are queryable before any engine executes them.
func (a *API) startNotebookRun(it *store.Item, jobID string) {
	def, err := a.notebookContent(it.ID)
	run := notebookRun{Status: "Pending", Cells: []notebookCellRun{}}
	if err == nil {
		// Re-sequence the executable code cells 0..n so the run is self-
		// contiguous (markdown/metadata don't leave gaps) and an engine can
		// iterate + report by a simple index.
		for i, c := range notebook.CodeCells(notebook.Parse(def)) {
			run.Cells = append(run.Cells, notebookCellRun{
				Index: i, Kind: string(c.Kind), Language: c.Language, Source: c.Source, Status: "Pending",
			})
		}
	}
	a.saveNotebookRun(jobID, run)
}

// notebookContent decodes the `notebook-content.py` payload from the item's
// definition parts.
func (a *API) notebookContent(itemID string) ([]byte, error) {
	return a.definitionPart(itemID, "notebook-content.py")
}

func (a *API) saveNotebookRun(jobID string, run notebookRun) {
	blob, _ := json.Marshal(run)
	_ = a.Store.SetNotebookRun(jobID, run.Status, string(blob))
}

// getNotebookRun returns the parsed/executed run detail for a RunNotebook job.
func (a *API) getNotebookRun(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	if _, _, ok := a.requireRole(w, wid, p, store.RoleViewer); !ok {
		return
	}
	if _, err := a.Store.GetJobInstance(r.PathValue("iid"), r.PathValue("jid")); err != nil {
		writeErr(w, http.StatusNotFound, "JobInstanceNotFound", "No such job instance.")
		return
	}
	_, runJSON, err := a.Store.GetNotebookRun(r.PathValue("jid"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "NotebookRunNotFound", "This job has no notebook run detail.")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(runJSON))
}

// notebookResultBody is what an execution engine reports back per cell.
type notebookResultBody struct {
	Status    string `json:"status"` // Completed | Failed (optional; derived if absent)
	ExitValue string `json:"exitValue"`
	Cells     []struct {
		Index  int    `json:"index"`
		Status string `json:"status"` // Succeeded | Failed | Skipped
		Output string `json:"output"`
		Error  string `json:"error"`
	} `json:"cells"`
}

// reportNotebookRun is the engine → service callback (the Spark runner posts
// here after executing the cells). It merges per-cell results into the recorded
// run and finalises the job's terminal status from the real outcome.
func (a *API) reportNotebookRun(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	if _, _, ok := a.requireRole(w, wid, p, store.RoleContributor); !ok {
		return
	}
	iid, jid := r.PathValue("iid"), r.PathValue("jid")
	if _, err := a.Store.GetJobInstance(iid, jid); err != nil {
		writeErr(w, http.StatusNotFound, "JobInstanceNotFound", "No such job instance.")
		return
	}
	_, runJSON, err := a.Store.GetNotebookRun(jid)
	if err != nil {
		writeErr(w, http.StatusNotFound, "NotebookRunNotFound", "This job is not a notebook run.")
		return
	}
	var run notebookRun
	_ = json.Unmarshal([]byte(runJSON), &run)

	var body notebookResultBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "Malformed notebook result body.")
		return
	}

	byIdx := map[int]int{}
	for i, c := range run.Cells {
		byIdx[c.Index] = i
	}
	for _, cr := range body.Cells {
		if i, ok := byIdx[cr.Index]; ok {
			run.Cells[i].Status = cr.Status
			run.Cells[i].Output = cr.Output
			run.Cells[i].Error = cr.Error
		}
	}
	run.ExitValue = body.ExitValue

	// Overall status: explicit if given, else failed iff any cell failed.
	failCode := ""
	run.Status = "Completed"
	if body.Status == "Failed" {
		run.Status = "Failed"
	}
	for _, c := range run.Cells {
		if c.Status == "Failed" {
			run.Status = "Failed"
		}
	}
	if run.Status == "Failed" {
		failCode = "NotebookExecutionFailed"
	}

	a.saveNotebookRun(jid, run)
	// Reflect the real run in the job (deterministically terminal now).
	_ = a.Store.FinalizeJob(iid, jid, failCode)
	writeJSON(w, http.StatusOK, run)
}
