package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

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

	case "Copy":
		// A Copy whose source and sink are OneLake locations really moves the
		// bytes through the storage layer — no external engine needed. External
		// stores / format transformation are not in scope and fail loudly.
		return e.copyActivity(act, tp, resolve)

	case "RefreshDataflow", "ExecuteDataFlow", "ExecutePowerQueryTemplate":
		// Dataflow Gen2 is the proprietary Power Query M engine — honestly
		// unimplemented (mirrors the Livy/Airflow stance), so the activity
		// fails loudly rather than pretending.
		return nil, fmt.Errorf("activity %q: Dataflow Gen2 (Power Query M) is not implemented in the emulator", act.Name)

	default:
		// Other leaf types (Lookup over SQL, Web to arbitrary URLs, external
		// connectors) can't be executed hermetically — a SQL engine can't embed
		// under the pure-Go/no-CGO build, and firing real network calls breaks
		// the offline/deterministic guarantee. The emulator records that the
		// orchestration reached the leaf; the effect does not run.
		return map[string]any{"status": "Succeeded", "activityType": act.Type}, nil
	}
}

// oneLakeLoc is a resolved OneLake location — the workspace/item/path a Copy
// side reads from or writes to.
type oneLakeLoc struct {
	wsID, itemID, path string
}

// copyActivity performs a real OneLake→OneLake byte copy. Source and sink each
// carry a `location` object {workspaceId?, itemId, path} (workspaceId defaults
// to the pipeline's workspace; ids accept a GUID or a name). A file copies to
// the sink path; a directory copies its whole subtree under the sink path.
func (e *pipelineExecutor) copyActivity(act pipeline.Activity, tp map[string]json.RawMessage, resolve func(json.RawMessage) (any, error)) (map[string]any, error) {
	src, err := e.resolveLoc("source", tp["source"], resolve)
	if err != nil {
		return nil, fmt.Errorf("copy %q source: %w", act.Name, err)
	}
	dst, err := e.resolveLoc("sink", tp["sink"], resolve)
	if err != nil {
		return nil, fmt.Errorf("copy %q sink: %w", act.Name, err)
	}

	root, err := e.a.Store.GetOneLakePath(src.itemID, src.path)
	if err != nil {
		return nil, fmt.Errorf("copy %q: source %s not found", act.Name, src.path)
	}

	type file struct {
		rel     string
		content []byte
	}
	var files []file
	if !root.IsDir {
		files = append(files, file{dst.path, root.Content})
	} else {
		children, err := e.a.Store.ListOneLakePaths(src.itemID, src.path, true)
		if err != nil {
			return nil, fmt.Errorf("copy %q: %v", act.Name, err)
		}
		base := strings.TrimRight(dst.path, "/")
		for _, c := range children {
			if c.IsDir {
				continue
			}
			suffix := strings.TrimPrefix(c.RelPath, strings.TrimRight(src.path, "/"))
			files = append(files, file{base + suffix, c.Content})
		}
	}

	var bytesCopied int
	for _, f := range files {
		p := &store.OneLakePath{WorkspaceID: dst.wsID, ItemID: dst.itemID, RelPath: f.rel, Content: f.content}
		if err := e.a.Store.CreateOneLakePath(p, false); err != nil {
			return nil, fmt.Errorf("copy %q: writing %s: %v", act.Name, f.rel, err)
		}
		bytesCopied += len(f.content)
	}
	return map[string]any{
		"filesRead": len(files), "filesWritten": len(files),
		"dataRead": bytesCopied, "dataWritten": bytesCopied, "copyDuration": 0,
	}, nil
}

// resolveLoc reads a Copy side's OneLake location, resolving each field as an
// expression and mapping name-or-GUID references to concrete ids.
func (e *pipelineExecutor) resolveLoc(side string, raw json.RawMessage, resolve func(json.RawMessage) (any, error)) (oneLakeLoc, error) {
	var obj map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &obj) != nil {
		return oneLakeLoc{}, fmt.Errorf("missing %s", side)
	}
	loc := obj
	if l, ok := obj["location"]; ok {
		loc = map[string]json.RawMessage{}
		_ = json.Unmarshal(l, &loc)
	}
	field := func(k string) (string, error) {
		raw, ok := loc[k]
		if !ok {
			return "", nil
		}
		v, err := resolve(raw)
		if err != nil {
			return "", err
		}
		if v == nil {
			return "", nil
		}
		return fmt.Sprint(v), nil
	}
	wsRef, err := field("workspaceId")
	if err != nil {
		return oneLakeLoc{}, err
	}
	itemRef, err := field("itemId")
	if err != nil {
		return oneLakeLoc{}, err
	}
	path, err := field("path")
	if err != nil {
		return oneLakeLoc{}, err
	}
	if itemRef == "" || path == "" {
		return oneLakeLoc{}, fmt.Errorf("a OneLake location (itemId + path) is required")
	}
	wsID, itemID, err := e.resolveItemRef(wsRef, itemRef)
	if err != nil {
		return oneLakeLoc{}, err
	}
	return oneLakeLoc{wsID: wsID, itemID: itemID, path: path}, nil
}

// resolveItemRef maps a workspace/item reference (GUID or name) to ids; an
// empty workspace defaults to the pipeline's own workspace.
func (e *pipelineExecutor) resolveItemRef(wsRef, itemRef string) (wsID, itemID string, err error) {
	wsID = e.wid
	if wsRef != "" {
		if w, e2 := e.a.Store.GetWorkspace(wsRef); e2 == nil {
			wsID = w.ID
		} else if w, e2 := e.a.Store.GetWorkspaceByName(wsRef); e2 == nil {
			wsID = w.ID
		} else {
			return "", "", fmt.Errorf("unknown workspace %q", wsRef)
		}
	}
	if it, e2 := e.a.Store.GetItem(wsID, itemRef); e2 == nil {
		return wsID, it.ID, nil
	}
	if i := strings.LastIndex(itemRef, "."); i > 0 {
		if it, e2 := e.a.Store.GetItemByName(wsID, itemRef[:i], itemRef[i+1:]); e2 == nil {
			return wsID, it.ID, nil
		}
	}
	return "", "", fmt.Errorf("unknown item %q", itemRef)
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
