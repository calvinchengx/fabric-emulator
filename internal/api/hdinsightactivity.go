package api

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/pipeline"
)

// The HDInsight Spark activity: the submission protocol terminated locally,
// the code executed by the engine the emulator already runs.
//
// This is the Livy precedent (docs/20, docs/12's `livy-native` row) applied to
// a second protocol: the emulator does not proxy to an HDInsight cluster, it
// TERMINATES the activity's contract itself and lets Sail compute — the same
// bet as "the emulator terminates the Livy protocol and drives a statement
// agent". A pipeline written against HDInsight therefore runs here, and the
// parity row says exactly which half is real.
//
// ORACLE: ADF's published schema (entityTypes/Pipeline.json), discriminator
// `HDInsightSpark`. Required: `rootPath`, `entryFilePath`. Also carried:
// `arguments` (a string array), `className`, `sparkConfig`, `proxyUser`,
// `getDebugInfo`, `sparkJobLinkedService`.
//
// WHAT IS REAL AND WHAT IS REFUSED, stated because the difference is the whole
// value of the row:
//
//   - The entry file is READ FROM ONELAKE at `rootPath`/`entryFilePath` and
//     EXECUTED by the Spark agent, with `arguments` visible to it as `sys.argv`
//     — the same shape a Spark Job Definition's main file gets, because it is
//     the same engine and the same statement endpoint.
//   - `className` is REFUSED BY NAME. A Java/Scala main class needs Spark's own
//     jars on a JVM classpath; Sail is Rust with an embedded CPython and cannot
//     take one (parity.md's "Java/Scala UDFs, spark.jars" row, same cause). The
//     JVM overlay is the answer there, and the error says so rather than
//     ignoring the field and running nothing.
//   - `sparkJobLinkedService` is REFUSED BY NAME when it names an external
//     store: the emulator models no connections, and silently reading OneLake
//     while a definition names Azure Blob would be the permissive direction.
//   - `proxyUser` is REFUSED BY NAME. There is no impersonation model here, and
//     accepting it would certify an authorization behaviour the emulator does
//     not implement.
//
// A `getDebugInfo` of `Always`/`Failure` is accepted and ignored: it selects
// log verbosity on a cluster, and there is no cluster whose logs to select.

func (e *pipelineExecutor) hdinsightSparkActivity(
	act pipeline.Activity,
	tp map[string]json.RawMessage,
	resolve func(json.RawMessage) (any, error),
) (map[string]any, error) {
	str := func(key string) (string, error) {
		raw, ok := tp[key]
		if !ok || len(raw) == 0 {
			return "", nil
		}
		v, err := resolve(raw)
		if err != nil {
			return "", fmt.Errorf("hdinsight activity %q: %s: %w", act.Name, key, err)
		}
		if v == nil {
			return "", nil
		}
		return strings.TrimSpace(fmt.Sprint(v)), nil
	}

	for _, unsupported := range []struct{ key, why string }{
		{"className", "a Java/Scala main class needs Spark's own jars on a JVM classpath; " +
			"the default engine is Sail (Rust, embedded CPython) and cannot load one — " +
			"use the JVM overlay (docker-compose.spark-jvm.yml)"},
		{"proxyUser", "the emulator has no impersonation model, and accepting this would " +
			"certify an authorization behaviour it does not implement"},
	} {
		if v, err := str(unsupported.key); err != nil {
			return nil, err
		} else if v != "" {
			return nil, fmt.Errorf("hdinsight activity %q: %s is not supported — %s",
				act.Name, unsupported.key, unsupported.why)
		}
	}
	// sparkJobLinkedService names WHERE the files live. Present means an
	// external store; the emulator reads OneLake and models no connections.
	if raw, ok := tp["sparkJobLinkedService"]; ok && len(raw) > 0 && string(raw) != "null" {
		return nil, fmt.Errorf("hdinsight activity %q: sparkJobLinkedService names an external "+
			"storage connection, which the emulator does not model — the entry file is read "+
			"from OneLake at rootPath/entryFilePath", act.Name)
	}

	rootPath, err := str("rootPath")
	if err != nil {
		return nil, err
	}
	if rootPath == "" {
		return nil, fmt.Errorf("hdinsight activity %q: rootPath is required", act.Name)
	}
	entryFilePath, err := str("entryFilePath")
	if err != nil {
		return nil, err
	}
	if entryFilePath == "" {
		return nil, fmt.Errorf("hdinsight activity %q: entryFilePath is required", act.Name)
	}

	// rootPath carries the lakehouse: {itemId}/{path...}, the same
	// {workspaceId?, itemId, path} addressing Copy and Lookup already use,
	// flattened into one string because ADF's rootPath IS one string.
	itemID, base, ok := splitRootPath(rootPath)
	if !ok {
		return nil, fmt.Errorf("hdinsight activity %q: rootPath %q must be "+
			"<lakehouseItemId>/<folder> — the emulator addresses OneLake by item, "+
			"where HDInsight addresses a storage account", act.Name, rootPath)
	}
	full := path.Join(base, entryFilePath)

	p, gerr := e.a.Store.GetOneLakePath(itemID, full)
	if gerr != nil || p.IsDir {
		return nil, fmt.Errorf("hdinsight activity %q: no entry file at %q in item %q",
			act.Name, full, itemID)
	}

	var args []string
	if raw, ok := tp["arguments"]; ok && len(raw) > 0 {
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, fmt.Errorf("hdinsight activity %q: arguments must be an array", act.Name)
		}
		for i, iraw := range items {
			v, aerr := resolve(iraw)
			if aerr != nil {
				return nil, fmt.Errorf("hdinsight activity %q: argument %d: %w", act.Name, i, aerr)
			}
			args = append(args, fmt.Sprint(v))
		}
	}

	sparkConf := map[string]any{}
	if raw, ok := tp["sparkConfig"]; ok && len(raw) > 0 {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, fmt.Errorf("hdinsight activity %q: sparkConfig must be an object", act.Name)
		}
		for k, vraw := range fields {
			v, cerr := resolve(vraw)
			if cerr != nil {
				return nil, fmt.Errorf("hdinsight activity %q: sparkConfig %q: %w", act.Name, k, cerr)
			}
			sparkConf[k] = fmt.Sprint(v)
		}
	}

	if !e.a.runsNotebooksItself() {
		// No engine: say so rather than reporting a submission nothing ran.
		return nil, fmt.Errorf("hdinsight activity %q: no Spark agent is configured, so there is "+
			"nothing to execute the entry file — start the stack with a Spark engine", act.Name)
	}

	session := "hdinsight-" + e.jobID + "-" + act.Name
	defer func() { _, _ = e.a.agentPost("/close", map[string]any{"session": session}) }()

	// sys.argv exactly as a submitted Spark application sees it: argv[0] is the
	// entry file, the activity's arguments follow.
	argv, _ := json.Marshal(append([]string{entryFilePath}, args...))
	conf, _ := json.Marshal(sparkConf)
	preamble := fmt.Sprintf("import sys, json\nsys.argv = json.loads(%q)\n"+
		"__spark_conf__ = json.loads(%q)\n", string(argv), string(conf))

	out, aerr := e.a.agentPost("/statements", map[string]any{
		"session": session,
		"code":    preamble + string(p.Content),
		"kind":    "python",
		"jobId":   e.jobID,
	})
	if aerr != nil {
		return nil, fmt.Errorf("hdinsight activity %q: %v", act.Name, aerr)
	}
	if status, _ := out["status"].(string); status != "ok" {
		return nil, fmt.Errorf("hdinsight activity %q: %s: %s", act.Name,
			fmt.Sprint(out["ename"]), fmt.Sprint(out["evalue"]))
	}

	return map[string]any{
		"status":        "Succeeded",
		"entryFilePath": entryFilePath,
		"rootPath":      rootPath,
		"arguments":     args,
		// Named so a reader of the run cannot mistake which engine answered —
		// the same instinct as the Web activity's `stubbed: true`.
		"executedBy": "the emulator's Spark engine, not an HDInsight cluster",
	}, nil
}

// splitRootPath splits "<itemId>/<folder...>" into its parts. The folder may
// be empty (the entry file sits at the item root).
func splitRootPath(rootPath string) (itemID, base string, ok bool) {
	s := strings.Trim(rootPath, "/")
	if s == "" {
		return "", "", false
	}
	if i := strings.Index(s, "/"); i > 0 {
		return s[:i], s[i+1:], true
	}
	return s, "", true
}
