package server_test

// Class B strict mode, witnessed by a real client over real TDS.
//
// The claim was own-tests-only for a reason that reads convincing and is
// wrong the same way `FABRIC_FORCE_LRO` was: the TOGGLE is emulator-side, so
// no tenant has it. But what the toggle produces is a REFUSAL delivered over
// the wire, and a real SQL client either receives it or does not.
// internal/tds's own tests call dialectFix() directly — our code on both ends,
// and no evidence that a client ever sees the refusal.
//
// Gated on WAREHOUSE_MSSQL_DSN like its sibling in tds_sqlserver_test.go, so it
// runs in the warehouse-tds CI job and skips offline.

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

// strictTDS stands up an emulator whose TDS endpoint relays to the real SQL
// Server, with strict mode on or off, and returns a connected go-mssqldb handle.
func strictTDS(t *testing.T, backendDSN string, strict bool) *sql.DB {
	t.Helper()
	emu := entra.StartT(t)
	cfg := &config.Config{
		EntraIssuer:     emu.Origin + "/" + emu.TenantID + "/v2.0",
		SQLTDSAddr:      "127.0.0.1:0",
		WarehouseSQLURL: backendDSN,
		TSQLStrict:      strict,
	}
	if err := cfg.Finish(); err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(cfg, emu.HTTPClient())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })

	ws := &store.Workspace{DisplayName: fmt.Sprintf("strict-ws-%t", strict)}
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
	t.Cleanup(func() { ln.Close() })
	go srv.TDS.Serve(ln)

	// The client names its database, which is what takes the byte-splice path
	// production uses — connecting with none would skip the RBAC wall and prove
	// less, as the sibling test records.
	addr := ln.Addr().(*net.TCPAddr)
	dsn := fmt.Sprintf("server=127.0.0.1;port=%d;database=%s;encrypt=disable;dial timeout=5",
		addr.Port, wh.ID)
	token := forgeAppToken(t, emu, "https://database.windows.net")
	c, err := mssql.NewAccessTokenConnector(dsn, func() (string, error) { return token, nil })
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(c)
	t.Cleanup(func() { db.Close() })
	return db
}

func TestWarehouseStrictModeRefusesClassBOverRealTDS(t *testing.T) {
	backendDSN := os.Getenv("WAREHOUSE_MSSQL_DSN")
	if backendDSN == "" {
		t.Skip("set WAREHOUSE_MSSQL_DSN (a reachable SQL Server) to run the strict-mode e2e")
	}
	// Same limitation as the relay e2e: the splice cannot handshake over a
	// named pipe, so a LocalDB backend cannot reach the surface under test.
	if strings.Contains(strings.ToLower(backendDSN), "np:") ||
		strings.Contains(backendDSN, `\pipe\`) {
		t.Skip("named-pipe backend: the TDS splice cannot handshake over one")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// A recursive CTE: legal T-SQL that SQL Server runs and Fabric refuses,
	// which is exactly what Class B means. A pure SELECT, so running it on the
	// real engine leaves nothing behind.
	const recursive = "with r as (select 1 n union all select n+1 from r where n < 5) select count(*) from r"

	// THE NEGATIVE HALF, and it has to come first: without the flag the same
	// statement must reach the engine and succeed. Without this, a refusal
	// under strict mode would be indistinguishable from SQL Server rejecting
	// the statement itself, and the test would prove nothing about the toggle.
	off := strictTDS(t, backendDSN, false)
	var n int
	var lastErr error
	for i := 0; i < 60; i++ { // SQL Server may still be starting
		if lastErr = off.QueryRowContext(ctx, recursive).Scan(&n); lastErr == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if lastErr != nil {
		t.Fatalf("strict OFF: the engine refused a recursive CTE, so this test cannot "+
			"attribute a strict-mode refusal to strict mode: %v", lastErr)
	}
	if n != 5 {
		t.Fatalf("strict OFF: recursive CTE returned %d rows; want 5", n)
	}

	on := strictTDS(t, backendDSN, true)

	// Class A still works with the flag on: strict mode removes capability, and
	// a version that refused everything would pass the refusal assertions below.
	var one int
	if err := on.QueryRowContext(ctx, "select 1").Scan(&one); err != nil {
		t.Fatalf("strict ON: an ordinary SELECT was refused: %v", err)
	}
	if one != 1 {
		t.Fatalf("strict ON: select 1 returned %d", one)
	}

	// Class B, refused before relay — so the client sees the emulator's own
	// error rather than the engine's, and each names the feature it refused.
	for _, tc := range []struct{ name, sql, feature string }{
		{"recursive CTE", recursive, "recursive-cte"},
		{"identity with a seed", "create table dbo.strict_seed (id int identity(10,2))", "identity-seed"},
		{"enforced constraint", "create table dbo.strict_pk (id int not null, constraint pk_strict primary key (id))", "enforced-constraint"},
		{"isolation level", "set transaction isolation level serializable", "set-isolation-level"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := on.ExecContext(ctx, tc.sql)
			if err == nil {
				t.Fatalf("accepted under strict mode; real Fabric refuses it")
			}
			if !strings.Contains(err.Error(), tc.feature) {
				t.Fatalf("refused, but the error does not name the feature %q: %v", tc.feature, err)
			}
			if !strings.Contains(strings.ToLower(err.Error()), "strict mode") {
				t.Fatalf("refused without saying it was strict mode, so a reader cannot "+
					"tell it from an engine error: %v", err)
			}
		})
	}
}
