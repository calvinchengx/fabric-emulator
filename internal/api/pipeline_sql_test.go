package api

// Non-gated coverage for the Script/StoredProcedure pipeline activities: a.SQLDB
// is wired to an in-memory SQLite database (no real SQL Server needed) to
// exercise resolveDatabaseRef, sqlDB, scriptActivity, and scanRows end to end
// through the real pipeline job API. SQLite has no stored-procedure support, so
// storedProcedureActivity's successful EXEC path is covered separately by the
// gated e2e (TestPipelineSQLActivitiesE2E, against a real SQL Server); here we
// cover its validation branch.

import (
	"context"
	"database/sql"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
	_ "modernc.org/sqlite"
)

func TestPipelineScriptSQLite(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	wh := &store.Item{WorkspaceID: ws.ID, Type: "Warehouse", DisplayName: "wh"}
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

	content := `{"properties":{"activities":[
        {"name":"Sc","type":"Script","typeProperties":{
          "database":{"itemId":"` + wh.ID + `"},
          "scripts":[
            {"type":"NonQuery","text":"CREATE TABLE t (id INTEGER, name TEXT)"},
            {"type":"NonQuery","text":"INSERT INTO t VALUES (1,'ada'),(2,'bob')"},
            {"type":"Query","text":"SELECT id, name FROM t ORDER BY id"}
          ]}}
      ]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		t.Fatalf("job status = %s", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	out := outputOf(runs, "Sc")
	resultSets := out["resultSets"].([]any)
	if len(resultSets) != 3 {
		t.Fatalf("resultSets = %d, want 3", len(resultSets))
	}
	selectRS := resultSets[2].(map[string]any)
	rows := selectRS["rows"].([]any)
	if len(rows) != 2 {
		t.Fatalf("SELECT rows = %d, want 2", len(rows))
	}
	if rows[0].(map[string]any)["name"] != "ada" || rows[1].(map[string]any)["name"] != "bob" {
		t.Fatalf("rows = %+v", rows)
	}
	if rc := resultSets[0].(map[string]any)["rowCount"]; rc.(float64) < 0 && rc.(float64) != 0 {
		// SQLite reports rowCount for DDL as 0; just sanity-check the field exists.
		t.Fatalf("unexpected DDL rowCount = %v", rc)
	}
}

// TestPipelineScriptMalformedDatabaseRef: a non-object "database" value falls
// through resolveDatabaseRef's loop and fails loudly (not a panic).
func TestPipelineScriptMalformedDatabaseRef(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	content := `{"properties":{"activities":[
        {"name":"Sc","type":"Script","typeProperties":{
          "database":"not-an-object",
          "scripts":[{"type":"Query","text":"SELECT 1"}]}}
      ]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("malformed database ref = %s, want Failed", s)
	}
}

// TestPipelineScriptMissingScripts: a Script activity with no scripts array
// fails loudly.
func TestPipelineScriptMissingScripts(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	wh := &store.Item{WorkspaceID: ws.ID, Type: "Warehouse", DisplayName: "wh"}
	if err := st.CreateItem(wh, nil); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	a.SQLDB = func(context.Context, string) (*sql.DB, error) { return db, nil }

	content := `{"properties":{"activities":[
        {"name":"Sc","type":"Script","typeProperties":{"database":{"itemId":"` + wh.ID + `"}}}
      ]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("missing scripts = %s, want Failed", s)
	}
}

// TestPipelineScriptNonQueryError: a failing NonQuery script fails the
// activity with the script index in the error.
func TestPipelineScriptNonQueryError(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	wh := &store.Item{WorkspaceID: ws.ID, Type: "Warehouse", DisplayName: "wh"}
	if err := st.CreateItem(wh, nil); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	a.SQLDB = func(context.Context, string) (*sql.DB, error) { return db, nil }

	content := `{"properties":{"activities":[
        {"name":"Sc","type":"Script","typeProperties":{
          "database":{"itemId":"` + wh.ID + `"},
          "scripts":[{"type":"NonQuery","text":"NOT VALID SQL"}]}}
      ]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("invalid SQL = %s, want Failed", s)
	}
}

// TestPipelineScriptQueryError: a failing Query script fails the activity too.
func TestPipelineScriptQueryError(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	wh := &store.Item{WorkspaceID: ws.ID, Type: "Warehouse", DisplayName: "wh"}
	if err := st.CreateItem(wh, nil); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	a.SQLDB = func(context.Context, string) (*sql.DB, error) { return db, nil }

	content := `{"properties":{"activities":[
        {"name":"Sc","type":"Script","typeProperties":{
          "database":{"itemId":"` + wh.ID + `"},
          "scripts":[{"type":"Query","text":"SELECT * FROM no_such_table"}]}}
      ]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("query against missing table = %s, want Failed", s)
	}
}

// TestPipelineStoredProcedureMissingName: SQLDB resolves fine, but no
// storedProcedureName fails loudly (validation, no real EXEC attempted).
func TestPipelineStoredProcedureMissingName(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	wh := &store.Item{WorkspaceID: ws.ID, Type: "Warehouse", DisplayName: "wh"}
	if err := st.CreateItem(wh, nil); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	a.SQLDB = func(context.Context, string) (*sql.DB, error) { return db, nil }

	content := `{"properties":{"activities":[
        {"name":"Sp","type":"SqlServerStoredProcedure","typeProperties":{
          "database":{"itemId":"` + wh.ID + `"}}}
      ]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("missing storedProcedureName = %s, want Failed", s)
	}
}
