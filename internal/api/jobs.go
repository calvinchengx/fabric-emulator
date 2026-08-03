package api

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// startJob creates a job instance and drives whatever really executes for that
// item type. It is the single entry point every invoker shares — the on-demand
// API below, the native scheduler (schedules.go), and event triggers — so a
// scheduled DataPipeline runs the same interpreter a manually-started one
// does, and only invokeType tells them apart.
//
// executionData is the decoded `executionData` object of the request (or of
// the schedule/trigger that fired), left as a generic map because each item
// type reads different keys out of it.
func (a *API) startJob(wid string, it *store.Item, jobType, invokeType string, exec map[string]any) (*store.JobInstance, error) {
	delay, failWith := a.nextOpFate()
	j := &store.JobInstance{ItemID: it.ID, JobType: jobType, InvokeType: invokeType, FailWith: failWith}
	j.CompleteAt = a.Store.Now() + delay
	if it.Type == "ApacheAirflowJob" && jobType == "Run" {
		// A real engine callback finalises this job; virtual time must not.
		j.CompleteAt = math.MaxInt64
	}
	// A notebook is parsed BEFORE its job exists, because what the parse finds
	// decides whether the clock may complete the job at all.
	//
	// WHY: a job's status is otherwise derived purely from virtual time, so a
	// RunNotebook job reported "Completed" the moment its completion time
	// passed — with every cell still Pending and no engine having run a line.
	// A green job that means nothing is worse than a job stuck InProgress,
	// because only one of the two is believed.
	//
	// Cells outstanding => only the engine's callback can finish this job.
	// No cells (no definition, or nothing executable) => there is no work to
	// wait for, and the job completes now. That is not a loophole: a run with
	// nothing to execute is not waiting on an engine, and `notebookutils.
	// notebook.run` against such a notebook must still reach a terminal state.
	var nbRun *notebookRun
	if it.Type == "Notebook" && jobType == "RunNotebook" {
		run, code := a.parseNotebookRun(it)
		nbRun = &run
		if code != "" {
			j.FailWith = code
		} else if len(run.Cells) > 0 {
			j.CompleteAt = math.MaxInt64
		}
	}
	// A Spark job definition is the same story with no empty case: if it parses,
	// there is a main file for an engine to run, so the clock must not finish it.
	var sjdRun *sparkJobRun
	if it.Type == "SparkJobDefinition" && jobType == "sparkjob" {
		run, code := a.parseSparkJobRun(it)
		sjdRun = &run
		if code != "" {
			j.FailWith = code
		} else {
			j.CompleteAt = math.MaxInt64
		}
	}
	if err := a.Store.CreateJobInstance(j); err != nil {
		return nil, err
	}
	a.Store.PublishJobEvent(wid, it.ID, j.ID, jobType, invokeType, store.JobStarted, "")
	// Announce the outcome for the item types that reach one *now*. A generic
	// item's status is derived from the clock and never has such a moment, so
	// nothing further is claimed for it — the stream says only what happened.
	defer func() {
		if terminal := a.terminalStatusOf(it, jobType, j); terminal != "" {
			a.Store.PublishJobEvent(wid, it.ID, j.ID, jobType, invokeType, terminal, j.FailWith)
		}
	}()
	// DataPipeline jobs actually execute: the interpreter runs the definition's
	// control flow now and records the activity runs; a pipeline failure sets
	// the job's terminal status (overriding fault injection).
	if it.Type == "DataPipeline" {
		params, _ := exec["parameters"].(map[string]any)
		trigger, _ := exec["triggerEvent"].(map[string]any)
		if code := a.runPipelineWith(wid, it, j.ID, params, trigger); code != "" && j.FailWith == "" {
			j.FailWith = code
			_ = a.Store.SetJobFailure(it.ID, j.ID, code)
		}
	}
	// The parse happened above; record it against the job now that one exists.
	// A real Spark engine executes the cells and reports back to finalise the
	// run and the job's status (see notebooks.go).
	if nbRun != nil {
		a.saveNotebookRun(j.ID, *nbRun)
		if j.FailWith != "" {
			_ = a.Store.FinalizeJob(it.ID, j.ID, j.FailWith)
		}
	}
	if sjdRun != nil {
		a.saveSparkJobRun(j.ID, *sjdRun)
		if j.FailWith != "" {
			_ = a.Store.FinalizeJob(it.ID, j.ID, j.FailWith)
		}
	}
	if it.Type == "ApacheAirflowJob" && jobType == "Run" {
		dagID, _ := exec["dagId"].(string)
		conf, _ := exec["conf"].(map[string]any)
		if dagID == "" {
			j.FailWith = "AirflowDAGIDRequired"
			_ = a.Store.FinalizeJob(it.ID, j.ID, j.FailWith)
		} else if a.Airflow == nil {
			j.FailWith = "AirflowNotConfigured"
			_ = a.Store.FinalizeJob(it.ID, j.ID, j.FailWith)
		} else {
			go a.runAirflow(context.Background(), it, j, dagID, conf)
		}
	}
	if it.Type == "Dataflow" && (jobType == "Refresh" || jobType == "Publish") {
		j.FailWith = "DataflowEngineNotImplemented"
		_ = a.Store.FinalizeJob(it.ID, j.ID, j.FailWith)
	}
	return j, nil
}

// terminalStatusOf reports the status a job has already reached by the time
// startJob returns, or "" when its outcome is still clock-derived (a generic
// item) or awaiting an engine callback (a notebook, a Spark job, an Airflow
// DAG — each finalised later, by its own reporting path).
func (a *API) terminalStatusOf(it *store.Item, jobType string, j *store.JobInstance) string {
	executesNow := it.Type == "DataPipeline" ||
		(it.Type == "Dataflow" && (jobType == "Refresh" || jobType == "Publish"))
	if !executesNow {
		// A notebook or Spark job that failed to even start is terminal now.
		if j.FailWith != "" && (it.Type == "Notebook" || it.Type == "SparkJobDefinition") {
			return store.JobFailed
		}
		return ""
	}
	if j.FailWith != "" {
		return store.JobFailed
	}
	return store.JobCompleted
}

// publishJobOutcome announces a job's terminal state on the flow stream. Called
// where an engine reports back — the point a notebook, Spark job, or Airflow
// DAG actually finishes, which the clock cannot know.
func (a *API) publishJobOutcome(wid, itemID, jobID, failWith string) {
	status := store.JobCompleted
	if failWith != "" {
		status = store.JobFailed
	}
	j, err := a.Store.GetJobInstance(itemID, jobID)
	jobType, invokeType := "", ""
	if err == nil {
		jobType, invokeType = j.JobType, j.InvokeType
	}
	a.Store.PublishJobEvent(wid, itemID, jobID, jobType, invokeType, status, failWith)
}

// createJobInstance runs an item job on demand. 202 with Location pointing at
// the job instance, per the documented run-on-demand shape.
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
	var body struct {
		ExecutionData map[string]any `json:"executionData"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	j, err := a.startJob(wid, it, jobType, store.InvokeManual, body.ExecutionData)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	loc := fmt.Sprintf("https://%s/v1/workspaces/%s/items/%s/jobs/instances/%s", r.Host, wid, it.ID, j.ID)
	w.Header().Set("Location", loc)
	w.Header().Set("Retry-After", fmt.Sprintf("%d", a.RetryAfterSeconds))
	w.WriteHeader(http.StatusAccepted)
}

// listJobInstances is Fabric's **List Item Job Instances**. Schedules are
// evaluated first so a caller polling this endpoint sees runs that have come
// due — the emulator has no background worker by design (see schedules.go).
func (a *API) listJobInstances(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	if _, _, ok := a.requireRole(w, wid, p, store.RoleViewer); !ok {
		return
	}
	it, err := a.Store.GetItem(wid, r.PathValue("iid"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "ItemNotFound", "The item is not available.")
		return
	}
	a.tickItemSchedules(it.ID)
	jobs, err := a.Store.ListItemJobInstances(it.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	out := make([]map[string]any, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, a.jobBody(j, wid))
	}
	writePage(a, w, r, out)
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
