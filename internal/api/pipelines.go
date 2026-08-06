package api

import (
	"bytes"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"path"
	"slices"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/pipeline"
	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/calvinchengx/fabric-emulator/internal/warehouse"
)

// pipelineExecutor bridges the interpreter's leaf activities to real engines.
// The interpreter itself runs all control flow, variables, and expressions;
// this only handles the "call an engine" activities — notably chaining a
// notebook activity to the same jobs engine R4 notebooks run on.
type pipelineExecutor struct {
	a     *API
	wid   string
	jobID string
	// chain is the stack of pipeline item IDs currently being invoked (the
	// outermost job's pipeline, then each nested Invoke pipeline). It guards
	// against invocation cycles and unbounded recursion.
	chain []string
}

// notebookActivityOutput builds the notebook activity's `output` object.
//
// Fabric publishes no schema for it. The REST definition specifies the
// activity's INPUT, and the only published output sample belongs to the Synapse
// ancestor, so that sample is the closest thing to a specification that exists
// and is what this mirrors: result.{runId, runStatus, sessionId, exitCode}.
//
// exitValue rides alongside exitCode because the sources disagree on the name
// and real pipelines are written against both: Synapse's prose and Fabric's
// portal say exitValue, while Synapse's own output sample names the field
// exitCode. Emitting both means either expression resolves, instead of betting
// the run on one spelling. jobInstanceId is an emulator extension, kept because
// it correlates the activity to the job the emulator created and nothing in
// Fabric's output does. An extra field costs nothing (an expression that does
// not name it cannot see it); a missing documented one breaks a real pipeline.
//
// sessionID is empty when no engine ran the notebook, and then no sessionId is
// reported at all, because there was no session.
func notebookActivityOutput(jobID, notebookID, status, exitValue, sessionID string) map[string]any {
	result := map[string]any{
		"runId":     jobID,
		"runStatus": status,
		"exitCode":  exitValue,
		"exitValue": exitValue,
	}
	if sessionID != "" {
		result["sessionId"] = sessionID
	}
	return map[string]any{
		"status":        status,
		"notebookId":    notebookID,
		"jobInstanceId": jobID,
		"result":        result,
	}
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
		// workspaceId says WHICH workspace holds the notebook, and Fabric marks
		// it required alongside notebookId precisely because the notebook need
		// not live beside the pipeline. Ignoring it and always reading the
		// pipeline's own workspace turns a legitimate cross-workspace activity
		// into "no notebook %q in this workspace", which blames the id for a
		// property that was in fact supplied and correct. Absent, it defaults to
		// the pipeline's workspace, which is the single-workspace shape.
		nbWID := e.wid
		if raw, ok := tp["workspaceId"]; ok && len(raw) > 0 {
			wv, werr := resolve(raw)
			if werr != nil {
				return nil, fmt.Errorf("notebook activity %q: workspaceId: %v", act.Name, werr)
			}
			// The zero GUID is Fabric's own sentinel for "this pipeline's
			// workspace" (it is what a same-workspace activity carries in Git),
			// so it must resolve to e.wid rather than be looked up literally.
			if s := fmt.Sprint(wv); wv != nil && s != "" && s != "00000000-0000-0000-0000-000000000000" {
				nbWID = s
			}
		}
		nb, err := e.a.Store.GetItem(nbWID, nbID)
		if err != nil || nb.Type != "Notebook" {
			if nbWID != e.wid {
				return nil, fmt.Errorf("notebook activity %q: no notebook %q in workspace %q", act.Name, nbID, nbWID)
			}
			return nil, fmt.Errorf("notebook activity %q: no notebook %q in this workspace", act.Name, nbID)
		}
		// The activity's parameters become the run's parameter overrides — the
		// whole point of a pipeline driving a parameterised notebook. Fabric's
		// shape is {name: {value, type}}; the value may itself be an
		// expression (@pipeline().parameters.X), so it resolves against the
		// pipeline scope before it is handed to the notebook.
		nbParams := map[string]any{}
		if raw, ok := tp["parameters"]; ok && len(raw) > 0 {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				return nil, fmt.Errorf("notebook activity %q: parameters are not an object", act.Name)
			}
			for name, vraw := range fields {
				target := vraw
				var pv struct {
					Value json.RawMessage `json:"value"`
				}
				if json.Unmarshal(vraw, &pv) == nil && len(pv.Value) > 0 {
					target = pv.Value
				}
				v, err := resolve(target)
				if err != nil {
					return nil, fmt.Errorf("notebook activity %q parameter %q: %v", act.Name, name, err)
				}
				// Fabric takes "simple types such as int, float, bool, and
				// string"; "complex types such as list and dict aren't yet
				// supported". Rendering one anyway would be the emulator being
				// MORE permissive than the thing it emulates, which is the one
				// direction that actively misleads: the pipeline passes here and
				// fails in Fabric, so the emulator has certified something it
				// cannot vouch for. Refuse it here, where the activity contract
				// lives, before the notebook runs.
				switch v.(type) {
				case []any, map[string]any:
					return nil, fmt.Errorf(
						"notebook activity %q parameter %q: Fabric notebook parameters support only int, float, bool and string; list and dict are not supported",
						act.Name, name)
				}
				nbParams[name] = v
			}
		}
		// Parse for real, exactly as a direct RunNotebook job POST does
		// (jobs.go): the Go parser splits the notebook into cells and the
		// compute binding is resolved, so an engine can execute it and report
		// back. Without this the activity fabricated a completed job with no
		// run behind it — nothing to execute, and nothing for lineage or the
		// notebookRunResult callback to attach to.
		//
		// The parse comes FIRST because it decides the job's completion time,
		// for the reason spelled out in jobs.go: a job the clock completes says
		// "Completed" while every cell is still Pending. Cells outstanding =>
		// only the engine finishes this job.
		run, code := e.a.parseNotebookRun(nb)
		j := &store.JobInstance{ItemID: nb.ID, JobType: "RunNotebook", InvokeType: "Pipeline"}
		j.CompleteAt = e.a.Store.Now()
		if code == "" && len(run.Cells) > 0 {
			j.CompleteAt = math.MaxInt64
		}
		if err := e.a.Store.CreateJobInstance(j); err != nil {
			return nil, fmt.Errorf("notebook activity %q: %v", act.Name, err)
		}
		e.a.saveNotebookRun(j.ID, run)
		if code != "" {
			_ = e.a.Store.FinalizeJob(nb.ID, j.ID, code)
			return nil, fmt.Errorf("notebook activity %q: %s", act.Name, code)
		}
		// Fabric's notebook activity is SYNCHRONOUS: the pipeline gates on the
		// notebook's outcome. With a Spark agent configured the emulator is the
		// pool (same contract as jobs.go), so drive the run here — in this
		// goroutine, because the activity must not report before the notebook
		// finishes — and let the activity succeed or fail on the run's actual
		// terminal state. Without an agent the run stays Pending for an
		// external engine's callback, which is the original contract and the
		// only honest answer when nothing can execute the cells.
		if e.a.runsNotebooksItself() && len(run.Cells) > 0 {
			e.a.driveNotebookRun(nbWID, nb.ID, j.ID, run, nbParams)
			status, runJSON, err := e.a.Store.GetNotebookRun(j.ID)
			if err != nil {
				return nil, fmt.Errorf("notebook activity %q: run detail lost: %v", act.Name, err)
			}
			var detail struct {
				ExitValue string `json:"exitValue"`
			}
			_ = json.Unmarshal([]byte(runJSON), &detail)
			if status != "Completed" {
				if jb, err := e.a.Store.GetJobInstance(nb.ID, j.ID); err == nil && jb.FailWith != "" {
					return nil, fmt.Errorf("notebook activity %q: %s", act.Name, jb.FailWith)
				}
				return nil, fmt.Errorf("notebook activity %q: notebook run ended %s", act.Name, status)
			}
			return notebookActivityOutput(j.ID, nb.ID, status, detail.ExitValue, notebookSessionID(j.ID)), nil
		}
		// No engine: the run is Pending until one executes the cells and
		// reports back; say that rather than claiming a completion that has
		// not happened.
		status, _, err := e.a.Store.GetNotebookRun(j.ID)
		if err != nil || status == "" {
			status = "Pending"
		}
		return notebookActivityOutput(j.ID, nb.ID, status, "", ""), nil

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

	case "Script":
		// Runs real T-SQL scripts against a Warehouse/SQLDatabase item's own SQL
		// Server database — real rows/rowcounts back, when a warehouse SQL
		// backend is attached; an honest error otherwise.
		return e.scriptActivity(act, tp, resolve)

	case "SqlServerStoredProcedure":
		// Calls a real stored procedure on a Warehouse/SQLDatabase item's own
		// database, same backend as Script.
		return e.storedProcedureActivity(act, tp, resolve)

	case "RefreshDataflow", "ExecuteDataFlow", "ExecutePowerQueryTemplate":
		// Dataflow Gen2 is the proprietary Power Query M engine — honestly
		// unimplemented (mirrors the Livy/Airflow stance), so the activity
		// fails loudly rather than pretending.
		return nil, fmt.Errorf("activity %q: Dataflow Gen2 (Power Query M) is not implemented in the emulator", act.Name)

	case "WebActivity", "Web", "WebHook":
		return e.webActivity(act, tp, resolve)

	default:
		// External connectors only: a Salesforce or ServiceNow leaf needs a
		// vendor SDK and credentials the emulator has neither of, so it records
		// that the orchestration reached the leaf without claiming the effect
		// ran. Web used to be swept in here too, which meant a pipeline
		// branching on a response got a fabricated success — see
		// webactivity.go.
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

	sub := &pipelineExecutor{a: e.a, wid: child.WorkspaceID, jobID: e.jobID, chain: append(append([]string{}, e.chain...), child.ID)}
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

	// A Tables/<name> sink is a Delta table, not a folder of bytes: read the
	// source's rows and commit them, so Append really appends and Overwrite
	// really replaces. Falls through to the byte copy for Files/ sinks and for
	// sources this cannot parse.
	if name, ok := deltaTableName(dst.path); ok {
		out, handled, err := e.copyIntoTable(act, tp, src, dst, name, resolve)
		if err != nil {
			return nil, err
		}
		if handled {
			return out, nil
		}
	}

	root, err := e.a.Store.GetOneLakePath(src.itemID, src.path)
	if err != nil {
		// ADLS parent directories may be implicit: Delta writers commonly create
		// only files below Tables/<name>. A nonempty prefix is therefore a real
		// directory source even when it has no standalone metadata row.
		children, listErr := e.a.Store.ListOneLakePaths(src.itemID, src.path, true)
		if listErr != nil || len(children) == 0 {
			return nil, fmt.Errorf("copy %q: source %s not found", act.Name, src.path)
		}
		root = &store.OneLakePath{ItemID: src.itemID, RelPath: src.path, IsDir: true}
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
		if err := e.a.Store.CreateOneLakePathAs(store.ActivityBy(e.jobID, act.Name), p, false); err != nil {
			return nil, fmt.Errorf("copy %q: writing %s: %v", act.Name, f.rel, err)
		}
		bytesCopied += len(f.content)
	}
	// Producer is STATED, not defaulted. CreateLineageEdge fills an empty
	// producer with Copy as a backstop, but Copy asserts the emulator
	// WATCHED the bytes move — so a caller that merely forgot would be
	// claiming evidence it never had. These are the sites that really are
	// Copy, and saying so keeps the backstop a backstop.
	edge := &store.LineageEdge{WorkspaceID: e.wid, JobID: e.jobID, ActivityName: act.Name,
		Producer:          store.ProducerCopy,
		SourceWorkspaceID: src.wsID, SourceItemID: src.itemID, SourcePath: src.path,
		TargetWorkspaceID: dst.wsID, TargetItemID: dst.itemID, TargetPath: dst.path}
	if err := e.a.Store.CreateLineageEdge(edge); err != nil {
		return nil, fmt.Errorf("copy %q lineage: %v", act.Name, err)
	}
	return map[string]any{
		"filesRead": len(files), "filesWritten": len(files),
		"dataRead": bytesCopied, "dataWritten": bytesCopied, "copyDuration": 0,
		"lineage": edge,
	}, nil
}

// copySideTypes are the Copy source/sink `type` discriminators the emulator can
// honour. Everything Fabric supports beyond this — external connectors, formats
// with no reader here — is rejected by name rather than silently treated as an
// opaque OneLake byte copy, which would be the one behaviour worse than a 501.
var copySideTypes = map[string]bool{
	"":                     true, // the emulator's own simplified shape
	"LakehouseTableSource": true, "LakehouseTableSink": true,
	"LakehouseReadSettings": true, "LakehouseWriteSettings": true,
	"BinarySource": true, "BinarySink": true,
	"DelimitedTextSource": true, "DelimitedTextSink": true,
	"ParquetSource": true, "ParquetSink": true,
	"JsonSource": true, "JsonSink": true,
}

// copyUnsupportedOpts are Copy options the emulator parses but cannot honour
// yet. Accepting a payload while ignoring these would silently do the wrong
// thing — an Upsert that appends, a wildcard that copies one file — so each
// fails loudly, naming itself.
var copyUnsupportedOpts = []struct{ key, why string }{
	{"sqlReaderQuery", "reading a Lakehouse table through a T-SQL query is not implemented"},
	{"wildcardFolderPath", "wildcard paths are not implemented"},
	{"wildcardFileName", "wildcard paths are not implemented"},
	{"fileListPath", "list-of-files sources are not implemented"},
	{"versionAsOf", "Delta time travel is not implemented"},
	{"timestampAsOf", "Delta time travel is not implemented"},
	{"partitionOption", "partitioned copy is not implemented"},
	{"keyColumns", "Upsert into a Lakehouse table is not implemented"},
}

// copyAllowedValues gates options the emulator honours for *some* values only.
// Append/Overwrite are real for a Tables/<name> sink (the rows are committed to
// the Delta log); Upsert needs row matching we do not do, and MergeFiles /
// FlattenHierarchy reshape output we never reshape. Compared case-insensitively.
var copyAllowedValues = map[string][]string{
	"tableActionOption": {"Append", "Overwrite", "OverwriteSchema"},
	"copyBehavior":      {"PreserveHierarchy"},
}

// resolveLoc reads a Copy side and resolves it to a OneLake location.
//
// Three shapes are accepted, so a pipeline authored in Fabric runs unchanged:
//
//  1. Fabric's script properties on the side itself — `rootFolder` (Tables or
//     Files) with `table`/`schema`, or `folderPath`/`fileName`, plus the
//     connection's `workspaceId`/`itemId`.
//  2. The Fabric UI's nested dataset shape —
//     `datasetSettings.typeProperties.{location,table,schema}` with the
//     lakehouse identified by `datasetSettings.linkedService.properties.
//     typeProperties.{workspaceId,artifactId}`.
//  3. The emulator's original simplified shape — `location.{workspaceId,itemId,
//     path}` — which existing pipelines and e2e suites use.
//
// Every field resolves as an expression first, so @pipeline().parameters work
// throughout.
func (e *pipelineExecutor) resolveLoc(side string, raw json.RawMessage, resolve func(json.RawMessage) (any, error)) (oneLakeLoc, error) {
	var obj map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &obj) != nil {
		return oneLakeLoc{}, fmt.Errorf("missing %s", side)
	}

	// Flatten the shapes into one lookup, innermost last so an explicit nested
	// value wins over an outer one.
	scopes := []map[string]json.RawMessage{obj}
	descend := func(m map[string]json.RawMessage, keys ...string) map[string]json.RawMessage {
		cur := m
		for _, k := range keys {
			v, ok := cur[k]
			if !ok {
				return nil
			}
			next := map[string]json.RawMessage{}
			if json.Unmarshal(v, &next) != nil {
				return nil
			}
			cur = next
		}
		return cur
	}
	if ds := descend(obj, "datasetSettings"); ds != nil {
		scopes = append(scopes, ds)
		if ls := descend(ds, "linkedService", "properties", "typeProperties"); ls != nil {
			scopes = append(scopes, ls)
		}
		if tp := descend(ds, "typeProperties"); tp != nil {
			scopes = append(scopes, tp)
			if loc := descend(tp, "location"); loc != nil {
				scopes = append(scopes, loc)
			}
		}
	}
	if loc := descend(obj, "location"); loc != nil {
		scopes = append(scopes, loc)
	}
	if st := descend(obj, "storeSettings"); st != nil {
		scopes = append(scopes, st)
	}

	lookup := func(k string) (json.RawMessage, bool) {
		for i := len(scopes) - 1; i >= 0; i-- {
			if v, ok := scopes[i][k]; ok {
				return v, true
			}
		}
		return nil, false
	}
	field := func(k string) (string, error) {
		raw, ok := lookup(k)
		if !ok {
			return "", nil
		}
		v, err := resolve(raw)
		if err != nil || v == nil {
			return "", err
		}
		return fmt.Sprint(v), nil
	}

	// The discriminator is the side's own `type`, never an inner one: nested
	// objects carry their own (`datasetSettings.type` is a *dataset* type like
	// "LakehouseTable", `location.type` a store type like "LakehouseLocation").
	sideType := ""
	if raw, ok := obj["type"]; ok {
		v, err := resolve(raw)
		if err != nil {
			return oneLakeLoc{}, err
		}
		if v != nil {
			sideType = fmt.Sprint(v)
		}
	}
	if !copySideTypes[sideType] {
		return oneLakeLoc{}, fmt.Errorf("%s type %q is not supported by the emulator", side, sideType)
	}
	for _, opt := range copyUnsupportedOpts {
		if _, ok := lookup(opt.key); ok {
			return oneLakeLoc{}, fmt.Errorf("%s option %q: %s", side, opt.key, opt.why)
		}
	}
	for key, allowed := range copyAllowedValues {
		got, err := field(key)
		if err != nil {
			return oneLakeLoc{}, err
		}
		if got == "" {
			continue
		}
		if !slices.ContainsFunc(allowed, func(a string) bool { return strings.EqualFold(a, got) }) {
			return oneLakeLoc{}, fmt.Errorf("%s option %s=%q is not supported by the emulator (supported: %s)",
				side, key, got, strings.Join(allowed, ", "))
		}
	}

	wsRef, err := field("workspaceId")
	if err != nil {
		return oneLakeLoc{}, err
	}
	itemRef, err := field("itemId")
	if err != nil {
		return oneLakeLoc{}, err
	}
	if itemRef == "" {
		// Fabric's linkedService names the lakehouse "artifactId".
		if itemRef, err = field("artifactId"); err != nil {
			return oneLakeLoc{}, err
		}
	}
	path, err := e.copyPath(field)
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

// copyPath derives the OneLake-relative path from whichever of Fabric's
// addressing styles the side used: an explicit `path`, a Tables-rooted `table`,
// or a Files-rooted `folderPath`/`fileName`.
func (e *pipelineExecutor) copyPath(field func(string) (string, error)) (string, error) {
	if p, err := field("path"); err != nil || p != "" {
		return p, err
	}
	root, err := field("rootFolder")
	if err != nil {
		return "", err
	}
	table, err := field("table")
	if err != nil {
		return "", err
	}
	if table != "" || strings.EqualFold(root, "Tables") {
		if table == "" {
			return "", fmt.Errorf("rootFolder Tables requires a table name")
		}
		// A schema-enabled lakehouse addresses Tables/<schema>/<table>; the
		// default dbo schema is implicit, matching Fabric.
		schema, err := field("schema")
		if err != nil {
			return "", err
		}
		if schema != "" && !strings.EqualFold(schema, "dbo") {
			return path.Join("Tables", schema, table), nil
		}
		return path.Join("Tables", table), nil
	}
	folder, err := field("folderPath")
	if err != nil {
		return "", err
	}
	name, err := field("fileName")
	if err != nil {
		return "", err
	}
	if folder == "" && name == "" {
		return "", nil
	}
	// folderPath is relative to the Files area unless it already names it.
	joined := path.Join(folder, name)
	if !strings.EqualFold(root, "Files") && (strings.HasPrefix(joined, "Files/") || joined == "Files") {
		return joined, nil
	}
	return path.Join("Files", joined), nil
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

// runPipelineWith parses the definition, executes it, and persists the activity
// runs against the job. It returns a failure code ("" on success) used to set
// the job's terminal status.
//
// `trigger` is the event that started the run, which the definition reads as
// `@pipeline()?.TriggerEvent?.FileName`. nil for a manual or scheduled run —
// which is why the documented way to read it safe-navigates.
func (a *API) runPipelineWith(wid string, it *store.Item, jobID string, params, trigger map[string]any) string {
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
	res := p.RunWith(params, &pipelineExecutor{a: a, wid: wid, jobID: jobID, chain: []string{it.ID}},
		pipeline.Options{
			TriggerEvent: trigger,
			// Each activity is announced as it settles, so a watcher sees a
			// failure at the moment it happens rather than reconstructing it
			// from the run afterwards.
			OnActivity: func(ar pipeline.ActivityRun) {
				a.Store.PublishActivityEvent(wid, it.ID, jobID, ar.Name, ar.Type,
					ar.Status, ar.Error, ar.Duration, ar.Retry)
			},
		})
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

// copyIntoTable lands a Copy's rows into a Tables/<name> Delta table. It
// reports handled=false when the source is not something it can parse into
// rows, leaving the caller's opaque byte copy as the fallback.
//
// This is what makes a medallion ingest real: Files/landing/*.csv into
// Tables/bronze_* with Append accumulating across runs, rather than one
// directory of bytes clobbering another.
func (e *pipelineExecutor) copyIntoTable(act pipeline.Activity, tp map[string]json.RawMessage, src, dst oneLakeLoc, table string, resolve func(json.RawMessage) (any, error)) (map[string]any, bool, error) {
	var srcProps map[string]json.RawMessage
	_ = json.Unmarshal(tp["source"], &srcProps)

	var tbl *warehouse.Table
	switch lookupFormat(srcProps, src.path) {
	case "delta":
		name, ok := deltaTableName(src.path)
		if !ok {
			return nil, false, nil
		}
		t, err := warehouse.ReadDeltaTable(e.a.Store, src.itemID, name)
		if err != nil {
			// Not a readable Delta table (no _delta_log yet, or a shape this
			// reader does not cover): fall back to the opaque directory copy
			// rather than failing a Copy that used to work.
			return nil, false, nil
		}
		tbl = t
	case "parquet", "csv":
		p, err := e.a.Store.GetOneLakePath(src.itemID, src.path)
		if err != nil || p.IsDir {
			return nil, false, nil // a directory or missing file: let the byte copy decide
		}
		t, err := parseTabular(p.Content, lookupFormat(srcProps, src.path))
		if err != nil {
			return nil, false, fmt.Errorf("copy %q: parsing source: %v", act.Name, err)
		}
		tbl = t
	default:
		return nil, false, nil
	}

	mode := warehouse.WriteOverwrite
	if action, err := e.copySinkAction(tp, resolve); err != nil {
		return nil, false, err
	} else if strings.EqualFold(action, "Append") {
		mode = warehouse.WriteAppend
	}
	if err := warehouse.WriteDeltaTableAs(store.ActivityBy(e.jobID, act.Name),
		e.a.Store, dst.wsID, dst.itemID, table, mode, tbl); err != nil {
		return nil, false, fmt.Errorf("copy %q: writing table %s: %v", act.Name, table, err)
	}

	// Producer is STATED, not defaulted. CreateLineageEdge fills an empty
	// producer with Copy as a backstop, but Copy asserts the emulator
	// WATCHED the bytes move — so a caller that merely forgot would be
	// claiming evidence it never had. These are the sites that really are
	// Copy, and saying so keeps the backstop a backstop.
	edge := &store.LineageEdge{WorkspaceID: e.wid, JobID: e.jobID, ActivityName: act.Name,
		Producer:          store.ProducerCopy,
		SourceWorkspaceID: src.wsID, SourceItemID: src.itemID, SourcePath: src.path,
		TargetWorkspaceID: dst.wsID, TargetItemID: dst.itemID, TargetPath: dst.path}
	if err := e.a.Store.CreateLineageEdge(edge); err != nil {
		return nil, false, fmt.Errorf("copy %q lineage: %v", act.Name, err)
	}
	return map[string]any{
		"rowsRead": len(tbl.Rows), "rowsCopied": len(tbl.Rows),
		"filesRead": 1, "filesWritten": 1, "copyDuration": 0,
		"writeBehavior": mode, "lineage": edge,
	}, true, nil
}

// copySinkAction reads the sink's tableActionOption (Fabric's Append /
// Overwrite / OverwriteSchema). Unsupported values were already rejected while
// resolving the location.
func (e *pipelineExecutor) copySinkAction(tp map[string]json.RawMessage, resolve func(json.RawMessage) (any, error)) (string, error) {
	var sink map[string]json.RawMessage
	if json.Unmarshal(tp["sink"], &sink) != nil {
		return "", nil
	}
	raw, ok := sink["tableActionOption"]
	if !ok {
		return "", nil
	}
	v, err := resolve(raw)
	if err != nil || v == nil {
		return "", err
	}
	return fmt.Sprint(v), nil
}

// parseTabular reads a single CSV or Parquet file into a warehouse.Table so a
// Copy can commit it as Delta rows. CSV values stay strings — the Delta writer
// types each column from its first non-null value, and inventing numeric types
// here would guess at data the file does not describe.
func parseTabular(content []byte, format string) (*warehouse.Table, error) {
	if format == "parquet" {
		return warehouse.ReadParquetBytes(content)
	}
	recs, err := csv.NewReader(bytes.NewReader(content)).ReadAll()
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return nil, fmt.Errorf("empty CSV")
	}
	tbl := &warehouse.Table{Columns: recs[0]}
	for _, rec := range recs[1:] {
		row := make([]any, len(tbl.Columns))
		for i := range tbl.Columns {
			if i < len(rec) {
				row[i] = rec[i]
			}
		}
		tbl.Rows = append(tbl.Rows, row)
	}
	return tbl, nil
}
