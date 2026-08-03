package api

// Notebook cell execution. A RunNotebook job on a Notebook item is *parsed* by
// the emulator (real Go parser: `notebook-content.py` → ordered cells), and its
// cells are executed by a real Spark engine — the emulator owns the parse, the
// run record, and the job's terminal status; Spark owns the compute.
//
// Real Fabric runs a notebook on a Spark pool that reports back to the service.
// The emulator mirrors that: creating the job records a run (cells Pending);
// an execution engine (the Spark runner in the e2e) POSTs per-cell results to
// the runner callback, which finalises the run and the job's status.
//
// THE JOB'S STATUS IS NOT DERIVED FROM THE CLOCK. Every other item type's job
// completes when virtual time passes its completion instant, and a notebook
// once did too — so a RunNotebook job read "Completed" with every cell still
// Pending and no engine having run a line. Callers reasonably read that as "the
// notebook ran". Now a job with cells outstanding has no completion time at all
// (jobs.go sets it beyond any clock); only the engine's callback finishes it,
// so a terminal status means execution happened.
//
// The one exception is a run with nothing to execute — no definition, or no
// code cells. There is no engine to wait for, so it completes immediately;
// `notebookutils.notebook.run` polls to a terminal state and must reach one.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/compute"
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
	Status      string              `json:"status"` // Pending | Completed | Failed
	ExitValue   string              `json:"exitValue,omitempty"`
	Binding     compute.Binding     `json:"binding,omitempty"`
	Environment compute.Environment `json:"environment,omitempty"`
	Cells       []notebookCellRun   `json:"cells"`
}

// parseNotebookRun parses a Notebook item's definition into the run record an
// engine will execute. It takes no job ID because the CALLER needs the result
// before the job exists: whether there are cells outstanding decides whether
// the job may complete on the clock at all (see startJob).
func (a *API) parseNotebookRun(it *store.Item) (notebookRun, string) {
	def, err := a.notebookContent(it.ID)
	run := notebookRun{Status: "Pending", Cells: []notebookCellRun{}}
	if err != nil {
		// No definition: nothing to parse and nothing to execute. The run is
		// not waiting on an engine, so it is not left hanging for one.
		return run, ""
	}
	run.Binding = compute.NotebookBinding(def)
	run.Binding, run.Environment, err = a.resolveComputeBinding(it, run.Binding)
	if err != nil {
		run.Status = "Failed"
		return run, "ComputeBindingInvalid"
	}
	// Re-sequence the executable code cells 0..n so the run is self-
	// contiguous (markdown/metadata don't leave gaps) and an engine can
	// iterate + report by a simple index.
	for i, c := range notebook.CodeCells(notebook.Parse(def)) {
		run.Cells = append(run.Cells, notebookCellRun{
			Index: i, Kind: string(c.Kind), Language: c.Language, Source: c.Source, Status: "Pending",
		})
	}
	return run, ""
}

func (a *API) resolveComputeBinding(owner *store.Item, binding compute.Binding) (compute.Binding, compute.Environment, error) {
	if binding.WorkspaceID == "" {
		binding.WorkspaceID = owner.WorkspaceID
	}
	if binding.LakehouseID != "" {
		lake, err := a.Store.GetItem(binding.WorkspaceID, binding.LakehouseID)
		if err != nil || lake.Type != "Lakehouse" {
			return binding, compute.Environment{}, fmt.Errorf("default lakehouse is unavailable")
		}
		binding.LakehouseName = lake.DisplayName
	}
	if binding.EnvironmentID == "" {
		return binding, compute.Environment{}, nil
	}
	envWorkspaceID := binding.EnvironmentWorkspaceID
	if envWorkspaceID == "" {
		envWorkspaceID = owner.WorkspaceID
	}
	binding.EnvironmentWorkspaceID = envWorkspaceID
	envItem, err := a.Store.GetItem(envWorkspaceID, binding.EnvironmentID)
	if err != nil || envItem.Type != "Environment" {
		return binding, compute.Environment{}, fmt.Errorf("environment is unavailable")
	}
	parts, err := a.Store.GetDefinition(envItem.ID)
	if err != nil {
		return binding, compute.Environment{}, err
	}
	env, err := compute.ParseEnvironment(parts)
	return binding, env, err
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
	Status    string               `json:"status"` // Completed | Failed (optional; derived if absent)
	ExitValue string               `json:"exitValue"`
	Cells     []notebookCellResult `json:"cells"`
}

// notebookCellResult is one cell's outcome as the engine that ran it reports.
//
// Named rather than inline because the emulator now produces these itself when
// it drives the agent (notebookdrive.go), and an anonymous struct cannot be
// built anywhere but here.
type notebookCellResult struct {
	Index  int    `json:"index"`
	Status string `json:"status"` // Succeeded | Failed | Skipped
	Output string `json:"output"`
	Error  string `json:"error"`
	// Reads/Writes are the datasets this cell actually touched, reported by
	// the engine that executed it. Shaped after OpenLineage's
	// RunEvent{inputs,outputs}. The emulator never parses notebook code to
	// infer these — an engine reporting what it did is an exact fact, a
	// static guess is not.
	Reads  []lineageRef `json:"reads"`
	Writes []lineageRef `json:"writes"`
}

// lineageRef addresses one dataset a notebook cell read or wrote. workspaceId
// defaults to the run's own workspace.
type lineageRef struct {
	WorkspaceID string `json:"workspaceId"`
	ItemID      string `json:"itemId"`
	Path        string `json:"path"`
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

	run = a.finalizeNotebookRun(wid, iid, jid, run, body)
	writeJSON(w, http.StatusOK, run)
}

// finalizeNotebookRun merges an engine's results into the recorded run, records
// the lineage they imply, and takes the job to a terminal state.
//
// Shared by the two engines that can report: an external Spark pool posting to
// the callback above, and the emulator driving the agent itself
// (notebookdrive.go). One function on purpose — a second, parallel copy of
// "what does Completed mean" is how the two paths would come to disagree about
// a notebook that half worked.
func (a *API) finalizeNotebookRun(wid, iid, jid string, run notebookRun, body notebookResultBody) notebookRun {
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
	a.recordNotebookLineage(wid, jid, body)
	a.recordObservedLineage(wid, jid)
	// Reflect the real run in the job (deterministically terminal now).
	_ = a.Store.FinalizeJob(iid, jid, failCode)
	a.publishJobOutcome(wid, iid, jid, failCode)
	return run
}

// recordNotebookLineage turns a cell's reported read/write set into lineage
// edges — one per (read x write) pair, so a cell joining two tables into one
// records both inputs.
//
// The activity name is cell[<index>], which matters: the store's unique key is
// (job, activity, source, target), so without a per-cell name a notebook
// touching the same pair in several cells would collapse into a single row.
//
// A cell that only reads produces no edge. That is not a limitation to work
// around — lineage describes movement, and a read that produced nothing did not
// move anything.
func (a *API) recordNotebookLineage(wid, jid string, body notebookResultBody) {
	for _, c := range body.Cells {
		if len(c.Reads) == 0 || len(c.Writes) == 0 {
			continue
		}
		activity := fmt.Sprintf("cell[%d]", c.Index)
		for _, in := range c.Reads {
			for _, out := range c.Writes {
				if in.ItemID == "" || in.Path == "" || out.ItemID == "" || out.Path == "" {
					continue // an incomplete reference is not an exact fact
				}
				srcWS, dstWS := in.WorkspaceID, out.WorkspaceID
				if srcWS == "" {
					srcWS = wid
				}
				if dstWS == "" {
					dstWS = wid
				}
				_ = a.Store.CreateLineageEdge(&store.LineageEdge{
					WorkspaceID: wid, JobID: jid, ActivityName: activity,
					SourceWorkspaceID: srcWS, SourceItemID: in.ItemID, SourcePath: in.Path,
					TargetWorkspaceID: dstWS, TargetItemID: out.ItemID, TargetPath: out.Path,
					Producer: store.ProducerNotebook,
				})
			}
		}
	}
}

// recordObservedLineage turns the I/O the storage layer actually saw during
// this run into lineage edges. The runtime tags each request with the cell it
// is executing, so reads and writes pair within a cell — no cross-product
// across the whole notebook, and nothing inferred from user code.
//
// These edges carry ProducerNotebookObserved so a catalog can tell evidence
// (the emulator watched it happen) from a report (the engine said so).
func (a *API) recordObservedLineage(wid, jid string) {
	accesses, err := a.Store.ListNotebookAccesses(jid)
	if err != nil || len(accesses) == 0 {
		return
	}
	type cellIO struct{ reads, writes []*store.NotebookAccess }
	byCell := map[int]*cellIO{}
	for _, ac := range accesses {
		c, ok := byCell[ac.CellIndex]
		if !ok {
			c = &cellIO{}
			byCell[ac.CellIndex] = c
		}
		if ac.Direction == store.AccessRead {
			c.reads = append(c.reads, ac)
		} else {
			c.writes = append(c.writes, ac)
		}
	}
	for idx, io := range byCell {
		for _, in := range io.reads {
			for _, out := range io.writes {
				if in.ItemID == out.ItemID && in.Path == out.Path {
					continue // reading a table to rewrite it is not an edge to itself
				}
				_ = a.Store.CreateLineageEdge(&store.LineageEdge{
					WorkspaceID: wid, JobID: jid, ActivityName: fmt.Sprintf("cell[%d]", idx),
					SourceWorkspaceID: wid, SourceItemID: in.ItemID, SourcePath: in.Path,
					TargetWorkspaceID: wid, TargetItemID: out.ItemID, TargetPath: out.Path,
					Producer: store.ProducerNotebookObserved,
				})
			}
		}
	}
}
