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
// jobFailureMessage turns a failure code into something a caller can act on.
//
// Every failed job said "The job failed." A notebook shim surfaces this text
// as the exception a user reads, so a refusal with a specific, fixable cause
// arrived indistinguishable from a cell that threw. Codes without an entry
// keep the generic text — a vague message is a smaller problem than a
// confidently wrong one.
func jobFailureMessage(code string) string {
	switch code {
	case "NotebookLakehouseMismatch":
		return "The referenced notebook is bound to a different default lakehouse " +
			"than the notebook referencing it. Bind them to the same lakehouse, " +
			"remove the child's binding so it inherits, or pass " +
			"useRootDefaultLakehouse=True in the arguments to bypass this check."
	case "ComputeBindingInvalid":
		return "The notebook's compute binding does not resolve to an existing item."
	case "CopyJobCDCNotImplemented":
		return "This Copy job's jobMode is CDC, which needs change tracking on a " +
			"source the emulator cannot reach. Use jobMode Batch, or run against " +
			"real Fabric."
	case "CopyJobExternalSourceNotSupported":
		return "This Copy job's SOURCE is an external connection (SQL, Blob, …). " +
			"The emulator executes Lakehouse-to-Lakehouse legs only; external " +
			"stores need credentials and drivers it does not hold."
	case "CopyJobExternalDestinationNotSupported":
		return "This Copy job's DESTINATION is an external connection (SQL, " +
			"Blob, …). The emulator executes Lakehouse-to-Lakehouse legs only; " +
			"external stores need credentials and drivers it does not hold."
	case "CopyJobWriteBehaviorNotSupported":
		return "This Copy job's writeBehavior needs key-based reconciliation " +
			"(Merge/Upsert). Append and Overwrite execute for real; a Merge " +
			"quietly downgraded to Overwrite would destroy rows."
	}
	return "The job failed."
}

// referenceRunLakehouseCode enforces Fabric's reference-run lakehouse rule.
//
// Fabric allows a referenced child notebook to run "only if they use the same
// lakehouse as the parent, inherit the parent's lakehouse, or neither defines
// one. The execution is blocked if the child specifies a different lakehouse
// than the parent notebook. To bypass this check, set
// `useRootDefaultLakehouse: True` in the arguments."
//
// A REFUSAL, not a rebinding. The emulator ran the child against its own
// lakehouse and returned green, so a DAG whose child was bound to the wrong
// lakehouse — the mistake this rule exists to catch — passed locally and was
// blocked in production. Silently rebinding the child instead would be the
// same defect wearing the opposite mask: the run would go green having read
// data the author never pointed it at.
//
// `parentLakehouseId` is absent for a direct job submission, which is not a
// reference run and is not subject to the rule.
func referenceRunLakehouseCode(exec map[string]any, childLakehouse string) string {
	parent, _ := exec["parentLakehouseId"].(string)
	if parent == "" {
		return ""
	}
	if bypass, _ := exec["useRootDefaultLakehouse"].(bool); bypass {
		return ""
	}
	// No lakehouse of its own means it inherits; the same one is trivially fine.
	if childLakehouse == "" || childLakehouse == parent {
		return ""
	}
	return "NotebookLakehouseMismatch"
}

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
		if code == "" {
			code = referenceRunLakehouseCode(exec, run.Binding.LakehouseID)
		}
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
	// A pipeline's completion time is decided by its own execution, not the
	// clock — same contract as a notebook with cells outstanding. Parked
	// BEFORE create so no poll can ever observe a clock-completed pipeline
	// whose activities are still running.
	// A CopyJob's completion time is decided by the copy, for the same reason.
	// Bound once here because the dispatch below and this parking must agree:
	// parking without dispatching hangs the job forever, dispatching without
	// parking lets the clock call it Completed mid-copy.
	isCopyJobRun := it.Type == "CopyJob" && (jobType == "Execute" || jobType == "CopyJob")
	if it.Type == "DataPipeline" || isCopyJobRun {
		j.CompleteAt = math.MaxInt64
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
	// DataPipeline jobs actually execute — now OUTLIVING the request, like
	// every other job type (doc 37 §4 was the last inline one). The POST
	// returns 202 and the client polls; a fan-out of notebooks no longer holds
	// a socket open for its whole runtime, and the per-activity flow events
	// become readable while the pipeline is still running instead of arriving
	// in a burst after it finished. The goroutine finalises the job and
	// publishes the terminal event itself — the clock cannot know when a
	// pipeline finishes, so CompleteAt was parked at MaxInt64 before create.
	if it.Type == "DataPipeline" {
		params, _ := exec["parameters"].(map[string]any)
		trigger, _ := exec["triggerEvent"].(map[string]any)
		injected := j.FailWith // fault injection wins over the pipeline's own outcome
		go func() {
			code := a.runPipelineWith(wid, it, j.ID, params, trigger)
			if injected != "" {
				code = injected
			}
			_ = a.Store.FinalizeJob(it.ID, j.ID, code)
			a.publishJobOutcome(wid, it.ID, j.ID, code)
		}()
	}
	// The parse happened above; record it against the job now that one exists.
	// A real Spark engine executes the cells and reports back to finalise the
	// run and the job's status (see notebooks.go).
	if nbRun != nil {
		a.saveNotebookRun(j.ID, *nbRun)
		if j.FailWith != "" {
			_ = a.Store.FinalizeJob(it.ID, j.ID, j.FailWith)
		} else if len(nbRun.Cells) > 0 && a.runsNotebooksItself() {
			// Nobody else is coming. With a Spark agent configured the emulator
			// is the pool: it executes the cells and reports the same results an
			// external engine would post (notebookdrive.go). Without one the job
			// stays open for a callback, which is the original contract and the
			// only honest thing to do when there is no engine to run anything.
			nbParams, _ := exec["parameters"].(map[string]any)
			go a.driveNotebookRun(wid, it.ID, j.ID, *nbRun, nbParams)
		}
	}
	if sjdRun != nil {
		a.saveSparkJobRun(j.ID, *sjdRun)
		if j.FailWith != "" {
			_ = a.Store.FinalizeJob(it.ID, j.ID, j.FailWith)
		} else if a.runsNotebooksItself() {
			// Same reasoning as the notebook branch above: with an agent
			// configured the emulator IS the pool, and real Fabric runs a
			// submitted Spark job rather than handing it back. Without one the
			// job stays open for the external callback — the original contract,
			// and the only honest thing when there is no engine to run anything.
			go a.driveSparkJobRun(wid, it.ID, j.ID, *sjdRun)
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
	// A CopyJob really copies: the definition's Lakehouse legs run through the
	// pipeline Copy executor (copyjob.go). Run-on-demand is documented as
	// jobType=Execute — Microsoft's own readback example says "CopyJob", so
	// both spellings dispatch; what was submitted is what the instance keeps.
	//
	// ASYNC, and not merely to free the socket. Run inline, this job's status
	// stayed CLOCK-DERIVED: CompleteAt was `Now()+lroDelay`, so with any delay
	// configured the emulator copied the bytes during the POST and then
	// reported InProgress for the rest of the window — status contradicting the
	// filesystem, the notebook bug inverted (there: green with nothing done;
	// here: pending with everything done). The goroutine finalises, and
	// FinalizeJob sets complete_at = Now(), so the status follows the work.
	// CompleteAt is parked at MaxInt64 above for the same reason DataPipeline
	// parks: no poll may observe a clock-completed copy that is still running.
	if isCopyJobRun {
		injected := j.FailWith // fault injection wins over the copy's outcome
		go func() {
			code := a.runCopyJob(wid, it.ID, j.ID)
			if injected != "" {
				code = injected
			}
			_ = a.Store.FinalizeJob(it.ID, j.ID, code)
			a.publishJobOutcome(wid, it.ID, j.ID, code)
		}()
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
	// DataPipeline left this list when its execution went async (doc 37 §4):
	// listing it here reported "Completed" at POST time, which after that
	// change would be the same lie the notebook reconciliation was built to
	// kill — the goroutine publishes the real outcome via publishJobOutcome.
	// CopyJob left it too, for the same reason and one more: listing it here
	// published Completed at POST time, AND left CompleteAt clock-derived, so a
	// finished copy reported InProgress until virtual time caught up. Both
	// halves are the same defect — a status that consults the clock instead of
	// the work. Only Dataflow remains, whose refusal really is instantaneous.
	executesNow := it.Type == "Dataflow" && (jobType == "Refresh" || jobType == "Publish")
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
		body["failureReason"] = map[string]string{"errorCode": j.FailWith, "message": jobFailureMessage(j.FailWith)}
	}
	a.reconcileNotebookJobBody(j, body)
	return body
}

// reconcileNotebookJobBody keeps a RunNotebook job's status honest against the
// run detail, and says WHY when it failed.
//
// Two views of one execution can disagree: the job's status is derived from
// virtual time plus finalisation, while the per-cell truth lives in the notebook
// run. A job reporting Completed over a run whose cells never executed is the
// worst of the pair, because only the green one is believed — the same reasoning
// that made a definition-less notebook Failed rather than a fast success.
//
// So the run wins. If it says Failed, the job is Failed. If the run has not
// reached a terminal state, the job cannot claim Completed while its cells sit
// Pending. And a failure now names the cell and carries its error, instead of
// the bare "The job failed." that sent a reader looking for logs that do not
// exist (the detail is at .../jobs/instances/{jid}/notebookRun).
func (a *API) reconcileNotebookJobBody(j *store.JobInstance, body map[string]any) {
	if j.JobType != "RunNotebook" {
		return
	}
	_, runJSON, err := a.Store.GetNotebookRun(j.ID)
	if err != nil {
		return
	}
	var run notebookRun
	if json.Unmarshal([]byte(runJSON), &run) != nil {
		return
	}

	failedCell, failedErr := -1, ""
	pending := 0
	for _, c := range run.Cells {
		switch c.Status {
		case "Failed":
			if failedCell < 0 {
				failedCell, failedErr = c.Index, c.Error
			}
		case "", "Pending":
			pending++
		}
	}

	switch {
	case run.Status == "Failed" || failedCell >= 0:
		body["status"] = store.JobFailed
		msg := "The job failed."
		if failedCell >= 0 {
			msg = fmt.Sprintf("Cell %d failed: %s", failedCell, failedErr)
		}
		code, _ := body["failureReason"].(map[string]string)
		errorCode := "NotebookExecutionFailed"
		if code != nil && code["errorCode"] != "" {
			errorCode = code["errorCode"]
		}
		body["failureReason"] = map[string]string{
			"errorCode": errorCode,
			"message":   msg,
			"detail":    "Per-cell status, output and error: GET .../jobs/instances/" + j.ID + "/notebookRun",
		}
	case body["status"] == store.JobCompleted && run.Status != "Completed" && pending > 0:
		// The run was never finalised, yet the job went green. Note the test is
		// on the RUN's status, not on pending cells alone: a notebook that calls
		// notebook_exit stops early and legitimately leaves the cells after it
		// Pending, and that run IS Completed. What must not happen is a job
		// claiming Completed over a run no engine ever reported on, which is the
		// silent green this function exists to stop. InProgress is the honest
		// reading — a caller polling keeps polling instead of acting on a lie.
		body["status"] = store.JobInProgress
		delete(body, "endTimeUtc")
	}
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
