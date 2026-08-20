package server_test

// Contract 4 on warehouse: write through the emulator TDS path, confirm on a
// brand-new connection. The writer must not be the one that SELECTs — that
// is the false-green shape every other write-landing miss shared.
//
// Gated on WAREHOUSE_MSSQL_DSN, same skip as TestWarehouseSQLServerRelayE2E.

import (
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
)

func TestConformanceWriteLanding(t *testing.T) {
	backendDSN := os.Getenv("WAREHOUSE_MSSQL_DSN")
	if backendDSN == "" {
		t.Skip("set WAREHOUSE_MSSQL_DSN (a reachable SQL Server) to run the relay e2e")
	}
	if strings.Contains(strings.ToLower(backendDSN), "np:") ||
		strings.Contains(backendDSN, `\pipe\`) {
		t.Skip("named-pipe backend: the TDS splice cannot handshake over one, so " +
			"the warehouse surface does not work here — see the splice/named-pipe note")
	}

	emu := entra.StartT(t)
	cfg := &config.Config{
		EntraIssuer:     emu.Origin + "/" + emu.TenantID + "/v2.0",
		SQLTDSAddr:      "127.0.0.1:0",
		WarehouseSQLURL: backendDSN,
	}
	if err := cfg.Finish(); err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(cfg, emu.HTTPClient())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	if srv.TDS == nil || srv.TDS.Backend == nil {
		t.Fatal("expected a TDS server with a SQL Server backend")
	}
	ws := &store.Workspace{DisplayName: "conformance-wh-ws"}
	if err := srv.Store.CreateWorkspace(ws, store.Principal{
		ID: entra.DaemonClientID, Type: "ServicePrincipal"}); err != nil {
		t.Fatal(err)
	}
	wh := &store.Item{WorkspaceID: ws.ID, Type: "Warehouse", DisplayName: "wh"}
	if err := srv.Store.CreateItem(wh, nil); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() { _ = srv.TDS.Serve(ln) }()

	addr := ln.Addr().(*net.TCPAddr)
	dsn := fmt.Sprintf("server=127.0.0.1;port=%d;database=%s;encrypt=disable;dial timeout=5",
		addr.Port, wh.ID)
	token := forgeAppToken(t, emu, "https://database.windows.net")
	newDB := func() *sql.DB {
		c, err := mssql.NewAccessTokenConnector(dsn, func() (string, error) { return token, nil })
		if err != nil {
			t.Fatal(err)
		}
		db := sql.OpenDB(c)
		db.SetMaxOpenConns(1)
		return db
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	exec := func(db *sql.DB, q string) error {
		var lastErr error
		for i := 0; i < 60; i++ {
			if _, err := db.ExecContext(ctx, q); err == nil {
				return nil
			} else {
				lastErr = err
			}
			time.Sleep(time.Second)
		}
		return lastErr
	}

	// Writer connection: CREATE + INSERT through the emulator, then close it.
	// Reusing this handle for the SELECT would make the read a writer-confirmed
	// write — the exact false green contract 4 exists to catch.
	writer := newDB()
	_ = exec(writer, "IF OBJECT_ID('dbo.conformance_events') IS NOT NULL DROP TABLE dbo.conformance_events")
	if err := exec(writer, "CREATE TABLE dbo.conformance_events (id INT, name NVARCHAR(8))"); err != nil {
		writer.Close()
		t.Fatalf("writer create table via relay: %v", err)
	}
	if err := exec(writer, "INSERT INTO dbo.conformance_events VALUES (1, 'a'), (2, 'b')"); err != nil {
		writer.Close()
		t.Fatalf("writer insert via relay: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	// Reader connection: a second connect, not a second query on the first.
	reader := newDB()
	defer reader.Close()
	rows, err := reader.QueryContext(ctx, "SELECT id, name FROM dbo.conformance_events ORDER BY id")
	if err != nil {
		t.Fatalf("reader select via relay: %v", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var id int
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("writer reported success; out-of-band reader found nothing at dbo.conformance_events")
	}
	_, _ = reader.ExecContext(ctx, "DROP TABLE dbo.conformance_events")
}
