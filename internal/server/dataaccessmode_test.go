package server_test

// Switching a SQL analytics endpoint's data access mode against a real SQL
// Server (docs/60): the workspace's sessions end, and the documented SQL objects
// go and come back.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	entra "github.com/calvinchengx/entra-emulator/emulator"
	mssql "github.com/microsoft/go-mssqldb"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// lakehouse adds a lakehouse and its SQL analytics endpoint to the fixture.
func (f *secFixture) lakehouse(t *testing.T) (lake, endpoint *store.Item) {
	t.Helper()
	lake = &store.Item{WorkspaceID: f.ws.ID, Type: "Lakehouse", DisplayName: "lake"}
	endpoint = &store.Item{WorkspaceID: f.ws.ID, Type: "SQLEndpoint", DisplayName: "lake"}
	for _, it := range []*store.Item{lake, endpoint} {
		if err := f.srv.Store.CreateItem(it, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.srv.Store.SetItemProperties(endpoint.ID, map[string]string{store.PropParentLakehouse: lake.ID}); err != nil {
		t.Fatal(err)
	}
	return lake, endpoint
}

// open connects oid to a database, reporting the connect error rather than
// failing, so a refusal can be asserted.
func (f *secFixture) open(t *testing.T, oid, database string) (*sql.DB, error) {
	t.Helper()
	token := forgeTokenAs(t, f.emu, "https://database.windows.net", oid)
	c, err := mssql.NewAccessTokenConnector(fmt.Sprintf("server=127.0.0.1;port=%d;database=%s;encrypt=disable;dial timeout=5",
		f.addr.Port, database), func() (string, error) { return token, nil })
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(c)
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	return db, db.PingContext(context.Background())
}

func (f *secFixture) switchMode(t *testing.T, web *httptest.Server, endpoint *store.Item, oid, mode string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"dataAccessMode": mode})
	req, err := http.NewRequest("PUT", web.URL+"/v1/workspaces/"+f.ws.ID+"/sqlEndpoints/"+endpoint.ID+"/_emulator/dataAccessMode", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+forgeTokenAs(t, f.emu, "https://api.fabric.microsoft.com", oid))
	resp, err := web.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func scalar(t *testing.T, db *sql.DB, q string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), q).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func TestSwitchingTheDataAccessModeAppliesItsEffects(t *testing.T) {
	f := newSecFixture(t)
	web := httptest.NewServer(f.srv.Handler())
	t.Cleanup(web.Close)
	lake, endpoint := f.lakehouse(t)
	alice := "aaaa1111-0000-0000-0000-0000000dam01"
	if err := f.srv.Store.CreateRoleAssignment(&store.RoleAssignment{WorkspaceID: f.ws.ID,
		Principal: store.Principal{ID: alice, Type: "User"}, Role: store.RoleContributor}); err != nil {
		t.Fatal(err)
	}

	// The endpoint's database as the service sees it, with what a delegated
	// endpoint accumulates: a custom role, a security policy and its predicate,
	// and a function nothing binds.
	svc, err := f.srv.API.LakehouseDB(context.Background(), lake.ID)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, svc,
		`CREATE TABLE dbo.dam_sales (owner_name sysname, v int)`,
		`INSERT INTO dbo.dam_sales VALUES (N'x', 1)`,
		`CREATE SCHEMA dam`,
		`CREATE FUNCTION dam.fn_owner(@owner sysname) RETURNS TABLE WITH SCHEMABINDING AS RETURN SELECT 1 AS ok WHERE @owner = USER_NAME()`,
		`CREATE FUNCTION dam.fn_unbound() RETURNS int AS BEGIN RETURN 1 END`,
		`CREATE SECURITY POLICY dam.p ADD FILTER PREDICATE dam.fn_owner(owner_name) ON dbo.dam_sales WITH (STATE = ON)`,
		`CREATE USER dam_member WITHOUT LOGIN`,
		`CREATE ROLE analysts`,
		`ALTER ROLE analysts ADD MEMBER dam_member`)

	// Live sessions: one to the lakehouse's endpoint, one to the warehouse beside
	// it — Fabric takes every SQL endpoint in the workspace offline.
	onLake, err := f.open(t, alice, lake.ID)
	if err != nil {
		t.Fatalf("alice to the lakehouse: %v", err)
	}
	onWarehouse, err := f.open(t, alice, f.wh.ID)
	if err != nil {
		t.Fatalf("alice to the warehouse: %v", err)
	}

	// A Contributor cannot switch; an Admin can.
	if code, body := f.switchMode(t, web, endpoint, alice, "UserIdentity"); code != http.StatusForbidden {
		t.Fatalf("contributor switch = %d %s", code, body)
	}
	if code, body := f.switchMode(t, web, endpoint, entra.DaemonClientID, "UserIdentity"); code != http.StatusOK {
		t.Fatalf("admin switch = %d %s", code, body)
	}

	for name, db := range map[string]*sql.DB{"lakehouse": onLake, "warehouse": onWarehouse} {
		if _, err := db.ExecContext(context.Background(), "SELECT 1"); err == nil {
			t.Errorf("the %s session survived the switch", name)
		}
	}
	if n := scalar(t, svc, `SELECT COUNT(*) FROM sys.database_principals WHERE name = 'analysts'`); n != 0 {
		t.Error("switching to user identity kept a custom SQL role")
	}
	if n := scalar(t, svc, `SELECT COUNT(*) FROM sys.objects WHERE name = 'fn_unbound'`); n != 0 {
		t.Error("an unbound function survived the switch")
	}
	if n := scalar(t, svc, `SELECT COUNT(*) FROM sys.objects WHERE name = 'fn_owner'`); n != 1 {
		t.Error("the security policy's predicate was dropped")
	}
	if n := scalar(t, svc, `SELECT CAST(is_enabled AS int) FROM sys.security_policies WHERE name = 'p'`); n != 0 {
		t.Error("the SQL security policy is still enforced in user identity mode")
	}

	// In user identity mode the endpoint serves again — table access from
	// OneLake security, which internal/server/onelakesync_test.go witnesses.
	if _, err := f.open(t, alice, lake.ID); err != nil {
		t.Errorf("connecting in user identity mode: %v", err)
	}

	// Back to delegated: the policy it turned off is enforced again, and the
	// endpoint serves.
	if code, body := f.switchMode(t, web, endpoint, entra.DaemonClientID, "DelegatedIdentity"); code != http.StatusOK {
		t.Fatalf("switch back = %d %s", code, body)
	}
	if n := scalar(t, svc, `SELECT CAST(is_enabled AS int) FROM sys.security_policies WHERE name = 'p'`); n != 1 {
		t.Error("switching back to delegated did not re-enable the policy")
	}
	if _, err := f.open(t, alice, lake.ID); err != nil {
		t.Errorf("connecting after switching back: %v", err)
	}

	// An endpoint with no SQL security policy switches both ways too, and has
	// nothing to turn back on.
	mustExec(t, svc, `DROP SECURITY POLICY dam.p`)
	for _, mode := range []string{"UserIdentity", "DelegatedIdentity"} {
		if code, body := f.switchMode(t, web, endpoint, entra.DaemonClientID, mode); code != http.StatusOK {
			t.Fatalf("switch to %s with no policy = %d %s", mode, code, body)
		}
	}
	if props, _ := f.srv.Store.ItemProperties(endpoint.ID); props["delegatedSecurityPoliciesDisabled"] != "" {
		t.Errorf("a remembered policy outlived the switch back: %q", props["delegatedSecurityPoliciesDisabled"])
	}
}
