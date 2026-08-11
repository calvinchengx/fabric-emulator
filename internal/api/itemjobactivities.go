package api

import (
	"encoding/json"
	"fmt"
	"math"

	"github.com/calvinchengx/fabric-emulator/internal/pipeline"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// The two Fabric activities that invoke another ITEM's job rather than doing
// the work inline: SparkJobDefinition and InvokeCopyJob.
//
// Both were refused by name in #180 with a cause that said, in as many words,
// "THE EMULATOR CAN DO THIS" — the item types already execute through the jobs
// API, and only the activity-side wiring was missing. This is that wiring, so
// those two refusals are now removed and the guard test demands it.
//
// ORACLE for the typeProperties below: Fabric's DataPipeline definition
// article, the same table that produced fabricactivitytypes.go —
// SparkJobDefinition takes `sparkJobDefinitionId` + `workspaceId` (both
// required), InvokeCopyJob takes `copyJobId` + `workspaceId` (both required).
// Nothing here is guessed; the optional SJD overrides are named there too and
// are deliberately NOT implemented yet, which is stated where it matters.
//
// Both activities are SYNCHRONOUS, as Fabric's are: the pipeline gates on the
// item's outcome. That is why the run is driven in this goroutine rather than
// dispatched like a POST to jobs/instances — an activity that returned before
// its job finished would let the next activity read outputs that do not exist.

// fabricItemRef resolves the {workspaceId, <id>} pair these activities carry.
//
// The zero GUID is Fabric's own sentinel for "this pipeline's workspace" — it
// is what a same-workspace reference carries in Git — so it must resolve to the
// running workspace rather than be looked up literally. Same rule the notebook
// activity already follows; getting it wrong turns a legitimate reference into
// "no such item", blaming the id for a value that was correct.
func (e *pipelineExecutor) fabricItemRef(
	act pipeline.Activity,
	tp map[string]json.RawMessage,
	resolve func(json.RawMessage) (any, error),
	idKey string,
) (*store.Item, error) {
	idv, err := resolve(tp[idKey])
	if err != nil || idv == nil || fmt.Sprint(idv) == "" {
		return nil, fmt.Errorf("activity %q: %s is required", act.Name, idKey)
	}
	itemRef := fmt.Sprint(idv)

	wsRef := ""
	if raw, ok := tp["workspaceId"]; ok && len(raw) > 0 {
		wv, werr := resolve(raw)
		if werr != nil {
			return nil, fmt.Errorf("activity %q: workspaceId: %v", act.Name, werr)
		}
		if s := fmt.Sprint(wv); wv != nil && s != "" && s != "00000000-0000-0000-0000-000000000000" {
			wsRef = s
		}
	}
	wsID, itemID, err := e.resolveItemRef(wsRef, itemRef)
	if err != nil {
		return nil, fmt.Errorf("activity %q: %v", act.Name, err)
	}
	it, err := e.a.Store.GetItem(wsID, itemID)
	if err != nil {
		return nil, fmt.Errorf("activity %q: no item %q in workspace %q", act.Name, itemRef, wsID)
	}
	return it, nil
}

// sparkJobDefinitionActivity runs a Spark Job Definition item and gates the
// pipeline on its outcome.
func (e *pipelineExecutor) sparkJobDefinitionActivity(
	act pipeline.Activity,
	tp map[string]json.RawMessage,
	resolve func(json.RawMessage) (any, error),
) (map[string]any, error) {
	it, err := e.fabricItemRef(act, tp, resolve, "sparkJobDefinitionId")
	if err != nil {
		return nil, err
	}
	if it.Type != "SparkJobDefinition" {
		return nil, fmt.Errorf("Spark job definition activity %q: item %q is a %s, not a SparkJobDefinition",
			act.Name, it.DisplayName, it.Type)
	}

	// The parse decides the job's completion time, exactly as jobs.go does it:
	// a job the clock completes reports Completed while the engine has run
	// nothing. Parked BEFORE create so no poll can observe the wrong answer.
	run, code := e.a.parseSparkJobRun(it)
	j := &store.JobInstance{ItemID: it.ID, JobType: "sparkjob", InvokeType: "Pipeline"}
	j.CompleteAt = e.a.Store.Now()
	if code == "" {
		j.CompleteAt = math.MaxInt64
	}
	if err := e.a.Store.CreateJobInstance(j); err != nil {
		return nil, fmt.Errorf("Spark job definition activity %q: %v", act.Name, err)
	}
	e.a.saveSparkJobRun(j.ID, run)
	if code != "" {
		_ = e.a.Store.FinalizeJob(it.ID, j.ID, code)
		return nil, fmt.Errorf("Spark job definition activity %q: %s", act.Name, code)
	}

	out := map[string]any{
		"jobId":                j.ID,
		"sparkJobDefinitionId": it.ID,
		"workspaceId":          it.WorkspaceID,
	}
	if !e.a.runsNotebooksItself() {
		// No engine attached. The job stays open for an external one to report,
		// which is the same contract a direct submit gets — and saying
		// "Pending" is the only honest answer when nothing here can execute the
		// job. It is NOT reported as a success: a pipeline branching on this
		// would otherwise proceed as though the Spark job had finished.
		out["status"] = "Pending"
		return out, fmt.Errorf("Spark job definition activity %q: no Spark engine is attached, so the "+
			"job was submitted and left Pending for an external engine rather than reported as run. "+
			"Attach an agent (FABRIC_SPARK_AGENT_URL) to execute it here", act.Name)
	}

	// With an agent the emulator IS the pool, so drive the run HERE — in this
	// goroutine, because the activity must not report before the job finishes.
	e.a.driveSparkJobRun(it.WorkspaceID, it.ID, j.ID, run)

	jb, err := e.a.Store.GetJobInstance(it.ID, j.ID)
	if err != nil {
		return nil, fmt.Errorf("Spark job definition activity %q: job detail lost: %v", act.Name, err)
	}
	if jb.FailWith != "" {
		return nil, fmt.Errorf("Spark job definition activity %q: %s", act.Name, jb.FailWith)
	}
	out["status"] = "Completed"
	return out, nil
}

// invokeCopyJobActivity runs a Copy job item and gates the pipeline on its
// outcome.
func (e *pipelineExecutor) invokeCopyJobActivity(
	act pipeline.Activity,
	tp map[string]json.RawMessage,
	resolve func(json.RawMessage) (any, error),
) (map[string]any, error) {
	it, err := e.fabricItemRef(act, tp, resolve, "copyJobId")
	if err != nil {
		return nil, err
	}
	if it.Type != "CopyJob" {
		return nil, fmt.Errorf("invoke copy job %q: item %q is a %s, not a CopyJob",
			act.Name, it.DisplayName, it.Type)
	}

	// A copy job's completion is decided by the copy, never the clock — the
	// same reasoning jobs.go parks CompleteAt for.
	j := &store.JobInstance{ItemID: it.ID, JobType: "Execute", InvokeType: "Pipeline"}
	j.CompleteAt = math.MaxInt64
	if err := e.a.Store.CreateJobInstance(j); err != nil {
		return nil, fmt.Errorf("invoke copy job %q: %v", act.Name, err)
	}

	// Run it in this goroutine: the activity gates on the copy, so returning
	// before the bytes moved would let the next activity read a sink that is
	// not there yet. runCopyJob returns a failure code, "" on success.
	code := e.a.runCopyJob(it.WorkspaceID, it.ID, j.ID)
	_ = e.a.Store.FinalizeJob(it.ID, j.ID, code)
	e.a.publishJobOutcome(it.WorkspaceID, it.ID, j.ID, code)
	if code != "" {
		return nil, fmt.Errorf("invoke copy job %q: copy job %q failed: %s", act.Name, it.DisplayName, code)
	}
	return map[string]any{
		"jobId":       j.ID,
		"copyJobId":   it.ID,
		"workspaceId": it.WorkspaceID,
		"status":      "Completed",
	}, nil
}
