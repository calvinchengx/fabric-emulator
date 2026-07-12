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
	// chain is the stack of pipeline item IDs currently being invoked (the
	// outermost job's pipeline, then each nested Invoke pipeline). It guards
	// against invocation cycles and unbounded recursion.
	chain []string
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

	case "ExecutePipeline", "InvokePipeline":
		// Invoke pipeline: resolve the referenced DataPipeline and run it for
		// real through a fresh interpreter — recursive interpretation, the same
		// engine, one level deeper. waitOnCompletion (default true) gates this
		// activity on the child's terminal status.
		return e.invokePipelineActivity(act, tp, resolve)

	case "Copy":
		// A Copy whose source and sink are OneLake locations really moves the
		// bytes through the storage layer — no external engine needed. External
		// stores / format transformation are not in scope and fail loudly.
		return e.copyActivity(act, tp, resolve)

	case "Lookup":
		// Reads real rows from a CSV/JSON file in OneLake — hermetic, pure-Go,
		// its output flows into @activity('lk').output for downstream steps.
		return e.lookupActivity(act, tp, resolve)

	case "GetMetadata":
		// Stats a real OneLake path (exists / type / size / lastModified /
		// childItems) — the storage layer answers for real.
		return e.getMetadataActivity(act, tp, resolve)

	case "RefreshDataflow", "ExecuteDataFlow", "ExecutePowerQueryTemplate":
		// Dataflow Gen2 is the proprietary Power Query M engine — honestly
		// unimplemented (mirrors the Livy/Airflow stance), so the activity
		// fails loudly rather than pretending.
		return nil, fmt.Errorf("activity %q: Dataflow Gen2 (Power Query M) is not implemented in the emulator", act.Name)

	default:
		// Other leaf types (Web to arbitrary URLs, external connectors, SQL
		// scripts against an external engine) can't be executed hermetically —
		// firing real network calls breaks the offline/deterministic guarantee
		// and no SQL engine embeds under the pure-Go/no-CGO build. The emulator
		// records that the orchestration reached the leaf; the effect does not run.
		return map[string]any{"status": "Succeeded", "activityType": act.Type}, nil
	}
}

// maxInvokeDepth bounds nested Invoke pipeline recursion (a cycle is caught
// earlier by the chain check; this backstops a pathologically deep-but-acyclic
// nesting).
const maxInvokeDepth = 32

// invokePipelineActivity resolves the referenced DataPipeline, loads its
// definition, and runs it through a fresh interpreter sharing this executor's
// engines — real recursive interpretation. A cycle (the child already on the
// call stack) or excessive depth fails the activity rather than looping. With
// waitOnCompletion (the default), a child failure fails this activity too.
func (e *pipelineExecutor) invokePipelineActivity(act pipeline.Activity, tp map[string]json.RawMessage, resolve func(json.RawMessage) (any, error)) (map[string]any, error) {
	wsRef, ref, err := e.resolvePipelineRef(tp, resolve)
	if err != nil {
		return nil, fmt.Errorf("invoke pipeline %q: %w", act.Name, err)
	}
	child, err := e.resolvePipelineItem(wsRef, ref)
	if err != nil {
		return nil, fmt.Errorf("invoke pipeline %q: %w", act.Name, err)
	}

	for _, id := range e.chain {
		if id == child.ID {
			return nil, fmt.Errorf("invoke pipeline %q: cycle detected (pipeline %q is already running)", act.Name, child.DisplayName)
		}
	}
	if len(e.chain) >= maxInvokeDepth {
		return nil, fmt.Errorf("invoke pipeline %q: nesting deeper than %d not allowed", act.Name, maxInvokeDepth)
	}

	def, err := e.a.pipelineDefinition(child.ID)
	if err != nil {
		return nil, fmt.Errorf("invoke pipeline %q: %w", act.Name, err)
	}
	p, err := pipeline.Parse(def)
	if err != nil {
		return nil, fmt.Errorf("invoke pipeline %q: %w", act.Name, err)
	}

	params, err := e.resolveInvokeParams(tp, resolve)
	if err != nil {
		return nil, fmt.Errorf("invoke pipeline %q: %w", act.Name, err)
	}

	sub := &pipelineExecutor{a: e.a, wid: child.WorkspaceID, chain: append(append([]string{}, e.chain...), child.ID)}
	res := p.Run(params, sub)

	out := map[string]any{
		"pipelineName": child.DisplayName,
		"pipelineId":   child.ID,
		"status":       res.Status,
	}
	if e.waitOnCompletion(tp, resolve) && res.Status != pipeline.StatusSucceeded {
		return nil, fmt.Errorf("invoke pipeline %q: child pipeline %q failed: %s", act.Name, child.DisplayName, res.Error)
	}
	return out, nil
}

// resolvePipelineRef extracts the child pipeline reference (and optional
// workspace) from an Invoke pipeline's typeProperties. It accepts both the
// nested `pipeline.referenceName` shape and a flat `pipelineId`.
func (e *pipelineExecutor) resolvePipelineRef(tp map[string]json.RawMessage, resolve func(json.RawMessage) (any, error)) (wsRef, ref string, err error) {
	if raw, ok := tp["pipeline"]; ok {
		var pref struct {
			ReferenceName string `json:"referenceName"`
			WorkspaceID   string `json:"workspaceId"`
		}
		if json.Unmarshal(raw, &pref) == nil && pref.ReferenceName != "" {
			return pref.WorkspaceID, pref.ReferenceName, nil
		}
	}
	if raw, ok := tp["pipelineId"]; ok {
		v, err := resolve(raw)
		if err != nil {
			return "", "", err
		}
		if v != nil && fmt.Sprint(v) != "" {
			return "", fmt.Sprint(v), nil
		}
	}
	return "", "", fmt.Errorf("no pipeline reference (pipeline.referenceName or pipelineId)")
}

// resolvePipelineItem maps a pipeline reference (GUID or display name) in an
// optional workspace to a concrete DataPipeline item.
func (e *pipelineExecutor) resolvePipelineItem(wsRef, ref string) (*store.Item, error) {
	wsID := e.wid
	if wsRef != "" {
		if w, err := e.a.Store.GetWorkspace(wsRef); err == nil {
			wsID = w.ID
		} else if w, err := e.a.Store.GetWorkspaceByName(wsRef); err == nil {
			wsID = w.ID
		} else {
			return nil, fmt.Errorf("unknown workspace %q", wsRef)
		}
	}
	if it, err := e.a.Store.GetItem(wsID, ref); err == nil && it.Type == "DataPipeline" {
		return it, nil
	}
	if it, err := e.a.Store.GetItemByName(wsID, ref, "DataPipeline"); err == nil {
		return it, nil
	}
	return nil, fmt.Errorf("no DataPipeline %q in this workspace", ref)
}

// resolveInvokeParams evaluates the child pipeline's parameter values against
// the current scope. Each value may be a literal or an expression.
func (e *pipelineExecutor) resolveInvokeParams(tp map[string]json.RawMessage, resolve func(json.RawMessage) (any, error)) (map[string]any, error) {
	raw, ok := tp["parameters"]
	if !ok || len(raw) == 0 {
		return nil, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("parameters are not an object")
	}
	params := make(map[string]any, len(fields))
	for name, vraw := range fields {
		v, err := resolve(vraw)
		if err != nil {
			return nil, fmt.Errorf("parameter %q: %w", name, err)
		}
		params[name] = v
	}
	return params, nil
}

// waitOnCompletion reports whether the Invoke pipeline activity should block on
// the child's terminal status. Fabric's default is true.
func (e *pipelineExecutor) waitOnCompletion(tp map[string]json.RawMessage, resolve func(json.RawMessage) (any, error)) bool {
	raw, ok := tp["waitOnCompletion"]
	if !ok {
		return true
	}
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return b
	}
	if v, err := resolve(raw); err == nil && v != nil {
		return fmt.Sprint(v) == "true"
	}
	return true
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
	res := p.Run(params, &pipelineExecutor{a: a, wid: wid, chain: []string{it.ID}})
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
