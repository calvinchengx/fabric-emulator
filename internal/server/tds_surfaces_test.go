package server_test

// Warehouse T4a e2e: the two surfaces over one TDS front. A **Warehouse** is
// read-write (the client's own CREATE/INSERT/SELECT run on the sidecar); a
// **Lakehouse** SQL analytics endpoint is read-only (reflected Delta, writes
// rejected); and the two are isolated (each Fabric item is its own SQL Server
// database). Gated on WAREHOUSE_MSSQL_DSN like the other warehouse e2es.

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	entra "github.com/calvinchengx/entra-emulator/emulator"
	"github.com/calvinchengx/fabric-emulator/internal/config"
	"github.com/calvinchengx/fabric-emulator/internal/server"
	"github.com/calvinchengx/fabric-emulator/internal/store"
	mssql "github.com/microsoft/go-mssqldb"
	"github.com/parquet-go/parquet-go"
)

func TestWarehouseTwoSurfaces(t *testing.T) {
	dsn := os.Getenv("WAREHOUSE_MSSQL_DSN")
	if dsn == "" {
		t.Skip("set WAREHOUSE_MSSQL_DSN (a reachable SQL Server) to run the two-surface e2e")
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

	ws := &store.Workspace{DisplayName: "two-surface-ws"}
	if err := srv.Store.CreateWorkspace(ws, store.Principal{ID: entra.DaemonClientID, Type: "ServicePrincipal"}); err != nil {
		t.Fatal(err)
	}
	lake := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lake"}
	wh := &store.Item{WorkspaceID: ws.ID, Type: "Warehouse", DisplayName: "wh"}
	for _, it := range []*store.Item{lake, wh} {
		if err := srv.Store.CreateItem(it, nil); err != nil {
			t.Fatal(err)
		}
	}
	// Seed the lakehouse's Delta table (sales).
	var buf bytes.Buffer
	pw := parquet.NewGenericWriter[whRow](&buf)
	if _, err := pw.Write([]whRow{{"us", 80}, {"eu", 60}}); err != nil {
		t.Fatal(err)
	}
	_ = pw.Close()
	put := func(rel string, content []byte) {
		if err := srv.Store.CreateOneLakePath(&store.OneLakePath{
			WorkspaceID: ws.ID, ItemID: lake.ID, RelPath: rel, Content: content}, false); err != nil {
			t.Fatal(err)
		}
	}
	put("Tables/sales/part-0.parquet", buf.Bytes())
	put("Tables/sales/_delta_log/00000000000000000000.json", []byte(`{"add":{"path":"part-0.parquet"}}`))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go srv.TDS.Serve(ln)
	port := ln.Addr().(*net.TCPAddr).Port
	token := forgeAppToken(t, emu, "https://database.windows.net")
	open := func(database string) *sql.DB {
		d := fmt.Sprintf("server=127.0.0.1;port=%d;database=%s;encrypt=disable;dial timeout=5", port, database)
		c, err := mssql.NewAccessTokenConnector(d, func() (string, error) { return token, nil })
		if err != nil {
			t.Fatal(err)
		}
		return sql.OpenDB(c)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// --- Warehouse: read-write (retry for SQL Server cold start) ---
	whdb := open(wh.ID)
	defer whdb.Close()
	var lastErr error
	for i := 0; i < 60; i++ {
		if _, err := whdb.ExecContext(ctx, "CREATE TABLE dbo.metrics (id INT, note NVARCHAR(20))"); err != nil {
			lastErr = err
			time.Sleep(time.Second)
			continue
		}
		lastErr = nil
		break
	}
	if lastErr != nil {
		t.Fatalf("warehouse CREATE TABLE: %v", lastErr)
	}
	if _, err := whdb.ExecContext(ctx, "INSERT INTO dbo.metrics VALUES (1, 'a'), (2, 'b')"); err != nil {
		t.Fatalf("warehouse INSERT: %v", err)
	}
	var n int
	if err := whdb.QueryRowContext(ctx, "SELECT COUNT(*) FROM dbo.metrics").Scan(&n); err != nil {
		t.Fatalf("warehouse SELECT: %v", err)
	}
	if n != 2 {
		t.Fatalf("warehouse row count = %d, want 2", n)
	}

	// --- Lakehouse: read-only. SELECT works; a write is rejected. ---
	lkdb := open(lake.ID)
	defer lkdb.Close()
	var total int64
	if err := lkdb.QueryRowContext(ctx, "SELECT SUM(amount) FROM [sales]").Scan(&total); err != nil {
		t.Fatalf("lakehouse SELECT: %v", err)
	}
	if total != 140 {
		t.Fatalf("lakehouse SUM(amount) = %d, want 140", total)
	}
	// Type fidelity: the reflected INT column comes back as an integer type, not text.
	tr, err := lkdb.QueryContext(ctx, "SELECT amount FROM [sales]")
	if err != nil {
		t.Fatal(err)
	}
	cts, _ := tr.ColumnTypes()
	if strings.Contains(strings.ToUpper(cts[0].DatabaseTypeName()), "CHAR") {
		t.Errorf("reflected amount column is text (%s) — type fidelity lost", cts[0].DatabaseTypeName())
	}
	tr.Close()
	if _, err := lkdb.ExecContext(ctx, "INSERT INTO [sales] VALUES ('xx', 1)"); err == nil {
		t.Fatal("lakehouse INSERT succeeded — the analytics endpoint must be read-only")
	} else if !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("lakehouse write error = %q; want a read-only rejection", err)
	}

	// --- Isolation: each item is its own database. ---
	if _, err := lkdb.QueryContext(ctx, "SELECT * FROM dbo.metrics"); err == nil {
		t.Fatal("the warehouse's table is visible from the lakehouse connection — items not isolated")
	}
	if _, err := whdb.QueryContext(ctx, "SELECT * FROM [sales]"); err == nil {
		t.Fatal("the lakehouse's table is visible from the warehouse connection — items not isolated")
	}

	// --- information_schema relays natively (SSMS/Power BI schema introspection). ---
	var tname string
	if err := whdb.QueryRowContext(ctx,
		"SELECT TABLE_NAME FROM INFORMATION_SCHEMA.TABLES WHERE TABLE_NAME='metrics'").Scan(&tname); err != nil || tname != "metrics" {
		t.Fatalf("information_schema: TABLE_NAME=%q err=%v", tname, err)
	}

	// --- Connect by display name (real Fabric addressing): the server name carries
	// the workspace, the database is the item's display name. A fixedDialer sends
	// the Fabric server name in LOGIN7 while dialing the test listener. Resolving
	// "wh" against the workspace must reach the *same* backend database as the GUID
	// connection above — the metrics table it created is visible here. ---
	fabricSrv := ws.DisplayName + ".datawarehouse.fabric.microsoft.com"
	openByName := func(server, database string) *sql.DB {
		d := fmt.Sprintf("server=%s;database=%s;encrypt=disable;dial timeout=5", server, database)
		c, err := mssql.NewConnectorWithAccessTokenProvider(d, func(context.Context) (string, error) { return token, nil })
		if err != nil {
			t.Fatal(err)
		}
		c.Dialer = fixedDialer{addr: fmt.Sprintf("127.0.0.1:%d", port)}
		return sql.OpenDB(c)
	}
	whByName := openByName(fabricSrv, wh.DisplayName)
	defer whByName.Close()
	var byNameCount int
	if err := whByName.QueryRowContext(ctx, "SELECT COUNT(*) FROM dbo.metrics").Scan(&byNameCount); err != nil {
		t.Fatalf("warehouse by name SELECT: %v", err)
	}
	if byNameCount != 2 {
		t.Fatalf("warehouse by name row count = %d, want 2 (same backend db as the GUID connection)", byNameCount)
	}
	// The lakehouse endpoint by name is still read-only.
	lkByName := openByName(fabricSrv, lake.DisplayName)
	defer lkByName.Close()
	if _, err := lkByName.ExecContext(ctx, "INSERT INTO [sales] VALUES ('zz', 9)"); err == nil {
		t.Fatal("lakehouse-by-name INSERT succeeded — must be read-only")
	} else if !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("lakehouse-by-name write error = %q; want a read-only rejection", err)
	}

	// --- RBAC: an item whose workspace the principal has no role on is rejected. ---
	ws2 := &store.Workspace{DisplayName: "other-ws"}
	if err := srv.Store.CreateWorkspace(ws2, store.Principal{ID: "someone-else", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	lake2 := &store.Item{WorkspaceID: ws2.ID, Type: "Lakehouse", DisplayName: "lake2"}
	if err := srv.Store.CreateItem(lake2, nil); err != nil {
		t.Fatal(err)
	}
	odb := open(lake2.ID)
	defer odb.Close()
	if _, err := odb.QueryContext(ctx, "SELECT 1"); err == nil {
		t.Fatal("connected to a lakehouse the principal has no workspace role on — RBAC not enforced")
	}
}

// fixedDialer sends the DSN's server name in LOGIN7 (so the emulator can read the
// workspace from it) while dialing a fixed address — the test listener. It
// implements mssql.HostDialer so go-mssqldb skips DNS resolution of the Fabric
// hostname and hands the raw host to DialContext, which ignores it.
type fixedDialer struct{ addr string }

func (d fixedDialer) DialContext(ctx context.Context, _ string, _ string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "tcp", d.addr)
}

func (d fixedDialer) HostName() string { return d.addr }
