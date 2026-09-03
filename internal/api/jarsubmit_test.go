package api

import (
	"encoding/json"
	"github.com/calvinchengx/fabric-emulator/internal/store"
	"strings"
	"testing"
)

// jarAgent is a fake engine that answers /submit. `available` is the whole
// point: it is what separates the JVM overlay from Sail, and the emulator must
// take the engine's answer rather than assume one.
type jarAgent struct {
	agent     *fakeAgent
	available bool
	exit      int
	mountOK   bool
	seen      map[string]any
}

func newJarAgent(t *testing.T, a *API, available bool, exit int) *jarAgent {
	t.Helper()
	j := &jarAgent{available: available, exit: exit, mountOK: true}
	j.agent = newFakeAgent(t, a)
	j.agent.onPost = func(path string, body map[string]any) (map[string]any, bool) {
		if path == "/mount" {
			if !j.mountOK {
				return map[string]any{"mounted": false,
					"error": "this agent already has lakehouse old mounted"}, true
			}
			return map[string]any{"mounted": true}, true
		}
		if path != "/submit" {
			return nil, false
		}
		j.seen = body
		if !j.available {
			return map[string]any{"ok": false, "available": false,
				"error": "this engine has no spark-submit"}, true
		}
		out := map[string]any{"ok": j.exit == 0, "available": true,
			"exitCode": j.exit, "stdout": "rows=3\n", "stderr": ""}
		if j.exit != 0 {
			out["error"] = "spark-submit exited 2"
			out["stderr"] = "Exception in thread \"main\" java.lang.NoClassDefFoundError"
		}
		return out, true
	}
	return j
}

func jarPipeline(tp string) string {
	return `{"properties":{"activities":[
      {"name":"Jar","type":"DatabricksSparkJar","typeProperties":{` + tp + `}}]}}`
}

// seedJar puts a jar in a lakehouse and returns the reference form the activity
// takes.
func seedJar(t *testing.T, st *store.Store, wsID string) (string, string) {
	t.Helper()
	lh := seedLakehouse(t, st, wsID, "lake")
	seedFile(t, st, wsID, lh.ID, "Files/jobs/etl.jar", []byte("PK\x03\x04 not really a jar"))
	return lh.ID, lh.ID + "/Files/jobs/etl.jar"
}

// TestDatabricksJarRunsOnAnEngineThatCanSubmit is the capability this change
// exists for. The assertion is what reached the engine — main class, jar path
// and argv — not that the activity returned Succeeded, because an
// implementation that submitted nothing would satisfy the latter.
func TestDatabricksJarRunsOnAnEngineThatCanSubmit(t *testing.T) {
	a, st := newAPI(t)
	j := newJarAgent(t, a, true, 0)
	ws := seedWorkspace(t, st)
	_, ref := seedJar(t, st, ws.ID)

	pl := createPipeline(t, st, ws.ID, jarPipeline(
		`"mainClassName":"com.acme.Etl","libraries":[{"jar":"`+ref+`"}],
         "parameters":["--full","--since","2026-01-01"]`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
		t.Fatalf("job = %s; runs=%+v", s, runs)
	}
	if j.seen == nil {
		t.Fatal("nothing was submitted to the engine")
	}
	if j.seen["mainClass"] != "com.acme.Etl" {
		t.Fatalf("mainClass = %v, want the class the definition named", j.seen["mainClass"])
	}
	// The jar must be addressed where the MOUNT put it, not by its OneLake
	// reference — the submitting process sees the mount, not the store.
	if got, _ := j.seen["jar"].(string); got != "/lakehouse/default/Files/jobs/etl.jar" {
		t.Fatalf("jar = %q, want the mounted path", got)
	}
	args, _ := json.Marshal(j.seen["args"])
	for _, want := range []string{"--full", "--since", "2026-01-01"} {
		if !strings.Contains(string(args), want) {
			t.Fatalf("argv %s is missing %q", args, want)
		}
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	out := outputOf(runs, "Jar")
	if out["stdout"] != "rows=3\n" {
		t.Fatalf("the engine's output did not surface: %+v", out)
	}
	if s, _ := out["executedBy"].(string); !strings.Contains(s, "not a Databricks cluster") {
		t.Fatalf("output does not say which engine answered: %+v", out)
	}
}

// TestDatabricksJarParametersAreResolved: argv is evaluated against the
// pipeline scope, the same way a Python task's parameters are. Unmarshal-and-
// Sprint would submit the literal "@pipeline().parameters.since" — which is
// what Fabric evaluates and this path used to skip.
func TestDatabricksJarParametersAreResolved(t *testing.T) {
	a, st := newAPI(t)
	j := newJarAgent(t, a, true, 0)
	ws := seedWorkspace(t, st)
	_, ref := seedJar(t, st, ws.ID)

	pl := createPipeline(t, st, ws.ID, `{"properties":{
      "parameters":{"since":{"type":"String","defaultValue":"2026-01-01"}},
      "activities":[{"name":"Jar","type":"DatabricksSparkJar","typeProperties":{
        "mainClassName":"com.acme.Etl",
        "libraries":[{"jar":"`+ref+`"}],
        "parameters":["--since","@pipeline().parameters.since"]}}]}}`)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
		t.Fatalf("job = %s; runs=%+v", s, runs)
	}
	args, _ := json.Marshal(j.seen["args"])
	if !strings.Contains(string(args), "2026-01-01") {
		t.Fatalf("argv %s did not receive the resolved parameter", args)
	}
	if strings.Contains(string(args), "@pipeline()") {
		t.Fatalf("argv %s submitted the unevaluated expression", args)
	}
}

func TestDatabricksJarRefusesWhenTheFilesMountDidNotBind(t *testing.T) {
	a, st := newAPI(t)
	j := newJarAgent(t, a, true, 0)
	j.mountOK = false
	ws := seedWorkspace(t, st)
	_, ref := seedJar(t, st, ws.ID)

	pl := createPipeline(t, st, ws.ID, jarPipeline(
		`"mainClassName":"com.acme.Etl","libraries":[{"jar":"`+ref+`"}]`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job = %s, want Failed when the Files mount refuses", s)
	}
	if j.seen != nil {
		t.Fatalf("submitted despite a failed Files mount: %+v", j.seen)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	e, _ := runs[0]["error"].(string)
	if !strings.Contains(e, "Files mount") || !strings.Contains(e, "stale or wrong jar") {
		t.Fatalf("error %q does not name the mount risk", e)
	}
}

// TestDatabricksJarRefusedWhereThereIsNoSubmit: the boundary that survives, and
// it is decided by ASKING the engine. Sail answers available:false; the
// emulator must relay that rather than claim the class ran.
func TestDatabricksJarRefusedWhereThereIsNoSubmit(t *testing.T) {
	a, st := newAPI(t)
	j := newJarAgent(t, a, false, 0)
	ws := seedWorkspace(t, st)
	_, ref := seedJar(t, st, ws.ID)

	pl := createPipeline(t, st, ws.ID, jarPipeline(
		`"mainClassName":"com.acme.Etl","libraries":[{"jar":"`+ref+`"}]`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job = %s, want Failed on an engine with no spark-submit", s)
	}
	if j.seen == nil {
		t.Fatal("the engine was never asked — the refusal must be a PROBE, not an assumption")
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	e, _ := runs[0]["error"].(string)
	if !strings.Contains(e, "no spark-submit") || !strings.Contains(e, "JVM overlay") {
		t.Fatalf("refusal %q does not name the limit or where the capability lives", e)
	}
	if !strings.Contains(e, "Probed, not assumed") {
		t.Fatalf("refusal %q does not say the engine was asked", e)
	}
}

// TestDatabricksJarNonZeroExitFails: the JVM's exit code decides. A submit that
// fails while the HTTP call succeeds is the case a naive implementation
// reports green.
func TestDatabricksJarNonZeroExitFails(t *testing.T) {
	a, st := newAPI(t)
	newJarAgent(t, a, true, 2)
	ws := seedWorkspace(t, st)
	_, ref := seedJar(t, st, ws.ID)

	pl := createPipeline(t, st, ws.ID, jarPipeline(
		`"mainClassName":"com.acme.Etl","libraries":[{"jar":"`+ref+`"}]`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job = %s, want Failed on a non-zero exit", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	e, _ := runs[0]["error"].(string)
	if !strings.Contains(e, "exited 2") || !strings.Contains(e, "NoClassDefFoundError") {
		t.Fatalf("error %q carries neither the exit code nor the JVM's own output", e)
	}
}

// TestDatabricksJarInputSurface: the shapes a definition can get wrong, each
// refused before the engine is troubled.
func TestDatabricksJarInputSurface(t *testing.T) {
	for _, tc := range []struct{ name, tp, wantErr string }{
		{"no jar", `"mainClassName":"c.A"`, "no `jar` library names the code"},
		{"no main class", `"libraries":[{"jar":"LH/Files/jobs/etl.jar"}]`, "mainClassName is required"},
		{"pypi library", `"mainClassName":"c.A","libraries":[{"pypi":{"package":"requests"}}]`,
			"Only a `jar` library is the task's own payload"},
		{"dbfs jar", `"mainClassName":"c.A","libraries":[{"jar":"dbfs:/jobs/etl.jar"}]`,
			"invent a mapping nobody wrote"},
		{"missing jar file", `"mainClassName":"c.A","libraries":[{"jar":"LH/Files/nope.jar"}]`,
			"no jar at"},
		{"main class expr", `"mainClassName":"@nope(1)","libraries":[{"jar":"LH/Files/jobs/etl.jar"}]`,
			"mainClassName"},
		{"parameters expr", `"mainClassName":"c.A","libraries":[{"jar":"LH/Files/jobs/etl.jar"}],"parameters":["@nope(1)"]`,
			"parameter 0"},
		{"parameters not an array", `"mainClassName":"c.A","libraries":[{"jar":"LH/Files/jobs/etl.jar"}],"parameters":{}`,
			"parameters must be an array"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, st := newAPI(t)
			newJarAgent(t, a, true, 0)
			ws := seedWorkspace(t, st)
			lhID, _ := seedJar(t, st, ws.ID)
			pl := createPipeline(t, st, ws.ID,
				jarPipeline(strings.ReplaceAll(tc.tp, "LH", lhID)))
			_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
			if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
				t.Fatalf("%s = %s, want Failed", tc.name, s)
			}
			_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
			if e, _ := runs[0]["error"].(string); !strings.Contains(e, tc.wantErr) {
				t.Errorf("error %q does not carry %q", e, tc.wantErr)
			}
		})
	}
}

// TestDatabricksJarNoAgentIsHonest: nothing to submit to, said plainly.
func TestDatabricksJarNoAgentIsHonest(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	_, ref := seedJar(t, st, ws.ID)
	pl := createPipeline(t, st, ws.ID, jarPipeline(
		`"mainClassName":"c.A","libraries":[{"jar":"`+ref+`"}]`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job = %s, want Failed with no agent", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	if e, _ := runs[0]["error"].(string); !strings.Contains(e, "no Spark agent is configured") {
		t.Fatalf("error %q does not name the missing engine", e)
	}
}

// TestJarMountPathDoesNotPrefixTheItemTwice pins the signature choice: the
// mapper takes the ITEM-RELATIVE half, because taking the raw reference is how
// the lakehouse id ends up in the path twice.
func TestJarMountPathDoesNotPrefixTheItemTwice(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Files/jobs/etl.jar", "/lakehouse/default/Files/jobs/etl.jar"},
		{"/Files/jobs/etl.jar", "/lakehouse/default/Files/jobs/etl.jar"},
		{"jobs/etl.jar", "/lakehouse/default/Files/jobs/etl.jar"},
	} {
		if got := jarMountPath(tc.in); got != tc.want {
			t.Errorf("jarMountPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
