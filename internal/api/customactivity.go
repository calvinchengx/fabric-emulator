package api

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/pipeline"
)

// The Azure Batch activity — ADF's `Custom` — which runs a caller-supplied
// SHELL COMMAND on a Batch pool.
//
// ORACLE: ADF's published schema, discriminator `Custom`, required `command`;
// also `folderPath`, `resourceLinkedService`, `extendedProperties`,
// `referenceObjects`, `retentionTimeInDays`, `autoUserSpecification`.
//
// ON BY DEFAULT. A notebook, an SJD, an HDInsight entry file and a Databricks
// task already run PYTHON through the Spark agent — user code inside the
// engine's sandbox. A Custom activity's `command` is a process on that same
// host, behind the same bearer and workspace RBAC. Refusing it by default was
// the false pass for any pipeline that actually uses Batch, the same class of
// reversal as the Web activity. FABRIC_CUSTOM_ACTIVITY=off restores the
// refusal so no command reaches the agent; the old opt-in spelling `shell`
// remains on.
//
// The command is executed by the SPARK AGENT rather than in the emulator's
// own process — containerised in the standard compose deployment, so the
// blast radius is the engine container and not the API. That is the closest
// available analogue to a Batch pool node: a machine that is not the
// orchestrator's. The portal terminal stays opt-in: that route is still
// arbitrary execution on an otherwise-unauthenticated surface.
//
// WHAT IS REFUSED EVEN WHEN ENABLED, each because honouring it would require
// something the emulator does not have:
//
//   - `resourceLinkedService` / `folderPath` — Batch stages resource files from
//     a storage account onto the node before the command runs. The emulator
//     models no connections; accepting these and staging nothing would leave a
//     command referencing files that are not there, failing for a reason the
//     definition does not describe.
//   - `autoUserSpecification` — elevation level and scope on a Batch node.
//     There is no user model here, and accepting it would certify a privilege
//     behaviour that does not exist.
//   - `referenceObjects` — linked services and datasets serialised onto the
//     node for the command to read. Same absence, same reason.
//
// `retentionTimeInDays` is accepted and ignored: it governs how long Batch
// keeps task files, and there are no task files to keep.
//
// `extendedProperties` ARE honoured, because Batch's own contract for them is
// environment variables, which a process really can read.

func (e *pipelineExecutor) customActivity(
	act pipeline.Activity,
	tp map[string]json.RawMessage,
	resolve func(json.RawMessage) (any, error),
) (map[string]any, error) {
	if !e.a.CustomActivityShell {
		return nil, fmt.Errorf("custom activity %q: Custom activity execution is off "+
			"(FABRIC_CUSTOM_ACTIVITY=off) — a command never reaches the agent. Unset it, or "+
			"set FABRIC_CUSTOM_ACTIVITY=shell, to run the command in the Spark agent's container",
			act.Name)
	}

	for _, unsupported := range []struct{ key, why string }{
		{"resourceLinkedService", "Batch stages resource files from a storage account onto the " +
			"node before the command runs; the emulator models no connections, and staging " +
			"nothing would leave the command referencing files that are not there"},
		{"folderPath", "this names where resource files are staged from, which the emulator " +
			"cannot do — see resourceLinkedService"},
		{"autoUserSpecification", "elevation level and scope on a Batch node; the emulator has " +
			"no user model and would be certifying a privilege behaviour it does not implement"},
		{"referenceObjects", "linked services and datasets serialised onto the node for the " +
			"command to read; the emulator models neither"},
	} {
		if raw, ok := tp[unsupported.key]; ok && len(raw) > 0 && string(raw) != "null" {
			return nil, fmt.Errorf("custom activity %q: %s is not supported — %s",
				act.Name, unsupported.key, unsupported.why)
		}
	}

	command := ""
	if raw, ok := tp["command"]; ok && len(raw) > 0 {
		v, err := resolve(raw)
		if err != nil {
			return nil, fmt.Errorf("custom activity %q: command: %w", act.Name, err)
		}
		if v != nil {
			command = strings.TrimSpace(fmt.Sprint(v))
		}
	}
	if command == "" {
		return nil, fmt.Errorf("custom activity %q: command is required", act.Name)
	}

	// Batch surfaces extendedProperties to the task as environment variables,
	// so they are resolved against the pipeline scope and set for the process.
	env := map[string]string{}
	if raw, ok := tp["extendedProperties"]; ok && len(raw) > 0 {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, fmt.Errorf("custom activity %q: extendedProperties must be an object", act.Name)
		}
		for k, vraw := range fields {
			v, perr := resolve(vraw)
			if perr != nil {
				return nil, fmt.Errorf("custom activity %q: extendedProperty %q: %w", act.Name, k, perr)
			}
			env[k] = fmt.Sprint(v)
		}
	}

	if !e.a.runsNotebooksItself() {
		return nil, fmt.Errorf("custom activity %q: no Spark agent is configured, and the command "+
			"runs in the agent's container rather than the emulator's process — start the stack "+
			"with a Spark engine", act.Name)
	}

	session := "custom-" + e.jobID + "-" + act.Name
	defer func() { _, _ = e.a.agentPost("/close", map[string]any{"session": session}) }()

	// Run through the agent's Python statement endpoint, which is the only
	// execution surface the agent exposes — subprocess, so the command's own
	// exit status decides the activity, and both streams are captured because
	// a Batch task's output is what a user inspects when one fails.
	envJSON, _ := json.Marshal(env)
	cmdJSON, _ := json.Marshal(command)
	code := fmt.Sprintf(`import json, os, subprocess
__env = dict(os.environ); __env.update(json.loads(%q))
__p = subprocess.run(json.loads(%q), shell=True, capture_output=True, text=True, env=__env)
print(json.dumps({"exitCode": __p.returncode, "stdout": __p.stdout[-8192:], "stderr": __p.stderr[-8192:]}))
`, string(envJSON), string(cmdJSON))

	out, aerr := e.a.agentPost("/statements", map[string]any{
		"session": session, "code": code, "kind": "python", "jobId": e.jobID,
	})
	if aerr != nil {
		return nil, fmt.Errorf("custom activity %q: %v", act.Name, aerr)
	}
	if status, _ := out["status"].(string); status != "ok" {
		return nil, fmt.Errorf("custom activity %q: %s: %s", act.Name,
			fmt.Sprint(out["ename"]), fmt.Sprint(out["evalue"]))
	}

	// The agent returns the statement's stdout; the last JSON line is the
	// runner's report. A command that printed nothing parseable is a failure
	// to REPORT, not a success — saying "Succeeded" there would claim an exit
	// status nobody read.
	var report struct {
		ExitCode int    `json:"exitCode"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
	}
	text := agentText(out)
	line := lastJSONLine(text)
	if line == "" || json.Unmarshal([]byte(line), &report) != nil {
		return nil, fmt.Errorf("custom activity %q: the agent returned no runnable report (%q) — "+
			"the command's exit status is unknown, which is not the same as success",
			act.Name, snippet([]byte(text)))
	}
	if report.ExitCode != 0 {
		return nil, fmt.Errorf("custom activity %q: command exited %d: %s",
			act.Name, report.ExitCode, snippet([]byte(report.Stderr+report.Stdout)))
	}
	return map[string]any{
		"status":   "Succeeded",
		"exitCode": report.ExitCode,
		"stdout":   report.Stdout,
		"stderr":   report.Stderr,
		// Named, as everywhere else, so a run cannot be misread as a Batch pool.
		"executedBy": "the emulator's Spark agent container, not an Azure Batch node",
	}, nil
}

// lastJSONLine returns the final line that looks like a JSON object, which is
// how the runner's report is separated from anything the command itself
// printed to stdout.
func lastJSONLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if strings.HasPrefix(l, "{") && strings.HasSuffix(l, "}") {
			return l
		}
	}
	return ""
}
