package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

func adxPipeline(tp string) string {
	return `{"properties":{"activities":[
      {"name":"Cmd","type":"AzureDataExplorerCommand","typeProperties":{` + tp + `}}]}}`
}

// attachEngine's fake records the command string without parsing KQL, so an
// invalid command passes here just as happily as a valid one. Keep the commands
// below accepted by real Kusto anyway: these tests read as the reference for
// what an AzureDataExplorerCommand activity should contain, and a command that
// earns a 400 from the engine is a poor thing to copy. `kind` and its fellow
// KQL keywords are not usable as column names; the identifiers used here are
// the ones e2e/rti/driver.py proves against Microsoft's kustainer.

// TestADXCommandReachesTheRealEngine: the command the definition names is the
// command the engine receives, against the ISOLATED engine database for that
// KQL Database item — not the display name, and not some other item's. A test
// that only checked the job completed would pass on an activity that sent
// nothing at all, so the engine's own record of the call is what is asserted.
func TestADXCommandReachesTheRealEngine(t *testing.T) {
	a, st := newAPI(t)
	engine := attachEngine(t, a)
	ws := seedWorkspace(t, st)
	_, db := seedEventhouse(t, a, ws.ID, "eh")

	pl := createPipeline(t, st, ws.ID, adxPipeline(
		`"command":".create table Events (DeviceId:string, At:datetime)",
         "database":{"itemId":"`+db.ID+`"}`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
		t.Fatalf("job = %s; runs=%+v", s, runs)
	}

	calls := engine.sent()
	seen := -1
	for i, c := range calls {
		if strings.HasPrefix(c.csl, ".create table Events") {
			seen = i
		}
	}
	if seen < 0 {
		t.Fatalf("the command never reached the engine: %+v", calls)
	}
	if want := engineDatabaseName(db.ID); calls[seen].db != want {
		t.Fatalf("command ran against db %q, want the isolated %q", calls[seen].db, want)
	}

	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	out := outputOf(runs, "Cmd")
	// The engine's internal name must not leak into the activity output — the
	// same mapping-home the data-plane relay does. The fake echoes body.DB in
	// its rows precisely so this can be checked rather than assumed.
	if s, _ := out["database"].(string); s != db.DisplayName {
		t.Fatalf("output names %q, want the Fabric display name %q", s, db.DisplayName)
	}
	if raw := out["tables"]; raw == nil {
		t.Fatalf("the engine's result was dropped: %+v", out)
	}
	rendered, _ := json.Marshal(out)
	if strings.Contains(string(rendered), engineDatabaseName(db.ID)) {
		t.Fatalf("the engine's internal database name leaked into the output: %s", rendered)
	}
	if s, _ := out["executedBy"].(string); !strings.Contains(s, "not an Azure Data Explorer cluster") {
		t.Fatalf("output does not say which engine answered: %+v", out)
	}
}

// TestADXCommandRefusesAQuery: the schema's own rule. A KQL query relayed to
// /v1/rest/mgmt is not a control command, and the engine's complaint would
// describe the engine rather than the mistake.
func TestADXCommandRefusesAQuery(t *testing.T) {
	a, st := newAPI(t)
	engine := attachEngine(t, a)
	ws := seedWorkspace(t, st)
	_, db := seedEventhouse(t, a, ws.ID, "eh")

	pl := createPipeline(t, st, ws.ID, adxPipeline(
		`"command":"Events | summarize count() by DeviceId","database":{"itemId":"`+db.ID+`"}`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job = %s, want Failed on a query", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	if e, _ := runs[0]["error"].(string); !strings.Contains(e, "is not a control command") {
		t.Fatalf("error %q does not name the mistake", e)
	}
	// Nothing was relayed: the refusal happens before the engine is touched,
	// so a rejected query cannot half-run.
	for _, c := range engine.sent() {
		if strings.Contains(c.csl, "summarize") {
			t.Fatalf("the query was relayed anyway: %+v", c)
		}
	}
}

// TestADXCommandRefusesTheWrongItemType: naming a warehouse where a KQL
// database belongs must fail by name. Left unchecked, ensureKustoDatabase
// would happily create an engine database from the warehouse's id and the
// command would "succeed" against a target nobody meant.
func TestADXCommandRefusesTheWrongItemType(t *testing.T) {
	a, st := newAPI(t)
	engine := attachEngine(t, a)
	ws := seedWorkspace(t, st)
	wh := &store.Item{WorkspaceID: ws.ID, Type: "Warehouse", DisplayName: "wh"}
	if err := st.CreateItem(wh, nil); err != nil {
		t.Fatal(err)
	}

	pl := createPipeline(t, st, ws.ID, adxPipeline(
		`"command":".show tables","database":{"itemId":"`+wh.ID+`"}`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job = %s, want Failed", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	e, _ := runs[0]["error"].(string)
	if !strings.Contains(e, "which is a Warehouse") || !strings.Contains(e, "wrong target") {
		t.Fatalf("error %q does not name the type mismatch", e)
	}
	if n := len(engine.sent()); n != 0 {
		t.Fatalf("the engine was called %d time(s) for a mistyped target", n)
	}
}

// TestADXCommandNoEngineIsHonest: attached to nothing, say so — never a
// success for a command that ran nowhere.
func TestADXCommandNoEngineIsHonest(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	_, db := seedEventhouse(t, a, ws.ID, "eh")
	a.KQLURL = nil

	pl := createPipeline(t, st, ws.ID, adxPipeline(
		`"command":".show tables","database":{"itemId":"`+db.ID+`"}`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job = %s, want Failed with no engine", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	if e, _ := runs[0]["error"].(string); !strings.Contains(e, "no Kusto engine is attached") {
		t.Fatalf("error %q does not name the missing engine", e)
	}
}

// TestADXCommandEngineErrorFailsTheActivity: the engine's verdict is the
// activity's verdict. A control command that the engine rejects must not be
// reported as having run.
func TestADXCommandEngineErrorFailsTheActivity(t *testing.T) {
	a, st := newAPI(t)
	engine := attachEngine(t, a)
	ws := seedWorkspace(t, st)
	_, db := seedEventhouse(t, a, ws.ID, "eh")
	// Warm the engine database first, so the 500 below lands on the COMMAND
	// rather than on the create-on-first-use that precedes it — otherwise the
	// test would pass while proving nothing about the command's own verdict.
	if err := a.ensureKustoDatabase(context.Background(), engineDatabaseName(db.ID)); err != nil {
		t.Fatal(err)
	}
	engine.mu.Lock()
	engine.status = 500
	engine.mu.Unlock()

	pl := createPipeline(t, st, ws.ID, adxPipeline(
		`"command":".drop table Nope","database":{"itemId":"`+db.ID+`"}`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job = %s, want Failed when the engine rejects the command", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	if e, _ := runs[0]["error"].(string); !strings.Contains(e, "500") {
		t.Fatalf("error %q does not carry the engine's status", e)
	}
}

// TestADXCommandInputSurface: the shapes a definition can get wrong. Each
// asserts the message names the field, so the author knows which line to fix.
func TestADXCommandInputSurface(t *testing.T) {
	for _, tc := range []struct{ name, tp, wantErr string }{
		{"missing command", `"database":{"itemId":"DB"}`, "command is required"},
		{"empty command", `"command":"  ","database":{"itemId":"DB"}`, "command is required"},
		{"command expr", `"command":"@nope(1)","database":{"itemId":"DB"}`, "command"},
		{"missing database", `"command":".show tables"`, "no database reference"},
		{"unknown database", `"command":".show tables","database":{"itemId":"11111111-1111-1111-1111-111111111111"}`,
			"unknown item"},
		{"bad commandTimeout", `"command":".show tables","database":{"itemId":"DB"},"commandTimeout":"soon"`,
			"is not D.HH:MM:SS"},
		{"commandTimeout expr", `"command":".show tables","database":{"itemId":"DB"},"commandTimeout":"@nope(1)"`,
			"commandTimeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, st := newAPI(t)
			attachEngine(t, a)
			ws := seedWorkspace(t, st)
			_, db := seedEventhouse(t, a, ws.ID, "eh")
			pl := createPipeline(t, st, ws.ID,
				adxPipeline(strings.ReplaceAll(tc.tp, `"DB"`, `"`+db.ID+`"`)))
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

// TestADXCommandHonoursCommandTimeout: a well-formed D.HH:MM:SS is accepted
// and the command still runs. The timeout bounds the call rather than
// rejecting it, and nothing here should silently drop the command.
func TestADXCommandHonoursCommandTimeout(t *testing.T) {
	a, st := newAPI(t)
	engine := attachEngine(t, a)
	ws := seedWorkspace(t, st)
	_, db := seedEventhouse(t, a, ws.ID, "eh")

	pl := createPipeline(t, st, ws.ID, adxPipeline(
		`"command":".show tables","database":{"itemId":"`+db.ID+`"},"commandTimeout":"00:05:00"`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
		t.Fatalf("job = %s; runs=%+v", s, runs)
	}
	found := false
	for _, c := range engine.sent() {
		if c.csl == ".show tables" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the command did not run under a valid timeout: %+v", engine.sent())
	}
}

// TestADXCommandEngineCannotCreateTheDatabase: the create-on-first-use step is
// part of the path, and its failure must surface as the activity's failure
// rather than a command reported against a database that does not exist.
func TestADXCommandEngineCannotCreateTheDatabase(t *testing.T) {
	a, st := newAPI(t)
	engine := attachEngine(t, a)
	ws := seedWorkspace(t, st)
	_, db := seedEventhouse(t, a, ws.ID, "eh")
	engine.mu.Lock()
	engine.failCreate = true
	engine.mu.Unlock()

	pl := createPipeline(t, st, ws.ID, adxPipeline(
		`"command":".show tables","database":{"itemId":"`+db.ID+`"}`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job = %s, want Failed when the engine database cannot be created", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	if e, _ := runs[0]["error"].(string); !strings.Contains(e, "creating database") {
		t.Fatalf("error %q does not name the step that failed", e)
	}
}

// TestADXCommandEngineUnreachable: the transport itself failing. The database
// is marked already-created so the failure lands on the COMMAND's own call —
// otherwise this would only re-test the branch above.
func TestADXCommandEngineUnreachable(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	_, db := seedEventhouse(t, a, ws.ID, "eh")
	if err := a.SetKQLBackend("http://127.0.0.1:1"); err != nil {
		t.Fatal(err)
	}
	a.kqlMu.Lock()
	a.kqlDatabases = map[string]bool{engineDatabaseName(db.ID): true}
	a.kqlMu.Unlock()

	pl := createPipeline(t, st, ws.ID, adxPipeline(
		`"command":".show tables","database":{"itemId":"`+db.ID+`"}`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job = %s, want Failed with an unreachable engine", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	if e, _ := runs[0]["error"].(string); !strings.Contains(e, "data explorer command") {
		t.Fatalf("error %q does not name the activity", e)
	}
}
