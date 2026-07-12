package server_test

// Warehouse T1 e2e: a real SQL client (go-mssqldb) connects to the emulator's
// TDS endpoint using a real entra-minted token (Azure SQL audience), the
// FedAuth login is validated against entra's JWKS, and `SELECT 1` returns 1.
// A token for the wrong audience is rejected — the FedAuth-termination oracle.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	entra "github.com/calvinchengx/entra-emulator/emulator"
	"github.com/calvinchengx/fabric-emulator/internal/config"
	"github.com/calvinchengx/fabric-emulator/internal/server"
	mssql "github.com/microsoft/go-mssqldb"
)

func forgeAppToken(t *testing.T, emu *entra.Emulator, audience string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"clientId": entra.DaemonClientID, "audience": audience})
	resp, err := emu.HTTPClient().Post(emu.Origin+"/admin/api/tokens", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var tok struct{ AccessToken, Token string }
	if err := json.Unmarshal(raw, &tok); err != nil {
		t.Fatalf("forge: %v %s", err, raw)
	}
	if tok.AccessToken != "" {
		return tok.AccessToken
	}
	if tok.Token == "" {
		t.Fatalf("forge returned no token: %s", raw)
	}
	return tok.Token
}

func TestWarehouseTDSEndpointE2E(t *testing.T) {
	emu := entra.StartT(t)
	cfg := &config.Config{
		EntraIssuer: emu.Origin + "/" + emu.TenantID + "/v2.0",
		SQLTDSAddr:  "127.0.0.1:0", // non-empty enables srv.TDS; we make our own listener
	}
	if err := cfg.Finish(); err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(cfg, emu.HTTPClient())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	if srv.TDS == nil {
		t.Fatal("SQLTDSAddr set but srv.TDS is nil")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go srv.TDS.Serve(ln)

	addr := ln.Addr().(*net.TCPAddr)
	dsn := fmt.Sprintf("server=127.0.0.1;port=%d;encrypt=disable;dial timeout=5", addr.Port)
	query := func(token string) (int, error) {
		c, err := mssql.NewAccessTokenConnector(dsn, func() (string, error) { return token, nil })
		if err != nil {
			return 0, err
		}
		db := sql.OpenDB(c)
		defer db.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var n int
		err = db.QueryRowContext(ctx, "select 1").Scan(&n)
		return n, err
	}

	// A real Azure-SQL-audience token → login validated against entra → SELECT 1.
	got, err := query(forgeAppToken(t, emu, "https://database.windows.net"))
	if err != nil {
		t.Fatalf("query with SQL-audience token: %v", err)
	}
	if got != 1 {
		t.Fatalf("SELECT 1 = %d", got)
	}

	// A token for the Fabric audience is rejected on the SQL endpoint.
	if _, err := query(forgeAppToken(t, emu, "https://api.fabric.microsoft.com")); err == nil {
		t.Fatal("wrong-audience token should be rejected by the TDS endpoint")
	}
}
