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
// WHY THE JAR VARIANT IS REFUSED AND THE OTHER TWO ARE NOT. A Databricks JAR
// task names a Java/Scala main class compiled against Spark's own classes; the
// default engine is Sail (Rust, embedded CPython) and cannot load one — the
// same cause as parity.md's `spark.jars` row and HDInsight's `className`.
// Notebook and Python tasks are Python, which the agent runs for real.
//
// PATH ADDRESSING. Databricks addresses a workspace path (`/Shared/etl`) or
// DBFS (`dbfs:/jobs/etl.py`); the emulator has neither. Paths resolve to
// OneLake by the same `<lakehouseItemId>/<path>` form HDInsight's rootPath
// uses, and a `dbfs:` or `/Workspace`-rooted path is REFUSED BY NAME rather
// than silently reinterpreted — a definition that names DBFS and quietly reads
// a lakehouse would be the emulator inventing a mapping nobody wrote.
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
		// Refused before anything else: there is no JVM to load a main class
		// into, and running the wrong thing would be worse than refusing.
		name := ""
		if raw, ok := tp["mainClassName"]; ok && len(raw) > 0 {
			if v, err := resolve(raw); err == nil && v != nil {
				name = fmt.Sprint(v)
			}
		}
		return nil, fmt.Errorf("databricks activity %q: a Spark JAR task (mainClassName %q) needs a "+
			"Java/Scala main class on a JVM classpath, and the default engine is Sail (Rust, "+
			"embedded CPython) — same limit as spark.jars in docs/parity.md. Use a "+
			"DatabricksSparkPython or DatabricksNotebook task, or the JVM overlay", act.Name, name)
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
	for _, foreign := range []string{"dbfs:/", "/Workspace/", "/Shared/", "/Repos/"} {
		if strings.HasPrefix(rawPath, foreign) {
			return nil, fmt.Errorf("databricks activity %q: %q addresses %s, which the emulator "+
				"does not have — use <lakehouseItemId>/<path> to name a file in OneLake. "+
				"Reinterpreting a Databricks path as a lakehouse path would invent a mapping "+
				"nobody wrote", act.Name, rawPath, strings.TrimSuffix(foreign, "/"))
		}
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
