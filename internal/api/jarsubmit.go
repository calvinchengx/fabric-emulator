package api

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/pipeline"
)

// Executing a named Java/Scala main class — the capability two activities were
// refused for.
//
// A Databricks JAR task names `mainClassName` and carries its jar in
// `libraries`. That was refused with the cause "the agent runs Python
// statements and nothing here submits a main class, on either engine". The
// second clause was true; the first was a GAP, not a law. The JVM overlay image
// IS Apache Spark and ships `spark-submit`; nothing had wired it up.
//
// SO THE REFUSAL SPLITS IN TWO, and the split is the honest part:
//
//   - on the JVM overlay, the class is really submitted and its EXIT CODE
//     decides the activity;
//   - on the default engine (Sail) there is no spark-submit, so it is still
//     refused — by a probe of the engine rather than an assumption about it.
//
// The jar is addressed through the LAKEHOUSE MOUNT rather than staged: the
// agent already mirrors the bound lakehouse at /lakehouse/default/Files, so a
// jar committed to OneLake is a file the submitting process can see. That
// reuses the mount the notebook path depends on instead of inventing a second
// transfer with its own failure modes.

// jarSubmitResult is the agent's /submit reply.
type jarSubmitResult struct {
	OK        bool   `json:"ok"`
	Available bool   `json:"available"`
	ExitCode  *int   `json:"exitCode"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	Error     string `json:"error"`
}

// submitMainClass runs one main class on the session's engine and turns the
// agent's reply into an activity outcome.
//
// `label` names the activity kind in errors so a Databricks JAR task and an
// HDInsight MapReduce job do not read as the same failure.
func (e *pipelineExecutor) submitMainClass(
	label string, act pipeline.Activity, session, mainClass, jarMountPath string,
	args []string, conf map[string]string,
) (map[string]any, error) {
	body := map[string]any{
		"mainClass": mainClass,
		"jar":       jarMountPath,
		"args":      args,
	}
	if len(conf) > 0 {
		body["conf"] = conf
	}
	out, err := e.a.agentPost("/submit", body)
	if err != nil {
		return nil, fmt.Errorf("%s %q: the Spark agent is unreachable: %v", label, act.Name, err)
	}

	raw, _ := json.Marshal(out)
	var res jarSubmitResult
	if json.Unmarshal(raw, &res) != nil {
		return nil, fmt.Errorf("%s %q: the agent's reply was not a submit result (%s) — the "+
			"exit status is unknown, which is not the same as success",
			label, act.Name, snippet(raw))
	}

	// AN AGENT THAT PREDATES /submit answers 404, and the transport turns that
	// into an error above. An agent that HAS the route but no spark-submit
	// answers available:false — a different fact, and the one that means
	// "this engine cannot, ask the overlay".
	if !res.Available {
		return nil, fmt.Errorf("%s %q: this engine has no spark-submit, so a Java/Scala main "+
			"class cannot be executed on it — the JVM overlay provides one (docker/spark-runtime, "+
			"docs/20). Probed, not assumed: %s", label, act.Name, res.Error)
	}
	if !res.OK {
		return nil, fmt.Errorf("%s %q: %s: %s", label, act.Name, res.Error,
			snippet([]byte(res.Stderr+res.Stdout)))
	}

	code := 0
	if res.ExitCode != nil {
		code = *res.ExitCode
	}
	return map[string]any{
		"status":   "Succeeded",
		"exitCode": code,
		"stdout":   res.Stdout,
		"stderr":   res.Stderr,
		// Named like every other executed activity, so a run cannot be misread
		// as a Databricks cluster or a YARN queue.
		"executedBy": "spark-submit on the emulator's JVM overlay, not a Databricks cluster",
	}, nil
}

// refuseForeignJarNamespace rejects the same foreign namespaces the notebook
// path rejects, and for the same reason: reinterpreting `dbfs:/jobs/x.jar` as a
// OneLake path invents a mapping nobody wrote.
func refuseForeignJarNamespace(label string, act pipeline.Activity, raw string) error {
	p := strings.TrimSpace(raw)
	if p == "" {
		return fmt.Errorf("%s %q: the jar to run was not named", label, act.Name)
	}
	for _, foreign := range []string{"dbfs:", "/Workspace", "/Shared", "/Repos", "abfss://", "wasbs://"} {
		if strings.HasPrefix(p, foreign) {
			return fmt.Errorf("%s %q: %q addresses %s, which the emulator does not model — "+
				"reinterpreting it as a OneLake path would invent a mapping nobody wrote. Commit "+
				"the jar to the bound lakehouse's Files/ and name it relative to that",
				label, act.Name, p, foreign)
		}
	}
	return nil
}

// jarMountPath maps an ITEM-RELATIVE path (the half after the lakehouse id) to
// where the agent's mount puts it. Taking the item-relative half rather than
// the raw reference is what stops the item id being prefixed twice — the bug
// this signature exists to make impossible.
func jarMountPath(itemRelative string) string {
	// Leading slash FIRST: stripping "Files/" from "/Files/x.jar" leaves the
	// slash, and the next line then re-adds a "Files/" it never removed —
	// which is the doubled-segment bug in a second costume.
	p := strings.TrimPrefix(strings.TrimSpace(itemRelative), "/")
	return "/lakehouse/default/Files/" + strings.TrimPrefix(p, "Files/")
}

// databricksJarActivity runs a Databricks JAR task for real on the JVM overlay.
//
// The jar arrives in `libraries` — that is where a JAR task carries it, so the
// blanket libraries refusal cannot apply to it. Every OTHER library kind
// (pypi, maven, cran) is still refused with the original cause: installing a
// package needs a cluster whose lifecycle the emulator does not own.
func (e *pipelineExecutor) databricksJarActivity(
	act pipeline.Activity,
	tp map[string]json.RawMessage,
	resolve func(json.RawMessage) (any, error),
) (map[string]any, error) {
	const label = "databricks JAR task"

	// A REAL DATABRICKS WORKSPACE IS STILL REFUSED, and this is not an
	// oversight carried forward. The local path below works because the jar is
	// a file in a lakehouse the agent already mounts; a remote workspace has
	// neither that mount nor the jar, and the emulator does not upload
	// libraries into someone else's DBFS. Submitting a jobs/create that names
	// a jar the workspace cannot see would fail there, late, for a reason the
	// definition does not describe.
	if strings.TrimRight(e.a.DatabricksURL, "/") != "" {
		name := ""
		if raw, ok := tp["mainClassName"]; ok && len(raw) > 0 {
			if v, err := resolve(raw); err == nil && v != nil {
				name = fmt.Sprint(v)
			}
		}
		return nil, fmt.Errorf("databricks activity %q: a Spark JAR task (mainClassName %q) names "+
			"a jar this emulator would have to upload into the configured Databricks workspace, "+
			"and it does not move libraries there — there is no submission path for one on "+
			"either engine when FABRIC_DATABRICKS_URL is set. Unset it to run the jar locally "+
			"on the JVM overlay, or use a DatabricksSparkPython or DatabricksNotebook task",
			act.Name, name)
	}

	mainClass := ""
	if raw, ok := tp["mainClassName"]; ok && len(raw) > 0 {
		v, err := resolve(raw)
		if err != nil {
			return nil, fmt.Errorf("%s %q: mainClassName: %w", label, act.Name, err)
		}
		if v != nil {
			mainClass = strings.TrimSpace(fmt.Sprint(v))
		}
	}
	if mainClass == "" {
		return nil, fmt.Errorf("%s %q: mainClassName is required", label, act.Name)
	}

	var libs []map[string]json.RawMessage
	if raw, ok := tp["libraries"]; ok && len(raw) > 0 && string(raw) != "null" {
		if json.Unmarshal(raw, &libs) != nil {
			return nil, fmt.Errorf("%s %q: libraries must be an array of objects", label, act.Name)
		}
	}
	jarRef := ""
	for _, lib := range libs {
		for kind, value := range lib {
			if strings.EqualFold(kind, "jar") {
				v, err := resolve(value)
				if err != nil {
					return nil, fmt.Errorf("%s %q: libraries jar: %w", label, act.Name, err)
				}
				if v != nil && jarRef == "" {
					jarRef = strings.TrimSpace(fmt.Sprint(v))
				}
				continue
			}
			// Unchanged from the original refusal, and deliberately: a jar is
			// the task's payload, a pypi/maven package is an install on a
			// cluster this emulator does not own.
			return nil, fmt.Errorf("%s %q: library %q installs a package on a cluster whose "+
				"lifecycle the emulator does not own — bind an Environment item to add packages "+
				"(docs/37 §1). Only a `jar` library is the task's own payload",
				label, act.Name, kind)
		}
	}
	if jarRef == "" {
		return nil, fmt.Errorf("%s %q: no `jar` library names the code to run — a JAR task "+
			"carries its jar in libraries", label, act.Name)
	}

	if err := refuseForeignJarNamespace(label, act, jarRef); err != nil {
		return nil, err
	}

	// The jar has to EXIST in OneLake before the mount can mirror it; a missing
	// file otherwise surfaces as a spark-submit error about a path, which
	// blames the engine for a definition mistake.
	itemID, base, ok := splitRootPath(jarRef)
	if !ok || base == "" {
		return nil, fmt.Errorf("%s %q: the jar %q must be <lakehouseItemId>/<path>",
			label, act.Name, jarRef)
	}
	if p, gerr := e.a.Store.GetOneLakePath(itemID, base); gerr != nil || p.IsDir {
		return nil, fmt.Errorf("%s %q: no jar at %q in item %q", label, act.Name, base, itemID)
	}

	var argv []string
	if raw, ok := tp["parameters"]; ok && len(raw) > 0 {
		var vals []any
		if json.Unmarshal(raw, &vals) != nil {
			return nil, fmt.Errorf("%s %q: parameters must be an array", label, act.Name)
		}
		for _, v := range vals {
			argv = append(argv, fmt.Sprint(v))
		}
	}

	if !e.a.runsNotebooksItself() {
		return nil, fmt.Errorf("%s %q: no Spark agent is configured, so there is nothing to "+
			"submit %q to — start the stack with a Spark engine", label, act.Name, mainClass)
	}

	session := "databricks-jar-" + e.jobID + "-" + act.Name
	defer func() { _, _ = e.a.agentPost("/close", map[string]any{"session": session}) }()
	// Mounts the lakehouse's Files/ at /lakehouse/default/Files, which is how
	// the submitted process reaches the jar and anything it reads.
	e.a.registerLakehouseTables(session, e.wid, itemID)

	return e.submitMainClass(label, act, session, mainClass, jarMountPath(base), argv, nil)
}
