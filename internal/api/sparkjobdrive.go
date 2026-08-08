package api

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
)

// sparkJobSessionID names the agent session a Spark job runs in. Distinct from
// notebookSessionID's prefix because the two lifecycles are independent: a
// notebook and a Spark job can hold sessions at the same time, and a collision
// would bind one run's lakehouse into the other's catalog.
func sparkJobSessionID(jid string) string { return "sparkjob-" + jid }

// driveSparkJobRun executes a Spark Job Definition on the configured agent and
// reports the outcome, which is what real Fabric does: submitting an SJD runs
// it on Fabric's pool. Until this existed the emulator resolved the job, served
// it at GET …/sparkJobRun, and waited — so the only thing that ever ran a Spark
// job was a client that fetched the source and exec'd it ITSELF, which is what
// e2e/notebook-run did. That made the suite the engine rather than the witness:
// it proved the definition parsed and proved nothing about the emulator running
// anything.
//
// Deliberately the same shape as driveNotebookRun, including the guard below,
// because the failure it prevents is identical.
func (a *API) driveSparkJobRun(wid, iid, jid string, run sparkJobRun) {
	session := sparkJobSessionID(jid)

	// A goroutine that dies silently leaves a job parked at CompleteAt=MaxInt64
	// forever — jobs.go parks every SJD that parses precisely so the clock
	// cannot finish it, which means nothing else will. Silence is the one
	// outcome that must not be possible, because a job stuck InProgress with no
	// record is indistinguishable from one still working.
	finalised := false
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("spark job drive job=%s item=%s PANIC: %v", jid, iid, rec)
			if !finalised {
				a.finishSparkJobRun(wid, iid, jid, run, "Failed", "",
					fmt.Sprintf("the Spark job driver panicked: %v", rec))
			}
			return
		}
		if !finalised {
			log.Printf("spark job drive job=%s item=%s ended without finalising the run", jid, iid)
			a.finishSparkJobRun(wid, iid, jid, run, "Failed", "",
				"the Spark job driver exited without reporting a result")
		}
	}()
	defer func() { _, _ = a.agentPost("/close", map[string]any{"session": session}) }()
	log.Printf("spark job drive job=%s item=%s main=%s start", jid, iid, run.Job.MainFile)

	// A JAR-bearing Environment is an explicit JVM requirement, and a Connect
	// session's classpath is fixed at engine start. Refusing here — before any
	// user code runs — is the honest answer; running the job and letting an
	// import fail somewhere inside it would report a code error for a runtime
	// mismatch. The JVM overlay has a real classloader and takes them.
	if len(run.Environment.JARs) > 0 && !a.agentHasJVM(session) {
		finalised = true
		a.finishSparkJobRun(wid, iid, jid, run, "Failed", "",
			fmt.Sprintf("JAR libraries require the JVM Spark runtime: %s",
				strings.Join(run.Environment.JARs, ", ")))
		return
	}

	// The attached lakehouse, as the job sees it: reads resolve unqualified
	// (`spark.table("events")`) and writes land in OneLake rather than the
	// engine's own warehouse directory. Same two calls a notebook run makes,
	// for the same two directions.
	if run.Binding.LakehouseID != "" {
		a.registerLakehouseTables(session, run.Binding.WorkspaceID, run.Binding.LakehouseID)
		a.bindDefaultLakehouse(session, run.Binding)
	}
	if run.Binding.EnvironmentID != "" {
		envWID := run.Binding.EnvironmentWorkspaceID
		if envWID == "" {
			envWID = wid
		}
		a.applyEnvironment(session, envWID, run.Binding.EnvironmentID)
	}

	// argv[0] is the main file and the rest are the definition's arguments,
	// which is what `sys.argv` looks like to a spark-submit'd script — the
	// contract the job's own code reads. Set inside the session before the
	// source runs, so the source needs no cooperation.
	argv, _ := json.Marshal(append([]string{run.Job.MainFile}, run.Job.Arguments...))
	prelude := fmt.Sprintf("import sys\nsys.argv = %s\n", argv)
	if _, err := a.agentPost("/statements", map[string]any{
		"session": session, "code": prelude, "kind": "python", "jobId": jid,
	}); err != nil {
		finalised = true
		a.finishSparkJobRun(wid, iid, jid, run, "Failed", "",
			fmt.Sprintf("the Spark agent is unreachable: %v", err))
		return
	}

	out, err := a.agentPost("/statements", map[string]any{
		"session": session, "code": run.Job.Source, "kind": "python", "jobId": jid,
	})
	if err != nil {
		finalised = true
		a.finishSparkJobRun(wid, iid, jid, run, "Failed", "", err.Error())
		return
	}

	status, output, errMsg := "Completed", agentText(out), ""
	if r := cellResult(0, out); r.Status == "Failed" {
		status, errMsg = "Failed", r.Error
	}
	finalised = true
	log.Printf("spark job drive job=%s item=%s status=%s done", jid, iid, status)
	a.finishSparkJobRun(wid, iid, jid, run, status, output, errMsg)
}

// finishSparkJobRun stores the outcome and finalises the job — the same
// transition reportSparkJobRun performs for an external engine's callback, so
// a job the emulator ran and a job a client reported end identically. Shared
// rather than duplicated: two paths writing one terminal state is how they
// drift.
func (a *API) finishSparkJobRun(wid, iid, jid string, run sparkJobRun, status, output, errMsg string) {
	run.Status, run.Output, run.Error = status, output, errMsg
	blob, _ := json.Marshal(run)
	_ = a.Store.SetNotebookRun(jid, run.Status, string(blob))
	fail := ""
	if status == "Failed" {
		fail = "SparkJobExecutionFailed"
	}
	_ = a.Store.FinalizeJob(iid, jid, fail)
	a.publishJobOutcome(wid, iid, jid, fail)
}

// agentHasJVM reports whether the session's engine exposes a JVM. Asked of the
// engine rather than assumed from configuration: the same emulator runs against
// Sail and the JVM overlay, and the JAR answer differs between them.
func (a *API) agentHasJVM(session string) bool {
	out, err := a.agentPost("/statements", map[string]any{
		"session": session, "kind": "python",
		"code": "print(hasattr(spark, 'sparkContext'))",
	})
	if err != nil {
		return false
	}
	return strings.Contains(agentText(out), "True")
}
