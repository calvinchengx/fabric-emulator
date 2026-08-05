package api

// RunNotebook, executed by the emulator itself.
//
// WHY THIS EXISTS. A RunNotebook job parks with no completion time and waits
// for an engine to POST its per-cell results back (notebooks.go). That contract
// is right — it is how real Fabric works, and it is what makes a terminal status
// mean execution happened — but until now the only implementation of the engine
// side was `e2e/notebook-run/runner.py`, which ships in no artifact. A consumer
// with the published images could submit a RunNotebook job and watch it hang
// forever; since 0.14.0 that surfaces as `NotebookError` on timeout.
//
// So the emulator drives the run itself when a Spark agent is configured. It
// already speaks to that agent for every Livy session and statement — the same
// `/statements` call, the same REPL namespace semantics — and the agent is now
// inside its own published image. Nothing new has to be deployed: a stack with
// FABRIC_SPARK_AGENT_URL set runs notebooks.
//
// WHAT THIS IS NOT. It does not make the callback optional or second-class.
// A real Spark pool reporting back is still the primary path, this reuses its
// exact finalisation, and an engine that posts results for a job the emulator
// also drove simply wins the race — both produce the same shape.

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/compute"
)

// notebookPrelude installs the one thing a Fabric notebook has that a bare REPL
// does not: a way to stop and hand a value back.
//
// `notebookutils.notebook.exit(v)` ends the run on real Fabric, so this must
// raise rather than merely record — a notebook that exits early and then runs
// the three cells after it has not been emulated, it has been mis-executed.
// The AGENT stashes the value into `__nb_exit__` when the raise reaches it
// (run_code matches the exception by type name); the driver then reads it back
// without parsing a traceback. The stash must NOT happen here: this prelude
// runs once per session, and each run re-points the one shared notebookutils
// module's `exit` at its own copy of notebook_exit. Under concurrent notebook
// runs, whichever session ran its prelude LAST owns that copy, so a
// `global __nb_exit__` write in it lands in that session's namespace, not the
// caller's. Observed as SUCCESS exits recorded Failed; the dual (a real
// failure inheriting a stale exit value from another run) would read as a
// false green. Only the agent knows which session executed the raising cell,
// so the agent writes the value.
//
// BOTH Fabric spellings are BOUND, not patched-if-present. The previous version
// wrapped the assignment in `try: import notebookutils`, which on this engine
// always raised ImportError and left nothing defined — so the fallback silently
// did nothing and every notebook written against real Fabric died on
// `NameError: name 'mssparkutils' is not defined`. Microsoft's own `fab` found
// it, running a notebook that used the older `mssparkutils` spelling. A helper
// that exists only when an absent package happens to be installed is the same
// class of defect as a check that cannot fail: it looks like support and
// provides none.
const notebookPrelude = `
class _NotebookExit(Exception):
    pass

__nb_exit__ = None

def notebook_exit(value=""):
    raise _NotebookExit(str(value))

class _NotebookNamespace(object):
    pass

_nb_ns = _NotebookNamespace()
_nb_ns.exit = notebook_exit
_utils_ns = _NotebookNamespace()
_utils_ns.notebook = _nb_ns

# notebookutils is the current name, mssparkutils the older one that a great
# deal of real Fabric code still uses. Real packages win where they exist; where
# they do not — which is here — the names still resolve.
try:
    import notebookutils
    notebookutils.notebook.exit = notebook_exit
except Exception:
    notebookutils = _utils_ns

try:
    import mssparkutils
    mssparkutils.notebook.exit = notebook_exit
except Exception:
    mssparkutils = _utils_ns
`

// driveNotebookRun executes a parsed run's cells on the Spark agent and
// finalises the job, standing in for the Spark pool.
//
// Runs in its own goroutine: submitting a job returns 202 with a Location, and
// a caller polls. Blocking the POST until a notebook finished would be a
// different API from the one Fabric offers, and a long notebook would time the
// client out on a request that was working perfectly.
// notebookSessionID is the Livy session a notebook run executes in. One
// definition, because the pipeline activity reports it as result.sessionId and a
// second spelling would report a session that never existed.
func notebookSessionID(jid string) string { return "notebook-" + jid }

func (a *API) driveNotebookRun(wid, iid, jid string, run notebookRun, params map[string]any) {
	session := notebookSessionID(jid)

	// A goroutine that dies silently is why a notebook can leave every cell
	// Pending under a job nobody can explain. Nothing here logged, and a panic
	// (or an early return added later) would take the drive down with no record
	// and no terminal state, leaving the run to be completed by something else.
	//
	// So: record that the drive finished, and if it finished WITHOUT finalising
	// the run, finalise it as a failure. Silence is the one outcome that must
	// not be possible, because it is the one that reads as success.
	finalised := false
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("notebook drive job=%s item=%s PANIC: %v", jid, iid, rec)
			if !finalised {
				a.failNotebookRun(wid, iid, jid, run, fmt.Sprintf("the notebook driver panicked: %v", rec))
			}
			return
		}
		if !finalised {
			log.Printf("notebook drive job=%s item=%s ended without finalising the run", jid, iid)
			a.failNotebookRun(wid, iid, jid, run,
				"the notebook driver exited without reporting a result")
		}
	}()
	defer func() { _, _ = a.agentPost("/close", map[string]any{"session": session}) }()
	log.Printf("notebook drive job=%s item=%s cells=%d start", jid, iid, len(run.Cells))

	// The default lakehouse, as a notebook attached to one would see it: on
	// Fabric a Lakehouse's Tables/ ARE catalog tables, so `spark.table("x")`
	// resolves without a path. Nothing here holds a metastore, so the folder is
	// enumerated into the session's catalog — exactly what a Livy session does.
	if run.Binding.LakehouseID != "" {
		a.registerLakehouseTables(session, run.Binding.WorkspaceID, run.Binding.LakehouseID)
		a.bindDefaultLakehouse(session, run.Binding)
	}

	body := notebookResultBody{Status: "Completed"}
	if _, err := a.agentPost("/statements", map[string]any{
		"session": session, "code": notebookPrelude,
	}); err != nil {
		finalised = true
		a.failNotebookRun(wid, iid, jid, run, fmt.Sprintf("the Spark agent is unreachable: %v", err))
		return
	}

	// A parameterised notebook declares its defaults in the PARAMETERS cell and a
	// caller overrides them per run: that is the whole point of `executionData.
	// parameters`, and it is how every pipeline-driven notebook receives the
	// workspace, lakehouse and batch ids it works on. Honoured for DataPipeline
	// but not here, the supplied values were silently dropped and the notebook
	// ran on its placeholder defaults — failing later on a validation the caller
	// had in fact satisfied.
	//
	// Fabric's semantics are override-after-defaults, so the assignments run
	// AFTER the parameters cell rather than replacing it: a parameter the caller
	// did not supply keeps the notebook's default.
	paramCode := parameterOverrides(params)

	// WHERE the overrides go is the whole game. Fabric applies them immediately
	// after the cell it designates as the PARAMETERS cell, which is not
	// necessarily the first one: a notebook that imports, or %run's a setup
	// notebook, before declaring its defaults puts a plain CELL first. Aiming at
	// index 0 in that shape writes the caller's values and then lets the
	// parameters cell assign its placeholders straight over them, so the run
	// proceeds on defaults the caller explicitly replaced and nothing reports
	// it. Fall back to index 0 only when the notebook designates no parameters
	// cell at all, which is the pre-existing behaviour for that shape.
	injectAfter := 0
	for i, cell := range run.Cells {
		if cell.Parameters {
			injectAfter = i
		}
	}

	for i, cell := range run.Cells {
		kind := "python"
		if strings.EqualFold(cell.Language, "sql") {
			kind = "sql"
		}
		// jobId/cellIndex are the cell's IDENTITY, and they travel with every
		// statement so the agent can export them for the duration of the cell.
		//
		// Two things downstream read that export and are dead without it:
		// notebookutils.fs tags its OneLake requests with the
		// x-ms-fabric-job-id / x-ms-fabric-cell-index headers, and
		// spark_agent/storage.py forges a Storage token carrying the same
		// values as claims (delta-rs takes credentials, not headers). Both
		// feed the storage layer's observed lineage, which is how an edge gets
		// recorded without anyone parsing the notebook's code.
		//
		// NOT sufficient for Spark's own writes: a `df.write` executes inside
		// Sail, whose storage token enters it through startup env and cannot
		// vary per cell (docker/sail/launcher.py). Those stay unattributed.
		out, err := a.agentPost("/statements", map[string]any{
			"session": session, "code": cell.Source, "kind": kind,
			"jobId": jid, "cellIndex": cell.Index,
		})
		if err != nil {
			finalised = true
			a.failNotebookRun(wid, iid, jid, run, fmt.Sprintf("cell %d: %v", cell.Index, err))
			return
		}

		// "The execution engine adds a new cell beneath the parameters cell":
		// beneath THAT cell, not beneath the first one.
		if i == injectAfter && paramCode != "" {
			if _, perr := a.agentPost("/statements", map[string]any{
				"session": session, "code": paramCode, "kind": "python",
				"jobId": jid, "cellIndex": cell.Index,
			}); perr != nil {
				finalised = true
				a.failNotebookRun(wid, iid, jid, run, fmt.Sprintf("applying run parameters: %v", perr))
				return
			}
		}

		result := cellResult(cell.Index, out)
		if result.Status == "Failed" {
			// An exit is reported as an exception, because it IS one — so ask
			// the namespace which happened rather than reading the traceback.
			if v, exited := a.notebookExitValue(session); exited {
				result.Status = "Succeeded"
				result.Error = ""
				body.ExitValue = v
				body.Cells = append(body.Cells, result)
				break
			}
			body.Status = "Failed"
			body.Cells = append(body.Cells, result)
			break
		}
		body.Cells = append(body.Cells, result)
	}

	// A notebook that ran to the end may still have exited on its last line.
	if body.ExitValue == "" && body.Status != "Failed" {
		if v, exited := a.notebookExitValue(session); exited {
			body.ExitValue = v
		}
	}

	finalised = true
	log.Printf("notebook drive job=%s item=%s status=%s cells=%d done",
		jid, iid, body.Status, len(body.Cells))
	a.finalizeNotebookRun(wid, iid, jid, run, body)
}

// bindDefaultLakehouse points the session's current database at the attached
// lakehouse's Tables/ folder, so a table the notebook CREATES lands in OneLake.
//
// registerLakehouseTables covers the read direction and stops there — it
// enumerates what already exists and returns early when a lakehouse is empty,
// which at the start of a notebook is the normal case. That left the write
// direction unbound: `df.write.saveAsTable("events")` succeeded, the next cell
// read the rows straight back, the run went green, and the bytes were sitting
// in the engine's own warehouse directory rather than in the lakehouse. A
// notebook that appears to work and writes nowhere anybody can find is worse
// than one that fails.
//
// Two statements because engines differ: Sail rejects `USE <schema>` outright
// but accepts setCurrentDatabase, so the binding is attempted the Python way
// and a failure is not fatal — a notebook addressing tables by full abfs path
// needs none of this.
func (a *API) bindDefaultLakehouse(session string, b compute.Binding) {
	tables := fmt.Sprintf("abfs://%s@onelake.dfs.fabric.microsoft.com/%s/Tables",
		b.WorkspaceID, b.LakehouseID)
	name := b.LakehouseName
	if name == "" {
		name = "lakehouse"
	}
	code := fmt.Sprintf(`
spark.sql("CREATE DATABASE IF NOT EXISTS `+"`%s`"+` LOCATION '%s'")
try:
    spark.catalog.setCurrentDatabase(%q)
except Exception:
    pass
`, name, tables, name)
	if _, err := a.agentPost("/statements", map[string]any{
		"session": session, "code": code,
	}); err != nil {
		log.Printf("notebook: binding default lakehouse %s: %v", name, err)
	}
}

// notebookExitValue reads __nb_exit__ out of the session, reporting whether the
// notebook called notebook_exit at all. An empty exit value is legitimate —
// `notebookutils.notebook.exit()` takes no argument — so "did it exit" cannot
// be inferred from the string being empty.
func (a *API) notebookExitValue(session string) (string, bool) {
	// PRINTED AS JSON, not repr'd. The obvious spelling — evaluate
	// `repr(__nb_exit__)` and strip the quotes — is wrong twice over. The agent
	// applies repr() to a statement's result for display, so asking for a repr
	// gets one repr'd AGAIN, and an exit value containing quotes comes back as
	// \'{"a": 1}\'. Stripping outer quote characters then cannot recover it.
	//
	// It survived the e2e because that notebook exits with str(count) — "4" has
	// no quotes, so double-repr still trimmed clean. The first real notebook to
	// return JSON found it immediately.
	//
	// json.dumps of a {exited, value} pair is unambiguous for any content, and
	// carries the did-it-exit bit that an empty string cannot: notebookutils'
	// exit() takes no argument, so "" is a legitimate exit value.
	out, err := a.agentPost("/statements", map[string]any{
		"session": session,
		"code":    `import json as _j; print(_j.dumps({"exited": __nb_exit__ is not None, "value": __nb_exit__}))`,
	})
	if err != nil {
		return "", false
	}
	var probe struct {
		Exited bool   `json:"exited"`
		Value  string `json:"value"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(agentText(out))), &probe); err != nil {
		return "", false
	}
	return probe.Value, probe.Exited
}

// cellResult maps one agent reply onto the per-cell record the callback uses.
func cellResult(index int, out map[string]any) notebookCellResult {
	r := notebookCellResult{Index: index, Status: "Succeeded"}
	if s, _ := out["status"].(string); s == "error" {
		r.Status = "Failed"
		ename, _ := out["ename"].(string)
		evalue, _ := out["evalue"].(string)
		r.Error = strings.TrimSpace(ename + ": " + evalue)
	}
	r.Output = agentText(out)
	return r
}

// agentText pulls the human-readable output out of an agent reply. A python
// statement answers with data["text/plain"]; a SQL statement answers with a
// structured envelope, which has no text to show.
func agentText(out map[string]any) string {
	data, _ := out["data"].(map[string]any)
	if data == nil {
		return ""
	}
	s, _ := data["text/plain"].(string)
	return s
}

// failNotebookRun ends a run the emulator could not execute — the agent went
// away mid-notebook, or was never reachable.
//
// The failure is recorded against the job rather than logged, because the job
// is the only thing the caller is watching. An unreachable agent that left the
// job Pending would present as a notebook that runs forever, which is the exact
// symptom this whole file exists to remove.
func (a *API) failNotebookRun(wid, iid, jid string, run notebookRun, msg string) {
	body := notebookResultBody{Status: "Failed"}
	body.Cells = append(body.Cells, notebookCellResult{Index: 0, Status: "Failed", Error: msg})
	a.finalizeNotebookRun(wid, iid, jid, run, body)
}

// runsNotebooksItself reports whether this emulator can execute a notebook
// without an external engine.
func (a *API) runsNotebooksItself() bool { return a.livyAgent != nil }

// parameterOverrides renders `executionData.parameters` as python assignments.
//
// Fabric passes each parameter as {"value": ..., "type": ...}; the value is what
// the notebook binds, and it is rendered through JSON so a string stays quoted
// and a number, bool or null arrives as the literal the notebook expects. A name
// that is not a plain identifier is skipped rather than injected, because this
// text is executed.
func parameterOverrides(params map[string]any) string {
	if len(params) == 0 {
		return ""
	}
	names := make([]string, 0, len(params))
	for name := range params {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic, so a run is reproducible
	var b strings.Builder
	for _, name := range names {
		if !isPyIdentifier(name) {
			continue
		}
		value := params[name]
		if m, ok := value.(map[string]any); ok {
			if v, present := m["value"]; present {
				value = v
			}
		}
		lit, err := json.Marshal(value)
		if err != nil {
			continue
		}
		// JSON null/true/false are not python literals.
		switch string(lit) {
		case "null":
			lit = []byte("None")
		case "true":
			lit = []byte("True")
		case "false":
			lit = []byte("False")
		}
		fmt.Fprintf(&b, "%s = %s\n", name, lit)
	}
	return b.String()
}

func isPyIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_':
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}
