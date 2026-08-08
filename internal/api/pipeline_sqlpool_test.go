package api

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
	_ "modernc.org/sqlite"
)

// sqlitePool wires a.SQLDB to an in-memory database, as the Script tests do,
// and returns the Warehouse item a definition can name.
func sqlitePool(t *testing.T, a *API, st *store.Store, wsID string) *store.Item {
	t.Helper()
	wh := &store.Item{WorkspaceID: wsID, Type: "Warehouse", DisplayName: "pool"}
	if err := st.CreateItem(wh, nil); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	a.SQLDB = func(_ context.Context, itemID string) (*sql.DB, error) {
		if itemID != wh.ID {
			t.Fatalf("SQLDB called with %q, want %q", itemID, wh.ID)
		}
		return db, nil
	}
	return wh
}

// TestSqlPoolReferenceResolvesToRealSQL: `sqlPool` is the Synapse spelling of
// the target reference, and it must reach the same real database the other
// keys do. Running an actual Script through it — DDL, DML, then a SELECT whose
// rows come back — proves the key resolves rather than merely parsing: a
// reference that resolved to nothing would fail before the first statement.
func TestSqlPoolReferenceResolvesToRealSQL(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	wh := sqlitePool(t, a, st, ws.ID)

	pl := createPipeline(t, st, ws.ID, `{"properties":{"activities":[
        {"name":"Sc","type":"Script","typeProperties":{
          "sqlPool":{"itemId":"`+wh.ID+`"},
          "scripts":[
            {"type":"NonQuery","text":"CREATE TABLE t (id INTEGER, name TEXT)"},
            {"type":"NonQuery","text":"INSERT INTO t VALUES (1,'ada')"},
            {"type":"Query","text":"SELECT name FROM t"}]}}]}}`)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
		t.Fatalf("job = %s; runs=%+v", s, runs)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	sets := outputOf(runs, "Sc")["resultSets"].([]any)
	rows := sets[2].(map[string]any)["rows"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["name"] != "ada" {
		t.Fatalf("the sqlPool reference did not reach a real database: %+v", rows)
	}
}

// TestSqlPoolStoredProcedureDispatches: the type string is the only difference
// from its SqlServerStoredProcedure sibling, and reaching the SHARED
// implementation is what this pins. The proof is the order in which things
// fail: the database reference resolves FIRST, so an activity that gets as far
// as complaining about a missing storedProcedureName has already resolved its
// sqlPool and opened the connection. Falling to the dispatch default instead
// would report Succeeded and never mention the field at all.
func TestSqlPoolStoredProcedureDispatches(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	wh := sqlitePool(t, a, st, ws.ID)

	pl := createPipeline(t, st, ws.ID, `{"properties":{"activities":[
        {"name":"Proc","type":"SqlPoolStoredProcedure","typeProperties":{
          "sqlPool":{"itemId":"`+wh.ID+`"}}}]}}`)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job = %s, want Failed on a missing storedProcedureName", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	e, _ := runs[0]["error"].(string)
	if !strings.Contains(e, "storedProcedureName is required") {
		t.Fatalf("error %q — the activity did not reach the stored-procedure implementation", e)
	}
	if out := outputOf(runs, "Proc"); out["activityType"] != nil {
		t.Fatalf("SqlPoolStoredProcedure fell through to the stubbed default: %+v", out)
	}
}

// TestSqlPoolStoredProcedureSendsRealSQL: the EXEC actually goes to the
// database. SQLite has no stored procedures, so its parse error IS the
// evidence — the statement reached a real engine and was rejected there,
// which no stub could produce. (The successful EXEC path is covered by the
// gated e2e against a real SQL Server.)
func TestSqlPoolStoredProcedureSendsRealSQL(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	wh := sqlitePool(t, a, st, ws.ID)

	pl := createPipeline(t, st, ws.ID, `{"properties":{"activities":[
        {"name":"Proc","type":"SqlPoolStoredProcedure","typeProperties":{
          "sqlPool":{"itemId":"`+wh.ID+`"},
          "storedProcedureName":"dbo.LoadSilver",
          "storedProcedureParameters":{"day":{"value":"2026-08-08","type":"String"}}}}]}}`)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job = %s, want Failed — SQLite cannot EXEC a procedure", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	e, _ := runs[0]["error"].(string)
	if !strings.Contains(e, "stored procedure") || !strings.Contains(e, "syntax error") {
		t.Fatalf("error %q does not look like a real engine rejecting real SQL", e)
	}
}

// TestSqlPoolMissingReferenceIsHonest: no target named at all still fails with
// the reference error, and the message lists sqlPool among the keys it looked
// for — otherwise an author using the Synapse spelling has no way to learn it
// is supported.
func TestSqlPoolMissingReferenceIsHonest(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	sqlitePool(t, a, st, ws.ID)

	pl := createPipeline(t, st, ws.ID, `{"properties":{"activities":[
        {"name":"Proc","type":"SqlPoolStoredProcedure","typeProperties":{
          "storedProcedureName":"dbo.X"}}]}}`)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job = %s, want Failed", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	e, _ := runs[0]["error"].(string)
	if !strings.Contains(e, "no database reference") || !strings.Contains(e, "sqlPool") {
		t.Fatalf("error %q does not list the keys it accepts", e)
	}
}
