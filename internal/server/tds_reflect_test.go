package server_test

// Warehouse T3 e2e: the cross-engine oracle. delta-rs/Spark-style Delta lands in
// OneLake; a SQL client connects to the warehouse endpoint with the lakehouse as
// its database; on login the emulator reads the Delta table (pure Go) and
// reflects it into the real SQL Server, and a GROUP BY over it returns the same
// answer DuckDB gives in R3. Gated on WAREHOUSE_MSSQL_DSN (CI Linux + a SQL
// Server service).

import (
	"bytes"
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
	mssql "github.com/microsoft/go-mssqldb"
	"github.com/parquet-go/parquet-go"
)

type whRow struct {
	Region string `parquet:"region"`
	Amount int64  `parquet:"amount"`
}

func TestWarehouseDeltaReflectionE2E(t *testing.T) {
	dsn := os.Getenv("WAREHOUSE_MSSQL_DSN")
	if dsn == "" {
		t.Skip("set WAREHOUSE_MSSQL_DSN (a reachable SQL Server) to run the reflection e2e")
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

	// Seed a lakehouse and a Delta table (part-0.parquet + _delta_log) in OneLake.
	ws := &store.Workspace{DisplayName: "wh-ws"}
	if err := srv.Store.CreateWorkspace(ws, store.Principal{ID: entra.DaemonClientID, Type: "ServicePrincipal"}); err != nil {
		t.Fatal(err)
	}
	lake := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lake"}
	if err := srv.Store.CreateItem(lake, nil); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	pw := parquet.NewGenericWriter[whRow](&buf)
	if _, err := pw.Write([]whRow{{"us", 80}, {"eu", 60}, {"us", 10}}); err != nil {
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
	addr := ln.Addr().(*net.TCPAddr)

	// Connect with database=<lakehouse id> → reflection runs on login.
	token := forgeAppToken(t, emu, "https://database.windows.net")
	connDSN := fmt.Sprintf("server=127.0.0.1;port=%d;database=%s;encrypt=disable;dial timeout=5", addr.Port, lake.ID)
	c, err := mssql.NewAccessTokenConnector(connDSN, func() (string, error) { return token, nil })
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(c)
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// A GROUP BY over the reflected Delta table (retry for SQL Server startup).
	got := map[string]int64{}
	var lastErr error
	for i := 0; i < 60; i++ {
		rows, err := db.QueryContext(ctx, "SELECT region, SUM(amount) t FROM [sales] GROUP BY region")
		if err != nil {
			lastErr = err
			time.Sleep(time.Second)
			continue
		}
		got = map[string]int64{}
		for rows.Next() {
			var r string
			var s int64
			if err := rows.Scan(&r, &s); err != nil {
				rows.Close()
				t.Fatalf("scan: %v", err)
			}
			got[r] = s
		}
		rows.Close()
		lastErr = rows.Err()
		if lastErr == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if lastErr != nil {
		t.Fatalf("query reflected table: %v", lastErr)
	}
	if got["us"] != 90 || got["eu"] != 60 {
		t.Fatalf("reflected Delta GROUP BY = %v; want us=90 eu=60 (matches DuckDB in R3)", got)
	}
}
