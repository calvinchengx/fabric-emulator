package api

import (
	"strings"
	"testing"
)

func customPipeline(tp string) string {
	return `{"properties":{"activities":[
      {"name":"Batch","type":"Custom","typeProperties":{` + tp + `}}]}}`
}

// TestCustomActivityRunsByDefault is the parity assertion, and it is first
// because it is the one that must never regress: a Custom activity's command
// reaches the agent with no extra flag, matching a notebook cell on the same
// machine. A test that only checked the job completed would pass even if the
// command had been refused and then stubbed, so this asserts the agent was asked.
func TestCustomActivityRunsByDefault(t *testing.T) {
	a, st := newAPI(t)
	agent := newFakeAgent(t, a)
	agent.reply = func(code string) map[string]any {
		return map[string]any{"status": "ok", "data": map[string]any{
			"text/plain": `{"exitCode":0,"stdout":"pwned\n","stderr":""}`}}
	}
	ws := seedWorkspace(t, st)

	pl := createPipeline(t, st, ws.ID, customPipeline(`"command":"echo pwned"`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
		t.Fatalf("job = %s; runs=%+v", s, runs)
	}
	code := strings.Join(agent.statements(), "\n")
	if !strings.Contains(code, "echo pwned") {
		t.Fatalf("the command never reached the agent: %s", code)
	}
	if !strings.Contains(code, "subprocess.run") || !strings.Contains(code, "shell=True") {
		t.Fatalf("the command was not run as a process: %s", code)
	}
}

// TestCustomActivityOffRefusesAndDoesNotReachAgent: FABRIC_CUSTOM_ACTIVITY=off
// restores the old refusal, and NO COMMAND REACHES THE AGENT. A test that only
// checked the job failed would pass even if the command had run and then been
// reported as an error, so this asserts the agent was never asked.
func TestCustomActivityOffRefusesAndDoesNotReachAgent(t *testing.T) {
	a, st := newAPI(t)
	a.CustomActivityShell = false
	agent := newFakeAgent(t, a)
	ws := seedWorkspace(t, st)

	pl := createPipeline(t, st, ws.ID, customPipeline(`"command":"echo pwned"`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job = %s, want Failed with the shell gate closed", s)
	}
	if got := agent.statements(); len(got) != 0 {
		t.Fatalf("the agent was asked to run %d statement(s) with the gate closed: %q", len(got), got)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	e, _ := runs[0]["error"].(string)
	if !strings.Contains(e, "FABRIC_CUSTOM_ACTIVITY=off") {
		t.Fatalf("refusal %q does not name the switch that produced it", e)
	}
}

// TestCustomActivityRunsWhenEnabled: the command reaches the agent, its
// extendedProperties are set as environment variables (Batch's own contract
// for them), and the report comes back from the process.
func TestCustomActivityRunsWhenEnabled(t *testing.T) {
	a, st := newAPI(t)
	agent := newFakeAgent(t, a)
	agent.reply = func(code string) map[string]any {
		return map[string]any{"status": "ok", "data": map[string]any{
			"text/plain": `{"exitCode":0,"stdout":"rows=7\n","stderr":""}`}}
	}
	ws := seedWorkspace(t, st)

	pl := createPipeline(t, st, ws.ID, customPipeline(
		`"command":"python load.py --rows 7","extendedProperties":{"STAGE":"silver"}`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
		t.Fatalf("job = %s; runs=%+v", s, runs)
	}
	code := strings.Join(agent.statements(), "\n")
	if !strings.Contains(code, "python load.py --rows 7") {
		t.Fatalf("the command never reached the agent: %s", code)
	}
	if !strings.Contains(code, "subprocess.run") || !strings.Contains(code, "shell=True") {
		t.Fatalf("the command was not run as a process: %s", code)
	}
	// Batch delivers extendedProperties as environment variables; a value that
	// is parsed and dropped would pass a success-only assertion.
	if !strings.Contains(code, "STAGE") || !strings.Contains(code, "silver") {
		t.Fatalf("extendedProperties never reached the environment: %s", code)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	out := outputOf(runs, "Batch")
	if out["stdout"] != "rows=7\n" {
		t.Fatalf("stdout did not surface: %+v", out)
	}
	if s, _ := out["executedBy"].(string); !strings.Contains(s, "not an Azure Batch node") {
		t.Fatalf("output does not say which machine answered: %+v", out)
	}
}

// TestCustomActivityNonZeroExitFails: the command's own exit status decides
// the activity. A shell command that fails while the agent statement succeeds
// is the case a naive implementation reports as green.
func TestCustomActivityNonZeroExitFails(t *testing.T) {
	a, st := newAPI(t)
	agent := newFakeAgent(t, a)
	agent.reply = func(code string) map[string]any {
		return map[string]any{"status": "ok", "data": map[string]any{
			"text/plain": `{"exitCode":2,"stdout":"","stderr":"load.py: no such file"}`}}
	}
	ws := seedWorkspace(t, st)

	pl := createPipeline(t, st, ws.ID, customPipeline(`"command":"python load.py"`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job = %s, want Failed on a non-zero exit", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	e, _ := runs[0]["error"].(string)
	if !strings.Contains(e, "exited 2") || !strings.Contains(e, "no such file") {
		t.Fatalf("error %q carries neither the exit code nor the command's own output", e)
	}
}

// TestCustomActivityUnparseableReportFails: if nothing readable comes back,
// the exit status is UNKNOWN — which is not success. Reporting Succeeded here
// would be the fabrication this repo keeps hunting.
func TestCustomActivityUnparseableReportFails(t *testing.T) {
	a, st := newAPI(t)
	agent := newFakeAgent(t, a)
	agent.reply = func(code string) map[string]any {
		return map[string]any{"status": "ok", "data": map[string]any{"text/plain": "who knows"}}
	}
	ws := seedWorkspace(t, st)

	pl := createPipeline(t, st, ws.ID, customPipeline(`"command":"true"`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job = %s, want Failed when the exit status is unknown", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	if e, _ := runs[0]["error"].(string); !strings.Contains(e, "not the same as success") {
		t.Fatalf("error %q does not distinguish unknown from succeeded", e)
	}
}

// TestCustomActivityRefusesByName: the Batch-node features the emulator has no
// equivalent for. Each would otherwise leave a command running in conditions
// the definition does not describe.
func TestCustomActivityRefusesByName(t *testing.T) {
	for _, tc := range []struct{ name, tp, wantErr string }{
		{"resourceLinkedService",
			`"command":"x","resourceLinkedService":{"referenceName":"blob","type":"LinkedServiceReference"}`,
			"stages resource files"},
		{"folderPath", `"command":"x","folderPath":"tasks/etl"`, "resource files are staged"},
		{"autoUserSpecification", `"command":"x","autoUserSpecification":"admin"`, "no user model"},
		{"referenceObjects", `"command":"x","referenceObjects":{"datasets":[]}`, "models neither"},
		{"missing command", `"extendedProperties":{"A":"1"}`, "command is required"},
		{"command expr", `"command":"@nope(1)"`, "command"},
		{"extendedProperties not an object", `"command":"x","extendedProperties":["a"]`,
			"must be an object"},
		{"extendedProperty expr", `"command":"x","extendedProperties":{"K":"@nope(1)"}`,
			`extendedProperty "K"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, st := newAPI(t)
			newFakeAgent(t, a)
			a.CustomActivityShell = true // refusals must hold even when enabled
			ws := seedWorkspace(t, st)
			pl := createPipeline(t, st, ws.ID, customPipeline(tc.tp))
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

// TestCustomActivityNoAgentIsHonest: enabled but with nowhere to run it.
func TestCustomActivityNoAgentIsHonest(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	pl := createPipeline(t, st, ws.ID, customPipeline(`"command":"echo hi"`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job = %s, want Failed with no agent", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	if e, _ := runs[0]["error"].(string); !strings.Contains(e, "no Spark agent is configured") {
		t.Fatalf("error %q does not name the missing agent", e)
	}
}

// TestCustomActivityAgentTransportFailure: unreachable agent, enabled gate.
func TestCustomActivityAgentTransportFailure(t *testing.T) {
	a, st := newAPI(t)
	brokenAgent(t, a)
	ws := seedWorkspace(t, st)
	pl := createPipeline(t, st, ws.ID, customPipeline(`"command":"echo hi"`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job = %s, want Failed", s)
	}
}

// TestCustomActivityStatementErrorFails: the agent itself reporting an error.
func TestCustomActivityStatementErrorFails(t *testing.T) {
	a, st := newAPI(t)
	agent := newFakeAgent(t, a)
	agent.reply = func(code string) map[string]any {
		return map[string]any{"status": "error", "ename": "OSError", "evalue": "fork failed"}
	}
	ws := seedWorkspace(t, st)
	pl := createPipeline(t, st, ws.ID, customPipeline(`"command":"echo hi"`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job = %s, want Failed", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	if e, _ := runs[0]["error"].(string); !strings.Contains(e, "fork failed") {
		t.Fatalf("error %q does not carry the agent's message", e)
	}
}

// TestLastJSONLineFindsTheReport: the command's own stdout precedes the
// runner's report, so the parser must take the LAST JSON object — a command
// that prints JSON of its own must not be mistaken for the report.
func TestLastJSONLineFindsTheReport(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`{"exitCode":0}`, `{"exitCode":0}`},
		{"noise\n{\"mine\":1}\n{\"exitCode\":0}", `{"exitCode":0}`},
		{"just text", ""},
		{"", ""},
		{"{\"exitCode\":0}\ntrailing text", `{"exitCode":0}`},
	} {
		if got := lastJSONLine(tc.in); got != tc.want {
			t.Errorf("lastJSONLine(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
