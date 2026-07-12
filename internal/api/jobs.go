package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// createJobInstance schedules an item job (nothing executes — state is
// clock-derived). 202 with Location pointing at the job instance, per the
// documented run-on-demand shape.
func (a *API) createJobInstance(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	if _, _, ok := a.requireRole(w, wid, p, store.RoleContributor); !ok {
		return
	}
	it, err := a.Store.GetItem(wid, r.PathValue("iid"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "ItemNotFound", "The item is not available.")
		return
	}
	jobType := r.URL.Query().Get("jobType")
	if jobType == "" {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "jobType query parameter is required.")
		return
	}
	delay, failWith := a.nextOpFate()
	j := &store.JobInstance{ItemID: it.ID, JobType: jobType, FailWith: failWith}
	j.CompleteAt = a.Store.Now() + delay
	if err := a.Store.CreateJobInstance(j); err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	// DataPipeline jobs actually execute: the interpreter runs the definition's
	// control flow now and records the activity runs; a pipeline failure sets
	// the job's terminal status (overriding fault injection).
	if it.Type == "DataPipeline" {
		var body struct {
			ExecutionData struct {
				Parameters map[string]any `json:"parameters"`
			} `json:"executionData"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if code := a.runPipeline(wid, it, j.ID, body.ExecutionData.Parameters); code != "" && j.FailWith == "" {
			j.FailWith = code
			_ = a.Store.SetJobFailure(it.ID, j.ID, code)
		}
	}
	// A RunNotebook job: parse the notebook into cells now (real Go parser) and
	// record a Pending run. A real Spark engine executes the cells and reports
	// back to finalise the run + the job's status (see notebooks.go).
	if it.Type == "Notebook" && jobType == "RunNotebook" {
		a.startNotebookRun(it, j.ID)
	}
	loc := fmt.Sprintf("https://%s/v1/workspaces/%s/items/%s/jobs/instances/%s", r.Host, wid, it.ID, j.ID)
	w.Header().Set("Location", loc)
	w.Header().Set("Retry-After", fmt.Sprintf("%d", a.RetryAfterSeconds))
	w.WriteHeader(http.StatusAccepted)
}

// jobBody is the wire shape of a job instance.
func (a *API) jobBody(j *store.JobInstance, wid string) map[string]any {
	now := a.Store.Now()
	status := j.StatusAt(now)
	body := map[string]any{
		"id": j.ID, "itemId": j.ItemID, "workspaceId": wid,
		"jobType": j.JobType, "invokeType": j.InvokeType, "status": status,
		"startTimeUtc": time.Unix(j.CreatedAt, 0).UTC().Format(time.RFC3339),
	}
	switch status {
	case store.JobCompleted, store.JobFailed:
		body["endTimeUtc"] = time.Unix(j.CompleteAt, 0).UTC().Format(time.RFC3339)
	case store.JobCancelled:
		body["endTimeUtc"] = time.Unix(now, 0).UTC().Format(time.RFC3339)
	}
	if status == store.JobFailed {
		body["failureReason"] = map[string]string{"errorCode": j.FailWith, "message": "The job failed."}
	}
	return body
}

// getJobInstance returns the job's clock-derived state.
func (a *API) getJobInstance(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	if _, _, ok := a.requireRole(w, wid, p, store.RoleViewer); !ok {
		return
	}
	j, err := a.Store.GetJobInstance(r.PathValue("iid"), r.PathValue("jid"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "JobInstanceNotFound", "No such job instance.")
		return
	}
	writeJSON(w, http.StatusOK, a.jobBody(j, wid))
}

// cancelJobInstance marks the job cancelled (202, like the real API).
func (a *API) cancelJobInstance(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	if _, _, ok := a.requireRole(w, wid, p, store.RoleContributor); !ok {
		return
	}
	if err := a.Store.CancelJobInstance(r.PathValue("iid"), r.PathValue("jid")); err != nil {
		writeErr(w, http.StatusNotFound, "JobInstanceNotFound", "No such job instance.")
		return
	}
	loc := fmt.Sprintf("https://%s/v1/workspaces/%s/items/%s/jobs/instances/%s",
		r.Host, wid, r.PathValue("iid"), r.PathValue("jid"))
	w.Header().Set("Location", loc)
	w.WriteHeader(http.StatusAccepted)
}
