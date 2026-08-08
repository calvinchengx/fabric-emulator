package api

import (
	"strings"
	"testing"
)

func dbxPipeline(actType, tp string) string {
	return `{"properties":{"activities":[
      {"name":"Dbx","type":"` + actType + `","typeProperties":{` + tp + `}}]}}`
}

// TestDatabricksNotebookRunsWithItsBaseParameters: the notebook's own code
// reaches the engine, and baseParameters arrive BOUND — a notebook task reads
// them as names, the way dbutils.widgets delivers them, not as argv.
func TestDatabricksNotebookRunsWithItsBaseParameters(t *testing.T) {
	a, st := newAPI(t)
	agent := newFakeAgent(t, a)
	ws := seedWorkspace(t, st)
	lh := seedLakehouse(t, st, ws.ID, "lake")
	seedFile(t, st, ws.ID, lh.ID, "Files/nb/transform.py", []byte("print(run_date)\n"))

	pl := createPipeline(t, st, ws.ID, dbxPipeline("DatabricksNotebook",
		`"notebookPath":"`+lh.ID+`/Files/nb/transform.py",
         "baseParameters":{"run_date":"2026-08-08","rows":"5"}`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
		t.Fatalf("job = %s; runs=%+v", s, runs)
	}

	code := strings.Join(agent.statements(), "\n")
	if !strings.Contains(code, "print(run_date)") {
		t.Fatalf("the notebook's code never reached the engine: %s", code)
	}
	// Bound into globals rather than assigned to sys.argv — the task type's
	// own delivery. Asserting the mechanism here is right: a notebook reading
	// `run_date` breaks if the value only ever lands in argv.
	if !strings.Contains(code, "globals()[__k] = __v") {
		t.Fatalf("baseParameters are not bound as names: %s", code)
	}
	for _, want := range []string{"run_date", "2026-08-08", "rows"} {
		if !strings.Contains(code, want) {
			t.Fatalf("baseParameter %q never reached the engine: %s", want, code)
		}
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	if s, _ := outputOf(runs, "Dbx")["executedBy"].(string); !strings.Contains(s, "not a Databricks cluster") {
		t.Fatalf("output does not say which engine answered: %+v", outputOf(runs, "Dbx"))
	}
}

// TestDatabricksSparkPythonRunsWithArgv: a python task's parameters are
// command-line arguments, so they arrive as sys.argv with argv[0] the file —
// the opposite delivery from the notebook task above, and getting the two
// backwards is exactly what this pair pins.
func TestDatabricksSparkPythonRunsWithArgv(t *testing.T) {
	a, st := newAPI(t)
	agent := newFakeAgent(t, a)
	ws := seedWorkspace(t, st)
	lh := seedLakehouse(t, st, ws.ID, "lake")
	seedFile(t, st, ws.ID, lh.ID, "Files/jobs/load.py", []byte("import sys; print(sys.argv)\n"))

	pl := createPipeline(t, st, ws.ID, dbxPipeline("DatabricksSparkPython",
		`"pythonFile":"`+lh.ID+`/Files/jobs/load.py","parameters":["--full","--since","2026-01-01"]`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
		t.Fatalf("job = %s; runs=%+v", s, runs)
	}
	code := strings.Join(agent.statements(), "\n")
	if !strings.Contains(code, "sys.argv = json.loads(") {
		t.Fatalf("parameters were not delivered as argv: %s", code)
	}
	for _, want := range []string{"--full", "--since", "2026-01-01", "load.py"} {
		if !strings.Contains(code, want) {
			t.Fatalf("argv is missing %q: %s", want, code)
		}
	}
	if strings.Contains(code, "globals()[__k]") {
		t.Fatal("a python task bound its parameters as names; that is the notebook task's delivery")
	}
}

// TestDatabricksRefusesByName: each unsupported shape refused with its cause.
// The JAR variant is the one that matters most — running a Python file in
// place of a named Java main class would be the emulator inventing behaviour.
func TestDatabricksRefusesByName(t *testing.T) {
	for _, tc := range []struct{ actType, tp, wantErr string }{
		{"DatabricksSparkJar", `"mainClassName":"com.acme.Job"`, "JVM overlay"},
		{"DatabricksNotebook", `"notebookPath":"x/y.py","libraries":[{"pypi":{"package":"requests"}}]`,
			"bind an Environment item"},
		{"DatabricksNotebook", `"notebookPath":"dbfs:/Shared/etl.py"`, "invent a mapping"},
		{"DatabricksSparkPython", `"pythonFile":"/Workspace/etl.py"`, "invent a mapping"},
		{"DatabricksNotebook", `"baseParameters":{}`, "notebookPath is required"},
		{"DatabricksSparkPython", `"parameters":[]`, "pythonFile is required"},
	} {
		t.Run(tc.actType+"/"+tc.wantErr, func(t *testing.T) {
			a, st := newAPI(t)
			newFakeAgent(t, a)
			ws := seedWorkspace(t, st)
			pl := createPipeline(t, st, ws.ID, dbxPipeline(tc.actType, tc.tp))
			_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
			if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
				t.Fatalf("%s = %s, want Failed", tc.wantErr, s)
			}
			_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
			e, _ := runs[0]["error"].(string)
			if !strings.Contains(e, tc.wantErr) {
				t.Errorf("error %q does not carry %q", e, tc.wantErr)
			}
		})
	}
}

// TestDatabricksEngineFailureFailsTheActivity: an engine error is the
// activity's error, not a submission reported as successful.
func TestDatabricksEngineFailureFailsTheActivity(t *testing.T) {
	a, st := newAPI(t)
	agent := newFakeAgent(t, a)
	agent.reply = func(code string) map[string]any {
		return map[string]any{"status": "error", "ename": "RuntimeError", "evalue": "cluster said no"}
	}
	ws := seedWorkspace(t, st)
	lh := seedLakehouse(t, st, ws.ID, "lake")
	seedFile(t, st, ws.ID, lh.ID, "Files/nb/x.py", []byte("raise RuntimeError('cluster said no')\n"))

	pl := createPipeline(t, st, ws.ID, dbxPipeline("DatabricksNotebook",
		`"notebookPath":"`+lh.ID+`/Files/nb/x.py"`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job = %s, want Failed", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	if e, _ := runs[0]["error"].(string); !strings.Contains(e, "cluster said no") {
		t.Fatalf("error %q does not carry the engine's message", e)
	}
}

// TestDatabricksNoEngineIsHonest: with nothing to execute the file, say so.
func TestDatabricksNoEngineIsHonest(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lh := seedLakehouse(t, st, ws.ID, "lake")
	seedFile(t, st, ws.ID, lh.ID, "Files/nb/x.py", []byte("x = 1\n"))

	pl := createPipeline(t, st, ws.ID, dbxPipeline("DatabricksNotebook",
		`"notebookPath":"`+lh.ID+`/Files/nb/x.py"`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job = %s, want Failed with no engine", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	if e, _ := runs[0]["error"].(string); !strings.Contains(e, "no Spark agent is configured") {
		t.Fatalf("error %q does not name the missing engine", e)
	}
}
