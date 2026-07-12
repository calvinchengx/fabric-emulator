package server_test

// Warehouse T2 e2e: the real relay. A real SQL client connects to the emulator's
// TDS endpoint with a real entra token; the FedAuth-authenticated batch is
// relayed to a real SQL Server sidecar, which actually runs the T-SQL. Gated on
// WAREHOUSE_MSSQL_DSN (a reachable SQL Server), so it runs in CI (Linux, a
// SQL Server service) and is skipped in the normal offline suite.

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
	mssql "github.com/microsoft/go-mssqldb"
)

func TestWarehouseSQLServerRelayE2E(t *testing.T) {
	backendDSN := os.Getenv("WAREHOUSE_MSSQL_DSN")
	if backendDSN == "" {
		t.Skip("set WAREHOUSE_MSSQL_DSN (a reachable SQL Server) to run the relay e2e")
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
	t.Cleanup(func() { srv.Close() })
	if srv.TDS == nil || srv.TDS.Backend == nil {
		t.Fatal("expected a TDS server with a SQL Server backend")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go srv.TDS.Serve(ln)

	// Connect the real SQL client to the emulator with a real entra token.
	addr := ln.Addr().(*net.TCPAddr)
	dsn := fmt.Sprintf("server=127.0.0.1;port=%d;encrypt=disable;dial timeout=5", addr.Port)
	token := forgeAppToken(t, emu, "https://database.windows.net")
	c, err := mssql.NewAccessTokenConnector(dsn, func() (string, error) { return token, nil })
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(c)
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// SQL Server may still be starting; retry the first relayed statement.
	exec := func(q string) error {
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

	// DDL + DML + SELECT all relay through the emulator to the real engine
	// (param-free so they travel as SQLBatch, not RPC).
	_ = exec("IF OBJECT_ID('dbo.wh_e2e') IS NOT NULL DROP TABLE dbo.wh_e2e")
	if err := exec("CREATE TABLE dbo.wh_e2e (region NVARCHAR(8), amount INT)"); err != nil {
		t.Fatalf("create table via relay: %v", err)
	}
	if err := exec("INSERT INTO dbo.wh_e2e VALUES ('us', 80), ('eu', 60)"); err != nil {
		t.Fatalf("insert via relay: %v", err)
	}

	rows, err := db.QueryContext(ctx,
		"SELECT region, SUM(amount) total FROM dbo.wh_e2e GROUP BY region ORDER BY region")
	if err != nil {
		t.Fatalf("select via relay: %v", err)
	}
	defer rows.Close()
	got := map[string]int{}
	for rows.Next() {
		var region string
		var total int
		if err := rows.Scan(&region, &total); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[region] = total
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if got["us"] != 80 || got["eu"] != 60 {
		t.Fatalf("relayed T-SQL result = %v; want us=80 eu=60", got)
	}
	_, _ = db.ExecContext(ctx, "DROP TABLE dbo.wh_e2e")
}
