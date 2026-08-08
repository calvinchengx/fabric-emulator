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
	"strings"
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
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Completed" {
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
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
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
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
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
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
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
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("query against missing table = %s, want Failed", s)
	}
}

// TestPipelineScriptNestedLocationAndBlob: the database reference also
// resolves when nested under "location" (the Copy/Lookup shape), and blob
// results come back as text, not base64.
func TestPipelineScriptNestedLocationAndBlob(t *testing.T) {
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
          "database":{"location":{"workspaceId":"@null","itemId":"` + wh.ID + `"}},
          "scripts":[{"type":"Query","text":"SELECT CAST('ab' AS BLOB) AS b"}]}}
      ]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		t.Fatalf("nested-location script = %s, want Completed", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	rows := outputOf(runs, "Sc")["resultSets"].([]any)[0].(map[string]any)["rows"].([]any)
	if rows[0].(map[string]any)["b"] != "ab" {
		t.Fatalf("blob column = %+v, want text 'ab'", rows[0])
	}
}

// TestPipelineScriptDatabaseRefErrors: expression failures inside the
// database reference, and a reference without an itemId, fail the activity.
func TestPipelineScriptDatabaseRefErrors(t *testing.T) {
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

	for name, ref := range map[string]string{
		"bad workspaceId expression": `{"workspaceId":"@nosuchfunc()","itemId":"` + wh.ID + `"}`,
		"bad itemId expression":      `{"itemId":"@nosuchfunc()"}`,
		"no itemId":                  `{"workspaceId":"` + ws.ID + `"}`,
	} {
		content := `{"properties":{"activities":[
            {"name":"Sc","type":"Script","typeProperties":{
              "database":` + ref + `,
              "scripts":[{"type":"Query","text":"SELECT 1"}]}}
          ]}}`
		pl := createPipeline(t, st, ws.ID, content)
		_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
		if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
			t.Errorf("%s = %s, want Failed", name, s)
		}
	}
}

// TestPipelineStoredProcedureParams: named parameters build the EXEC call
// (@name = @pN in some order); SQLite has no EXEC, so the real query fails —
// which is exactly the path under test (parameter marshalling + query error).
func TestPipelineStoredProcedureParams(t *testing.T) {
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
          "database":{"itemId":"` + wh.ID + `"},
          "storedProcedureName":"dbo.upsert",
          "storedProcedureParameters":{"id":{"value":1,"type":"Int"},"name":{"value":"ada"}}}}
      ]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("EXEC against SQLite = %s, want Failed (no stored procedures)", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	if e, _ := runs[0]["error"].(string); !strings.Contains(e, `stored procedure "Sp"`) {
		t.Fatalf("error = %q, want the stored-procedure wrap", e)
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
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("missing storedProcedureName = %s, want Failed", s)
	}
}
