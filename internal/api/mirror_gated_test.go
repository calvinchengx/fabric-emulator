package api

// Gated e2e (WAREHOUSE_MSSQL_DSN): refreshMirroredDatabase reaches a genuinely
// EXTERNAL SQL Server database — created directly on the SQL Server, bypassing
// the emulator's own per-item TDS routing entirely — via a Connection carrying
// Basic credentials, and mirrors its tables to OneLake as Delta. This reuses
// the exact same mirror writer (warehouse.Mirror) the Fabric SQL Database uses:
// same code, external source.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/calvinchengx/fabric-emulator/internal/tds"
	"github.com/calvinchengx/fabric-emulator/internal/warehouse"
	"github.com/microsoft/go-mssqldb/msdsn"
)

func TestRefreshMirroredDatabaseE2E(t *testing.T) {
	dsn := os.Getenv("WAREHOUSE_MSSQL_DSN")
	if dsn == "" {
		t.Skip("set WAREHOUSE_MSSQL_DSN (a reachable SQL Server) to run the MirroredDatabase e2e")
	}
	cfg, err := msdsn.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	host := cfg.Host
	if cfg.Port != 0 {
		host = fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	}
	ctx := context.Background()

	// Create a fresh database directly on the SQL Server — a genuinely external
	// source, never routed through the emulator's own per-item TDS backend —
	// and seed a table in it.
	const extDB = "mirroredext"
	master, err := tds.NewSQLServerBackend(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := master.DB("").ExecContext(ctx, "IF DB_ID('"+extDB+"') IS NULL CREATE DATABASE ["+extDB+"]"); err != nil {
		t.Fatal(err)
	}
	tblDSN := fmt.Sprintf("sqlserver://%s:%s@%s?database=%s&encrypt=disable", cfg.User, cfg.Password, host, extDB)
	tblBE, err := tds.NewSQLServerBackend(tblDSN)
	if err != nil {
		t.Fatal(err)
	}
	tdb := tblBE.DB("")
	if _, err := tdb.ExecContext(ctx, "IF OBJECT_ID('dbo.orders','U') IS NOT NULL DROP TABLE dbo.orders"); err != nil {
		t.Fatal(err)
	}
	if _, err := tdb.ExecContext(ctx, "CREATE TABLE dbo.orders (id INT, total FLOAT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := tdb.ExecContext(ctx, "INSERT INTO dbo.orders VALUES (1, 9.5), (2, 20.0)"); err != nil {
		t.Fatal(err)
	}

	// The emulator side: a MirroredDatabase item + a Connection pointing at the
	// external database above, then trigger the mirror.
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	md := &store.Item{WorkspaceID: ws.ID, Type: "MirroredDatabase", DisplayName: "mirror"}
	if err := st.CreateItem(md, nil); err != nil {
		t.Fatal(err)
	}
	connDetails, _ := json.Marshal(sqlConnDetails{Server: host, Database: extDB})
	credsJSON, _ := json.Marshal(connectionCredentials{CredentialType: "Basic", Username: cfg.User, Password: cfg.Password})
	conn := &store.Connection{DisplayName: "ext", Details: connDetails, CredentialsJSON: string(credsJSON)}
	if err := st.CreateConnection(conn); err != nil {
		t.Fatal(err)
	}

	body := fmt.Sprintf(`{"connectionId":"%s"}`, conn.ID)
	w := do(a.refreshMirroredDatabase, admin, "POST", body, map[string]string{"wid": ws.ID, "iid": md.ID})
	if w.Code != http.StatusOK {
		t.Fatalf("refreshMirroredDatabase: %d %s", w.Code, w.Body)
	}

	// The mirrored Delta reads back with exactly the rows the external
	// database's table held.
	tbl, err := warehouse.ReadDeltaTable(st, md.ID, "orders")
	if err != nil {
		t.Fatalf("ReadDeltaTable(orders): %v", err)
	}
	if len(tbl.Rows) != 2 {
		t.Fatalf("mirrored rows = %d, want 2", len(tbl.Rows))
	}
	idc, totalc := -1, -1
	for i, c := range tbl.Columns {
		switch c {
		case "id":
			idc = i
		case "total":
			totalc = i
		}
	}
	if idc < 0 || totalc < 0 {
		t.Fatalf("mirrored columns = %v, want id and total", tbl.Columns)
	}
	var sumID int64
	var sumTotal float64
	for _, row := range tbl.Rows {
		if v, ok := row[idc].(int64); ok {
			sumID += v
		}
		if v, ok := row[totalc].(float64); ok {
			sumTotal += v
		}
	}
	if sumID != 3 { // 1+2
		t.Errorf("sum(id) = %d, want 3", sumID)
	}
	if sumTotal != 29.5 { // 9.5+20.0
		t.Errorf("sum(total) = %v, want 29.5", sumTotal)
	}
}
