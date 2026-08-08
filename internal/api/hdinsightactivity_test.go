package api

import (
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

// TestHDInsightSparkRefusesByName: every property the emulator cannot honour
// is refused with its own reason. Ignoring one and running anyway would
// certify a definition whose behaviour differs — className especially, where
// the activity would silently run nothing of the Java main class it names.
func TestHDInsightSparkRefusesByName(t *testing.T) {
	for _, tc := range []struct{ name, tp, wantErr string }{
		{"className", `"rootPath":"x/y","entryFilePath":"e.py","className":"com.acme.Main"`,
			"JVM overlay"},
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
