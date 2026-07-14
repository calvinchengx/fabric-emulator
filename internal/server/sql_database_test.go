package server_test

// Fabric SQL Database e2e (gated on WAREHOUSE_MSSQL_DSN): a client writes T-SQL
// over the read-write TDS endpoint into the item's own SQL Server database, the
// control-plane mirror hook snapshots it to OneLake as Delta, and the mirrored
// Delta reads back with the rows that were written — the SQL→OneLake mirroring
// that makes an operational database queryable as Delta. Our own pure-Go Delta
// reader is the oracle here; the independent delta-rs oracle lives in e2e/.

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	entra "github.com/calvinchengx/entra-emulator/emulator"
	"github.com/calvinchengx/fabric-emulator/internal/config"
	"github.com/calvinchengx/fabric-emulator/internal/server"
	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/calvinchengx/fabric-emulator/internal/warehouse"
	mssql "github.com/microsoft/go-mssqldb"
)

func TestSQLDatabaseMirror(t *testing.T) {
	dsn := os.Getenv("WAREHOUSE_MSSQL_DSN")
	if dsn == "" {
		t.Skip("set WAREHOUSE_MSSQL_DSN (a reachable SQL Server) to run the SQL Database mirror e2e")
	}

	emu := entra.StartT(t)
	cfg := &config.Config{
		EntraIssuer:     emu.Origin + "/" + emu.TenantID + "/v2.0",
		SQLTDSAddr:      "127.0.0.1:0",
		WarehouseSQLURL: dsn,
	}
	if err := cfg.Finish(); err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(cfg, emu.HTTPClient())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })

	ws := &store.Workspace{DisplayName: "sqldb-ws"}
	if err := srv.Store.CreateWorkspace(ws, store.Principal{ID: entra.DaemonClientID, Type: "ServicePrincipal"}); err != nil {
		t.Fatal(err)
	}
	db := &store.Item{WorkspaceID: ws.ID, Type: "SQLDatabase", DisplayName: "opsdb"}
	if err := srv.Store.CreateItem(db, nil); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go srv.TDS.Serve(ln)
	port := ln.Addr().(*net.TCPAddr).Port
	token := forgeAppToken(t, emu, "https://database.windows.net")
	conn := func() *sql.DB {
		d := fmt.Sprintf("server=127.0.0.1;port=%d;database=%s;encrypt=disable;dial timeout=5", port, db.ID)
		c, err := mssql.NewAccessTokenConnector(d, func() (string, error) { return token, nil })
		if err != nil {
			t.Fatal(err)
		}
		return sql.OpenDB(c)
	}()
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// --- Write into the SQL Database over TDS (read-write; retry cold start). ---
	var lastErr error
	for i := 0; i < 60; i++ {
		if _, err := conn.ExecContext(ctx, "CREATE TABLE dbo.customers (id INT, name NVARCHAR(50), active BIT)"); err != nil {
			lastErr = err
			time.Sleep(time.Second)
			continue
		}
		lastErr = nil
		break
	}
	if lastErr != nil {
		t.Fatalf("CREATE TABLE: %v", lastErr)
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO dbo.customers VALUES (1, 'ada', 1), (2, 'bob', 0), (3, NULL, 1)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	// --- Trigger the mirror (the control-plane hook the API exposes). ---
	if srv.API.MirrorItem == nil {
		t.Fatal("MirrorItem hook not wired (needs a warehouse SQL backend)")
	}
	if err := srv.API.MirrorItem(ctx, db.ID); err != nil {
		t.Fatalf("mirror: %v", err)
	}

	// --- The mirrored Delta reads back with exactly the rows written. ---
	tbl, err := warehouse.ReadDeltaTable(srv.Store, db.ID, "customers")
	if err != nil {
		t.Fatalf("ReadDeltaTable(customers): %v", err)
	}
	if len(tbl.Rows) != 3 {
		t.Fatalf("mirrored rows = %d, want 3", len(tbl.Rows))
	}
	col := func(name string) int {
		for i, c := range tbl.Columns {
			if c == name {
				return i
			}
		}
		t.Fatalf("mirrored table missing column %q (got %v)", name, tbl.Columns)
		return -1
	}
	idc, namec, activec := col("id"), col("name"), col("active")

	var sumID int64
	trueActives, nullNames := 0, 0
	for _, r := range tbl.Rows {
		if v, ok := r[idc].(int64); ok {
			sumID += v
		}
		if b, ok := r[activec].(bool); ok && b {
			trueActives++
		}
		if r[namec] == nil {
			nullNames++
		}
	}
	if sumID != 6 { // 1+2+3
		t.Errorf("sum(id) = %d, want 6", sumID)
	}
	if trueActives != 2 { // rows 1 and 3
		t.Errorf("active=true count = %d, want 2", trueActives)
	}
	if nullNames != 1 { // row 3's NULL name survived
		t.Errorf("NULL name count = %d, want 1", nullNames)
	}
}
