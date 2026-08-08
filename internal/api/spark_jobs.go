package api

import (
	"encoding/json"
	"net/http"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/compute"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

type sparkJobRun struct {
	Status      string              `json:"status"`
	Job         compute.SparkJob    `json:"job"`
	Binding     compute.Binding     `json:"binding,omitempty"`
	Environment compute.Environment `json:"environment,omitempty"`
	Output      string              `json:"output,omitempty"`
	Error       string              `json:"error,omitempty"`
}

// parseSparkJobRun resolves a SparkJobDefinition into the run an engine will
// execute. Like parseNotebookRun it takes no job ID, because the caller needs
// the outcome before the job exists: a definition that parses has real work
// outstanding, and a job with work outstanding must not be completed by the
// clock (see startJob).
//
// Unlike a notebook there is no "nothing to execute" case — a Spark job
// definition that parses always names a main file — so a successful parse
// always means: wait for the engine.
func (a *API) parseSparkJobRun(it *store.Item) (sparkJobRun, string) {
	parts, err := a.Store.GetDefinition(it.ID)
	if err != nil {
		return sparkJobRun{Status: "Failed", Error: "no definition"}, "InvalidSparkJobDefinition"
	}
	job, binding, err := compute.ParseSparkJob(parts)
	run := sparkJobRun{Status: "Pending", Job: job, Binding: binding}
	if err == nil {
		run.Binding, run.Environment, err = a.resolveComputeBinding(it, binding)
	}
	if err != nil {
		run.Status, run.Error = "Failed", err.Error()
		return run, "InvalidSparkJobDefinition"
	}
	return run, ""
}

// saveSparkJobRun records the parsed run against a job that now exists.
func (a *API) saveSparkJobRun(jobID string, run sparkJobRun) {
	blob, _ := json.Marshal(run)
	_ = a.Store.SetNotebookRun(jobID, run.Status, string(blob))
}

func (a *API) getSparkJobRun(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
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
		writeErr(w, http.StatusNotFound, "SparkJobRunNotFound", "This job has no Spark run detail.")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(runJSON))
}

func (a *API) reportSparkJobRun(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	if _, _, ok := a.requireRole(w, wid, p, store.RoleContributor); !ok {
		return
	}
	iid, jid := r.PathValue("iid"), r.PathValue("jid")
	_, raw, err := a.Store.GetNotebookRun(jid)
	if err != nil {
		writeErr(w, http.StatusNotFound, "SparkJobRunNotFound", "This job has no Spark run detail.")
		return
	}
	var run sparkJobRun
	if json.Unmarshal([]byte(raw), &run) != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", "Malformed stored Spark run.")
		return
	}
	var result struct{ Status, Output, Error string }
	if json.NewDecoder(r.Body).Decode(&result) != nil || (result.Status != "Completed" && result.Status != "Failed") {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "status must be Completed or Failed.")
		return
	}
	// One finalisation path for both engines: an external one reporting here,
	// and the emulator's own driver (sparkjobdrive.go). Two writers of one
	// terminal state is how they drift.
	a.finishSparkJobRun(wid, iid, jid, run, result.Status, result.Output, result.Error)
	run.Status, run.Output, run.Error = result.Status, result.Output, result.Error
	writeJSON(w, http.StatusOK, run)
}
