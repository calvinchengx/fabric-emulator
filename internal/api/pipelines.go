package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/pipeline"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// pipelineExecutor bridges the interpreter's leaf activities to real engines.
// The interpreter itself runs all control flow, variables, and expressions;
// this only handles the "call an engine" activities — notably chaining a
// notebook activity to the same jobs engine R4 notebooks run on.
type pipelineExecutor struct {
	a   *API
	wid string
}

func (e *pipelineExecutor) Execute(act pipeline.Activity, resolve func(json.RawMessage) (any, error)) (map[string]any, error) {
	tp := map[string]json.RawMessage{}
	_ = json.Unmarshal(act.TypeProperties, &tp)

	switch act.Type {
	case "TridentNotebook", "SynapseNotebook", "RunNotebook":
		// Resolve the referenced notebook and submit a real RunNotebook job —
		// the pipeline → jobs → notebook chain, end to end.
		idv, err := resolve(tp["notebookId"])
		if err != nil || idv == nil || fmt.Sprint(idv) == "" {
			return nil, fmt.Errorf("notebook activity %q: notebookId is required", act.Name)
		}
		nbID := fmt.Sprint(idv)
		nb, err := e.a.Store.GetItem(e.wid, nbID)
		if err != nil || nb.Type != "Notebook" {
			return nil, fmt.Errorf("notebook activity %q: no notebook %q in this workspace", act.Name, nbID)
		}
		j := &store.JobInstance{ItemID: nb.ID, JobType: "RunNotebook", InvokeType: "Pipeline"}
		j.CompleteAt = e.a.Store.Now()
		if err := e.a.Store.CreateJobInstance(j); err != nil {
			return nil, fmt.Errorf("notebook activity %q: %v", act.Name, err)
		}
		return map[string]any{"jobInstanceId": j.ID, "notebookId": nb.ID, "status": "Completed"}, nil

	case "RefreshDataflow", "ExecuteDataFlow", "ExecutePowerQueryTemplate":
		// Dataflow Gen2 is the proprietary Power Query M engine — honestly
		// unimplemented (mirrors the Livy/Airflow stance), so the activity
		// fails loudly rather than pretending.
		return nil, fmt.Errorf("activity %q: Dataflow Gen2 (Power Query M) is not implemented in the emulator", act.Name)

	default:
		// Control-plane-only leaf (Copy, WebActivity, Lookup, SetVariable-like
		// externals): the emulator records success. Storage effects are proven
		// by the OneLake e2es; here we prove the orchestration reached the leaf.
		return map[string]any{"status": "Succeeded", "activityType": act.Type}, nil
	}
}

// pipelineDefinition extracts and decodes the pipeline-content.json payload
// from an item's stored definition parts.
func (a *API) pipelineDefinition(itemID string) ([]byte, error) {
	parts, err := a.Store.GetDefinition(itemID)
	if err != nil {
		return nil, err
	}
	var payload string
	for _, p := range parts {
		if p.Path == "pipeline-content.json" {
			payload = p.Payload
			break
		}
	}
	if payload == "" && len(parts) > 0 {
		payload = parts[0].Payload // fall back to the sole part
	}
	if payload == "" {
		return nil, fmt.Errorf("no pipeline definition")
	}
	return base64.StdEncoding.DecodeString(payload)
}

// runPipeline parses the definition, executes it, and persists the activity
// runs against the job. It returns a failure code ("" on success) used to set
// the job's terminal status.
func (a *API) runPipeline(wid string, it *store.Item, jobID string, params map[string]any) string {
	def, err := a.pipelineDefinition(it.ID)
	if err != nil {
		a.savePipelineRun(jobID, pipeline.StatusFailed, nil)
		return "PipelineDefinitionInvalid"
	}
	p, err := pipeline.Parse(def)
	if err != nil {
		a.savePipelineRun(jobID, pipeline.StatusFailed, nil)
		return "PipelineDefinitionInvalid"
	}
	res := p.Run(params, &pipelineExecutor{a: a, wid: wid})
	a.savePipelineRun(jobID, res.Status, res.Activities)
	if res.Status != pipeline.StatusSucceeded {
		return "PipelineActivityFailed"
	}
	return ""
}

func (a *API) savePipelineRun(jobID, status string, activities []pipeline.ActivityRun) {
	if activities == nil {
		activities = []pipeline.ActivityRun{}
	}
	blob, _ := json.Marshal(activities)
	_ = a.Store.SetPipelineRun(jobID, status, string(blob))
}

// queryActivityRuns returns the interpreter's per-activity run detail for a
// pipeline job — the shape the Fabric "Query activity runs" API returns.
func (a *API) queryActivityRuns(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	if _, _, ok := a.requireRole(w, wid, p, store.RoleViewer); !ok {
		return
	}
	if _, err := a.Store.GetJobInstance(r.PathValue("iid"), r.PathValue("jid")); err != nil {
		writeErr(w, http.StatusNotFound, "JobInstanceNotFound", "No such job instance.")
		return
	}
	status, runsJSON, err := a.Store.GetPipelineRun(r.PathValue("jid"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "PipelineRunNotFound", "This job has no pipeline run detail.")
		return
	}
	var runs []json.RawMessage
	_ = json.Unmarshal([]byte(runsJSON), &runs)
	if runs == nil {
		runs = []json.RawMessage{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": status, "value": runs})
}
