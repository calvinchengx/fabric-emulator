package tds

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

// TestSQLServerBackendQuery exercises the backend's row-materialisation logic
// against a real database/sql *DB (in-memory SQLite — no CGO, already a
// dependency). This covers the same Query path the SQL Server backend uses:
// column names, per-row scanning, []byte→string normalisation, NULLs, and the
// no-column (DDL/DML) case.
func TestSQLServerBackendQuery(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	be := &sqlServerBackend{db: db}
	ctx := context.Background()

	// A statement with no result set → a Result with no columns.
	res, err := be.Query(ctx, "CREATE TABLE t (region TEXT, amount INT, blob BLOB)")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(res.Columns) != 0 {
		t.Fatalf("DDL should yield no columns, got %v", res.Columns)
	}
	if _, err := be.Query(ctx, "INSERT INTO t VALUES ('us', 80, x'0102'), (NULL, 60, NULL)"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// A SELECT → columns + rows, with a NULL and a BLOB (→ string).
	res, err = be.Query(ctx, "SELECT region, amount, blob FROM t ORDER BY amount")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(res.Columns) != 3 || res.Columns[0].Name != "region" || res.Columns[2].Name != "blob" {
		t.Fatalf("columns = %+v", res.Columns)
	}
	if len(res.Rows) != 2 {
		t.Fatalf("rows = %d", len(res.Rows))
	}
	// Row order by amount: (NULL,60,NULL) then (us,80,blob).
	if res.Rows[0][0] != nil {
		t.Errorf("row0 region should be NULL, got %v", res.Rows[0][0])
	}
	if _, ok := res.Rows[1][2].(string); !ok {
		t.Errorf("blob should be normalised to string, got %T", res.Rows[1][2])
	}

	// A query error surfaces.
	if _, err := be.Query(ctx, "SELECT * FROM nope"); err == nil {
		t.Error("expected error for missing table")
	}
}

// TestNewSQLServerBackend confirms the constructor opens without dialing.
func TestNewSQLServerBackend(t *testing.T) {
	be, err := NewSQLServerBackend("sqlserver://sa:x@127.0.0.1:11433?database=warehouse")
	if err != nil || be == nil {
		t.Fatalf("NewSQLServerBackend: be=%v err=%v", be, err)
	}
}
