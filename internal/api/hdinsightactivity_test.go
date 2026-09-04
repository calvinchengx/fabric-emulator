package api

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func hdiPipeline(tp string) string {
	return `{"properties":{"activities":[
      {"name":"Spark","type":"HDInsightSpark","typeProperties":{` + tp + `}}]}}`
}

// TestHDInsightSparkExecutesTheEntryFile: the submission protocol is
// terminated locally and the ENTRY FILE'S OWN CODE reaches the engine — the
// Livy precedent applied to a second protocol. The assertion is on what the
// agent was asked to run, because "the activity succeeded" would also be true
// of an activity that ran nothing.
func TestHDInsightSparkExecutesTheEntryFile(t *testing.T) {
	a, st := newAPI(t)
	agent := newFakeAgent(t, a)
	ws := seedWorkspace(t, st)
	lh := seedLakehouse(t, st, ws.ID, "lake")
	seedFile(t, st, ws.ID, lh.ID, "Files/jobs/etl.py", []byte("print('ran the entry file')\n"))

	pl := createPipeline(t, st, ws.ID, hdiPipeline(
		`"rootPath":"`+lh.ID+`/Files/jobs","entryFilePath":"etl.py",
         "arguments":["--date","2026-08-08"]`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
		t.Fatalf("job = %s; runs=%+v", s, runs)
	}

	code := strings.Join(agent.statements(), "\n")
	if !strings.Contains(code, "print('ran the entry file')") {
		t.Fatalf("the entry file's code never reached the engine: %q", code)
	}
	// arguments arrive as sys.argv, argv[0] being the entry file — the shape a
	// submitted Spark application actually sees. The argv list is embedded as a
	// quoted JSON literal, so the assertion is on the DECODED list rather than
	// on quoting that %q escapes: matching the raw text would pin the embedding
	// mechanism instead of the contract.
	for _, want := range []string{"etl.py", "--date", "2026-08-08"} {
		if !strings.Contains(code, want) {
			t.Fatalf("argv is missing %q: %s", want, code)
		}
	}
	if !strings.Contains(code, "sys.argv = json.loads(") {
		t.Fatalf("argv is not assigned at all: %s", code)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	out := outputOf(runs, "Spark")
	if out["entryFilePath"] != "etl.py" {
		t.Fatalf("output does not name the entry file: %+v", out)
	}
	// The output must not let a reader mistake this for a real cluster.
	if s, _ := out["executedBy"].(string); !strings.Contains(s, "not an HDInsight cluster") {
		t.Fatalf("output does not say which engine answered: %+v", out)
	}
}

// TestHDInsightSparkFailsWhenTheEngineFails: an engine error fails the
// activity rather than reporting a submission that "succeeded".
func TestHDInsightSparkFailsWhenTheEngineFails(t *testing.T) {
	a, st := newAPI(t)
	agent := newFakeAgent(t, a)
	agent.reply = func(code string) map[string]any {
		return map[string]any{"status": "error", "ename": "ValueError", "evalue": "bad input"}
	}
	ws := seedWorkspace(t, st)
	lh := seedLakehouse(t, st, ws.ID, "lake")
	seedFile(t, st, ws.ID, lh.ID, "Files/jobs/etl.py", []byte("raise ValueError('bad input')\n"))

	pl := createPipeline(t, st, ws.ID, hdiPipeline(
		`"rootPath":"`+lh.ID+`/Files/jobs","entryFilePath":"etl.py"`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job = %s, want Failed", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	if e, _ := runs[0]["error"].(string); !strings.Contains(e, "bad input") {
		t.Fatalf("error %q does not carry the engine's message", e)
	}
}

// TestHDInsightSparkNoEngineIsHonest: with no agent there is nothing to run
// the file, and the activity says so instead of claiming a submission.
func TestHDInsightSparkNoEngineIsHonest(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lh := seedLakehouse(t, st, ws.ID, "lake")
	seedFile(t, st, ws.ID, lh.ID, "Files/jobs/etl.py", []byte("x = 1\n"))

	pl := createPipeline(t, st, ws.ID, hdiPipeline(
		`"rootPath":"`+lh.ID+`/Files/jobs","entryFilePath":"etl.py"`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job = %s, want Failed with no engine attached", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	if e, _ := runs[0]["error"].(string); !strings.Contains(e, "no Spark agent is configured") {
		t.Fatalf("error %q does not name the missing engine", e)
	}
}

// TestHDInsightSparkClassNameSubmitsTheJar: className is a Spark main class,
// and jarsubmit.go is the path that used to be claimed missing. The
// assertion is what reached /submit — class, mounted jar, argv — not that
// the activity returned Succeeded.
func TestHDInsightSparkClassNameSubmitsTheJar(t *testing.T) {
	a, st := newAPI(t)
	j := newJarAgent(t, a, true, 0)
	ws := seedWorkspace(t, st)
	lh := seedLakehouse(t, st, ws.ID, "lake")
	seedFile(t, st, ws.ID, lh.ID, "Files/jobs/etl.jar", []byte("PK\x03\x04 not really a jar"))

	pl := createPipeline(t, st, ws.ID, hdiPipeline(
		`"rootPath":"`+lh.ID+`/Files/jobs","entryFilePath":"etl.jar",
         "className":"com.acme.Etl","arguments":["--date","2026-01-01"],
         "sparkConfig":{"spark.executor.memory":"1g"}`))
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
	if got, _ := j.seen["jar"].(string); got != "/lakehouse/default/Files/jobs/etl.jar" {
		t.Fatalf("jar = %q, want the mounted path", got)
	}
	args, _ := json.Marshal(j.seen["args"])
	for _, want := range []string{"--date", "2026-01-01"} {
		if !strings.Contains(string(args), want) {
			t.Fatalf("argv %s is missing %q", args, want)
		}
	}
	conf, _ := json.Marshal(j.seen["conf"])
	if !strings.Contains(string(conf), "spark.executor.memory") || !strings.Contains(string(conf), "1g") {
		t.Fatalf("sparkConfig never reached /submit: %s", conf)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	out := outputOf(runs, "Spark")
	if s, _ := out["executedBy"].(string); !strings.Contains(s, "not an HDInsight cluster") {
		t.Fatalf("output does not say which engine answered: %+v", out)
	}
	if strings.Contains(fmt.Sprint(out["executedBy"]), "Databricks") {
		t.Fatalf("HDInsight inherited the Databricks executedBy: %+v", out)
	}
}

// TestHDInsightSparkClassNameAsksTheEngine: Sail answers available:false;
// the emulator must relay that rather than keep the stale "no path" sentence.
func TestHDInsightSparkClassNameAsksTheEngine(t *testing.T) {
	a, st := newAPI(t)
	j := newJarAgent(t, a, false, 0)
	ws := seedWorkspace(t, st)
	lh := seedLakehouse(t, st, ws.ID, "lake")
	seedFile(t, st, ws.ID, lh.ID, "Files/jobs/etl.jar", []byte("PK\x03\x04"))

	pl := createPipeline(t, st, ws.ID, hdiPipeline(
		`"rootPath":"`+lh.ID+`/Files/jobs","entryFilePath":"etl.jar",
         "className":"com.acme.Etl"`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job = %s, want Failed on an engine with no spark-submit", s)
	}
	if j.seen == nil {
		t.Fatal("the engine was never asked — the refusal must be a probe, not the old assumption")
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	e, _ := runs[0]["error"].(string)
	if !strings.Contains(e, "no spark-submit") || !strings.Contains(e, "JVM overlay") {
		t.Fatalf("refusal %q does not name the limit or where the capability lives", e)
	}
	if !strings.Contains(e, "Probed, not assumed") {
		t.Fatalf("refusal %q does not say the engine was asked", e)
	}
	if strings.Contains(e, "no path that submits") {
		t.Fatalf("error still uses the stale cause: %q", e)
	}
}

func TestHDInsightSparkClassNameNeedsTheMount(t *testing.T) {
	a, st := newAPI(t)
	j := newJarAgent(t, a, true, 0)
	j.mountOK = false
	ws := seedWorkspace(t, st)
	lh := seedLakehouse(t, st, ws.ID, "lake")
	seedFile(t, st, ws.ID, lh.ID, "Files/jobs/etl.jar", []byte("PK\x03\x04"))

	pl := createPipeline(t, st, ws.ID, hdiPipeline(
		`"rootPath":"`+lh.ID+`/Files/jobs","entryFilePath":"etl.jar",
         "className":"com.acme.Etl"`))
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

// TestHDInsightSparkClassNameNonZeroExitFails: the JVM's exit code decides.
// A submit that fails while the HTTP call succeeds is the case a naive
// implementation reports green.
func TestHDInsightSparkClassNameNonZeroExitFails(t *testing.T) {
	a, st := newAPI(t)
	newJarAgent(t, a, true, 2)
	ws := seedWorkspace(t, st)
	lh := seedLakehouse(t, st, ws.ID, "lake")
	seedFile(t, st, ws.ID, lh.ID, "Files/jobs/etl.jar", []byte("PK\x03\x04"))

	pl := createPipeline(t, st, ws.ID, hdiPipeline(
		`"rootPath":"`+lh.ID+`/Files/jobs","entryFilePath":"etl.jar",
         "className":"com.acme.Etl"`))
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

// TestAzureHDInsightSparkClassNameIsTheSamePath: Fabric's merged type must
// not keep the stale ADF refusal when program=spark names a class.
func TestAzureHDInsightSparkClassNameIsTheSamePath(t *testing.T) {
	a, st := newAPI(t)
	j := newJarAgent(t, a, true, 0)
	ws := seedWorkspace(t, st)
	lh := seedLakehouse(t, st, ws.ID, "lake")
	seedFile(t, st, ws.ID, lh.ID, "Files/jobs/etl.jar", []byte("PK\x03\x04"))

	pl := createPipeline(t, st, ws.ID, `{"properties":{"activities":[
      {"name":"Spark","type":"AzureHDInsight","typeProperties":{
        "type":"spark","rootPath":"`+lh.ID+`/Files/jobs","entryFilePath":"etl.jar",
        "className":"com.acme.Etl"}}]}}`)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
		t.Fatalf("job = %s; runs=%+v", s, runs)
	}
	if j.seen == nil || j.seen["mainClass"] != "com.acme.Etl" {
		t.Fatalf("AzureHDInsight spark did not submit the class: %+v", j.seen)
	}
}

// TestHDInsightSparkRefusesByName: every property the emulator cannot honour
// is refused with its own reason. Ignoring one and running anyway would
// certify a definition whose behaviour differs.
func TestHDInsightSparkRefusesByName(t *testing.T) {
	for _, tc := range []struct{ name, tp, wantErr string }{
		{"proxyUser", `"rootPath":"x/y","entryFilePath":"e.py","proxyUser":"analyst"`,
			"no impersonation model"},
		{"external linked service", `"rootPath":"x/y","entryFilePath":"e.py","sparkJobLinkedService":{"referenceName":"blob","type":"LinkedServiceReference"}`,
			"does not model"},
		{"missing rootPath", `"entryFilePath":"e.py"`, "rootPath is required"},
		{"missing entryFilePath", `"rootPath":"x/y"`, "entryFilePath is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, st := newAPI(t)
			newFakeAgent(t, a)
			ws := seedWorkspace(t, st)
			pl := createPipeline(t, st, ws.ID, hdiPipeline(tc.tp))
			_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
			if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
				t.Fatalf("%s = %s, want Failed", tc.name, s)
			}
			_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
			e, _ := runs[0]["error"].(string)
			if !strings.Contains(e, tc.wantErr) {
				t.Errorf("error %q does not carry %q", e, tc.wantErr)
			}
		})
	}
}

// TestHDInsightSparkMissingEntryFile: a rootPath/entryFilePath that resolves
// to nothing fails naming the path, rather than executing an empty string.
func TestHDInsightSparkMissingEntryFile(t *testing.T) {
	a, st := newAPI(t)
	newFakeAgent(t, a)
	ws := seedWorkspace(t, st)
	lh := seedLakehouse(t, st, ws.ID, "lake")

	pl := createPipeline(t, st, ws.ID, hdiPipeline(
		`"rootPath":"`+lh.ID+`/Files/jobs","entryFilePath":"nope.py"`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job = %s, want Failed", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	if e, _ := runs[0]["error"].(string); !strings.Contains(e, "Files/jobs/nope.py") {
		t.Fatalf("error %q does not name the missing entry file", e)
	}
}
