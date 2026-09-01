package api

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/pipeline"
)

// The three Databricks activities, on the Livy/HDInsight precedent: the
// submission contract is terminated locally and the code is executed by the
// engine the emulator already runs.
//
// ORACLE: ADF's published schema (entityTypes/Pipeline.json), three
// discriminators with three required fields —
//
//	DatabricksNotebook     notebookPath  + baseParameters + libraries
//	DatabricksSparkPython  pythonFile    + parameters     + libraries
//	DatabricksSparkJar     mainClassName + parameters     + libraries
//
// THE JAR VARIANT RUNS ON THE OVERLAY AND IS REFUSED ON SAIL, which is a
// narrower boundary than the one that used to be recorded here. It read: "the
// agent's statement endpoint runs Python, and nothing here submits a main
// class — on Sail or on the JVM overlay". The first clause is still true; the
// second was a GAP rather than a law, because the overlay image is Apache
// Spark and ships spark-submit. It is now wired up (jarsubmit.go), and which
// engine answers is decided by asking it, not by assuming.
//
// The distinction that made the old note careful still holds and is still worth
// keeping: attaching a JAR *library* and EXECUTING a main class are different
// capabilities. The Spark-Job-Definition path probes for the first with
// `agentHasJVM`; this path needs the second, and the agent reports it as
// `available` from the presence of spark-submit rather than from the JVM.
//
// PATH ADDRESSING. Databricks addresses a workspace path (`/Shared/etl`) or
// DBFS (`dbfs:/jobs/etl.py`). Without FABRIC_DATABRICKS_URL the emulator has
// neither: paths resolve to OneLake by the same `<lakehouseItemId>/<path>`
// form HDInsight's rootPath uses, and a `dbfs:` or `/Workspace`-rooted path
// is REFUSED BY NAME rather than silently reinterpreted. When the URL is set,
// those paths are submitted to that workspace as written, and a lakehouse
// path is imported there first.
//
// LIBRARIES are refused when present: installing a PyPI/Maven library needs a
// cluster whose lifecycle the emulator does not own, and an Environment item
// is the modelled way to add packages (docs/37 §1). Accepting the field and
// installing nothing would be the silent half of that.

// databricksSpec is the per-variant shape read out of the ADF schema.
type databricksSpec struct {
	kind     string // for messages: "notebook", "python", "jar"
	pathKey  string // notebookPath | pythonFile
	paramKey string // baseParameters | parameters
}

func (e *pipelineExecutor) databricksActivity(
	act pipeline.Activity,
	tp map[string]json.RawMessage,
	resolve func(json.RawMessage) (any, error),
) (map[string]any, error) {
	var spec databricksSpec
	switch act.Type {
	case "DatabricksNotebook":
		spec = databricksSpec{kind: "notebook", pathKey: "notebookPath", paramKey: "baseParameters"}
	case "DatabricksSparkPython":
		spec = databricksSpec{kind: "python", pathKey: "pythonFile", paramKey: "parameters"}
	case "DatabricksSparkJar":
		// RUNS NOW, on the engine that can. This was refused with the cause
		// "nothing here submits a main class, on either engine" — true of the
		// agent's statement endpoint, and a gap rather than a law: the JVM
		// overlay ships spark-submit. The refusal survives where it is still
		// true (Sail has none), decided by PROBING the engine rather than
		// assuming. See jarsubmit.go.
		return e.databricksJarActivity(act, tp, resolve)
	default:
		return nil, fmt.Errorf("databricks activity %q: unknown type %q", act.Name, act.Type)
	}

	if raw, ok := tp["libraries"]; ok && len(raw) > 0 && string(raw) != "null" && string(raw) != "[]" {
		return nil, fmt.Errorf("databricks activity %q: `libraries` installs packages on a cluster "+
			"whose lifecycle the emulator does not own — bind an Environment item to add packages "+
			"(docs/37 §1). Accepting this and installing nothing would be the silent version of "+
			"the same gap", act.Name)
	}

	rawPath := ""
	if raw, ok := tp[spec.pathKey]; ok && len(raw) > 0 {
		v, err := resolve(raw)
		if err != nil {
			return nil, fmt.Errorf("databricks activity %q: %s: %w", act.Name, spec.pathKey, err)
		}
		if v != nil {
			rawPath = strings.TrimSpace(fmt.Sprint(v))
		}
	}
	if rawPath == "" {
		return nil, fmt.Errorf("databricks activity %q: %s is required", act.Name, spec.pathKey)
	}
	remote := strings.TrimRight(e.a.DatabricksURL, "/")
	if remote == "" {
		for _, foreign := range []string{"dbfs:/", "/Workspace/", "/Shared/", "/Repos/"} {
			if strings.HasPrefix(rawPath, foreign) {
				return nil, fmt.Errorf("databricks activity %q: %q addresses %s, which the emulator "+
					"does not have — use <lakehouseItemId>/<path> to name a file in OneLake, or set "+
					"FABRIC_DATABRICKS_URL so those paths resolve on databricks-emulator. "+
					"Reinterpreting a Databricks path as a lakehouse path would invent a mapping "+
					"nobody wrote", act.Name, rawPath, strings.TrimSuffix(foreign, "/"))
			}
		}
	}

	// baseParameters is an OBJECT (name -> value) for a notebook task;
	// parameters is an ARRAY of command-line arguments for a python task.
	// Both are resolved against the pipeline scope, and each reaches the code
	// the way its own task type delivers it.
	params := map[string]any{}
	var argv []string
	if raw, ok := tp[spec.paramKey]; ok && len(raw) > 0 {
		if spec.kind == "notebook" {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				return nil, fmt.Errorf("databricks activity %q: baseParameters must be an object", act.Name)
			}
			for k, vraw := range fields {
				v, perr := resolve(vraw)
				if perr != nil {
					return nil, fmt.Errorf("databricks activity %q: baseParameter %q: %w", act.Name, k, perr)
				}
				params[k] = v
			}
		} else {
			var items []json.RawMessage
			if err := json.Unmarshal(raw, &items); err != nil {
				return nil, fmt.Errorf("databricks activity %q: parameters must be an array", act.Name)
			}
			for i, iraw := range items {
				v, perr := resolve(iraw)
				if perr != nil {
					return nil, fmt.Errorf("databricks activity %q: parameter %d: %w", act.Name, i, perr)
				}
				argv = append(argv, fmt.Sprint(v))
			}
		}
	}

	if remote != "" {
		return e.databricksRemote(act, spec, rawPath, params, argv)
	}

	itemID, base, ok := splitRootPath(rawPath)
	if !ok || base == "" {
		return nil, fmt.Errorf("databricks activity %q: %s %q must be "+
			"<lakehouseItemId>/<path>", act.Name, spec.pathKey, rawPath)
	}
	p, gerr := e.a.Store.GetOneLakePath(itemID, base)
	if gerr != nil || p.IsDir {
		return nil, fmt.Errorf("databricks activity %q: no file at %q in item %q",
			act.Name, base, itemID)
	}

	if !e.a.runsNotebooksItself() {
		return nil, fmt.Errorf("databricks activity %q: no Spark agent is configured, so there is "+
			"nothing to execute %q — start the stack with a Spark engine", act.Name, base)
	}

	session := "databricks-" + e.jobID + "-" + act.Name
	defer func() { _, _ = e.a.agentPost("/close", map[string]any{"session": session}) }()

	// A notebook task's baseParameters arrive as dbutils.widgets would deliver
	// them — bound names in the namespace, which is what notebook code reads.
	// A python task's parameters arrive as sys.argv, argv[0] the file.
	var preamble string
	if spec.kind == "notebook" {
		pj, _ := json.Marshal(params)
		preamble = fmt.Sprintf("import json\nfor __k, __v in json.loads(%q).items():\n"+
			"    globals()[__k] = __v\n", string(pj))
	} else {
		aj, _ := json.Marshal(append([]string{base}, argv...))
		preamble = fmt.Sprintf("import sys, json\nsys.argv = json.loads(%q)\n", string(aj))
	}

	out, aerr := e.a.agentPost("/statements", map[string]any{
		"session": session,
		"code":    preamble + string(p.Content),
		"kind":    "python",
		"jobId":   e.jobID,
	})
	if aerr != nil {
		return nil, fmt.Errorf("databricks activity %q: %v", act.Name, aerr)
	}
	if status, _ := out["status"].(string); status != "ok" {
		return nil, fmt.Errorf("databricks activity %q: %s: %s", act.Name,
			fmt.Sprint(out["ename"]), fmt.Sprint(out["evalue"]))
	}

	result := map[string]any{
		"status": "Succeeded",
		// Fabric/ADF surface a runPageUrl pointing at the Databricks run. There
		// is no such page here, and inventing a URL that 404s would be worse
		// than omitting it — so the field says what happened instead.
		"executedBy": "the emulator's Spark engine, not a Databricks cluster",
	}
	result[spec.pathKey] = rawPath
	if spec.kind == "notebook" {
		result["baseParameters"] = params
	} else {
		result["parameters"] = argv
	}
	return result, nil
}
