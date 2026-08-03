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
	"fmt"
	"log"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/compute"
)

// notebookPrelude installs the one thing a Fabric notebook has that a bare REPL
// does not: a way to stop and hand a value back.
//
// `notebookutils.notebook.exit(v)` ends the run on real Fabric, so this must
// raise rather than merely record — a notebook that exits early and then runs
// the three cells after it has not been emulated, it has been mis-executed.
// The value is stashed BEFORE the raise so the driver can read it back without
// parsing a traceback: the agent reports a generic "Error" for every exception,
// and telling an intentional exit from a real failure by matching text in a
// stack trace would break the first time an unrelated message contained the
// word.
const notebookPrelude = `
class _NotebookExit(Exception):
    pass

__nb_exit__ = None

def notebook_exit(value=""):
    global __nb_exit__
    __nb_exit__ = str(value)
    raise _NotebookExit(__nb_exit__)

# The Fabric spelling, for a notebook written against the real thing.
try:
    import notebookutils
    notebookutils.notebook.exit = notebook_exit
except Exception:
    pass
`

// driveNotebookRun executes a parsed run's cells on the Spark agent and
// finalises the job, standing in for the Spark pool.
//
// Runs in its own goroutine: submitting a job returns 202 with a Location, and
// a caller polls. Blocking the POST until a notebook finished would be a
// different API from the one Fabric offers, and a long notebook would time the
// client out on a request that was working perfectly.
func (a *API) driveNotebookRun(wid, iid, jid string, run notebookRun) {
	session := "notebook-" + jid
	defer func() { _, _ = a.agentPost("/close", map[string]any{"session": session}) }()

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
		a.failNotebookRun(wid, iid, jid, run, fmt.Sprintf("the Spark agent is unreachable: %v", err))
		return
	}

	for _, cell := range run.Cells {
		kind := "python"
		if strings.EqualFold(cell.Language, "sql") {
			kind = "sql"
		}
		out, err := a.agentPost("/statements", map[string]any{
			"session": session, "code": cell.Source, "kind": kind,
		})
		if err != nil {
			a.failNotebookRun(wid, iid, jid, run, fmt.Sprintf("cell %d: %v", cell.Index, err))
			return
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
	out, err := a.agentPost("/statements", map[string]any{
		"session": session,
		// repr, so an exit value that is itself the text "None" is not mistaken
		// for the sentinel.
		"code": "repr(__nb_exit__)",
	})
	if err != nil {
		return "", false
	}
	text := strings.TrimSpace(agentText(out))
	if text == "" || text == "None" {
		return "", false
	}
	return strings.Trim(text, "'\""), true
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
