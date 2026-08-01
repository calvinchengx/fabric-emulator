package tds

import (
	"context"
	"github.com/calvinchengx/fabric-emulator/internal/testsupport"
	"testing"
)

// TestSQLServerBackendQuery exercises the backend's row-materialisation logic
// against a real SQL Server. This covers the same Query path production uses:
// column names, per-row scanning, []byte→string normalisation, NULLs, and the
// no-column (DDL/DML) case.
//
// It was written against SQLite and declared its columns as TEXT/BLOB, neither
// of which SQL Server has. That went unnoticed for as long as the only backend
// under test was the one that accepted them.
func TestSQLServerBackendQuery(t *testing.T) {
	db := testsupport.OpenMSSQL(t)
	be := &sqlServerBackend{db: db}
	ctx := context.Background()

	// A statement with no result set → a Result with no columns.
	res, err := be.Query(ctx, "CREATE TABLE t (region NVARCHAR(50), amount INT, blob VARBINARY(50))")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(res.Columns) != 0 {
		t.Fatalf("DDL should yield no columns, got %v", res.Columns)
	}
	if _, err := be.Query(ctx, "INSERT INTO t VALUES ('us', 80, 0x0102), (NULL, 60, NULL)"); err != nil {
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
	// A malformed DSN errors at open (go-mssqldb parses eagerly via DriverContext).
	if _, err := NewSQLServerBackend("sqlserver://%zz"); err == nil {
		t.Error("malformed DSN was accepted")
	}
}
