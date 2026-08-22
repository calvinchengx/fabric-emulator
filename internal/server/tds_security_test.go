package server_test

// T-SQL security through the relay: RLS, CLS and dynamic data masking, driven
// by a real go-mssqldb client over the emulator's TDS endpoint
// (docs/55-tsql-security.md, stages 3-5).
//
// WHAT MAKES THIS A WITNESS. Two callers, one query, different answers. A
// single restricted caller proves nothing: a policy that denied everyone, or a
// relay that had simply broken, would pass it identically. Every assertion below
// is paired with the other caller getting the other answer, in the same run.
//
// It is also the first test that exercises the whole chain rather than a piece
// of it — Entra token, workspace RBAC, per-caller provisioning, the byte-splice,
// and SQL Server's own enforcement. The security features are the engine's; what
// is being witnessed is that a caller arrives as themselves.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"

	entra "github.com/calvinchengx/entra-emulator/emulator"
	mssql "github.com/microsoft/go-mssqldb"

	"github.com/calvinchengx/fabric-emulator/internal/config"
	"github.com/calvinchengx/fabric-emulator/internal/server"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// secFixture brings up the emulator with a SQL Server backend and a warehouse,
// and returns a dialer for connecting as an arbitrary principal.
type secFixture struct {
	srv  *server.Server
	emu  *entra.Emulator
	ws   *store.Workspace
	wh   *store.Item
	addr *net.TCPAddr
}

func newSecFixture(t *testing.T) *secFixture {
	t.Helper()
	dsn := os.Getenv("WAREHOUSE_MSSQL_DSN")
	if dsn == "" {
		t.Skip("set WAREHOUSE_MSSQL_DSN (a reachable SQL Server) to run the T-SQL security e2e")
	}
	if strings.Contains(strings.ToLower(dsn), "np:") || strings.Contains(dsn, `\pipe\`) {
		t.Skip("named-pipe backend: the TDS splice cannot handshake over one")
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
	t.Cleanup(func() { _ = srv.Close() })

	ws := &store.Workspace{DisplayName: "tsql-sec-ws"}
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
	t.Cleanup(func() { _ = ln.Close() })
	go func() { _ = srv.TDS.Serve(ln) }()

	return &secFixture{srv: srv, emu: emu, ws: ws, wh: wh, addr: ln.Addr().(*net.TCPAddr)}
}

// connectAs opens a client connection whose Fabric principal is `oid`, granting
// it `role` on the workspace first. The oid override is what makes two distinct
// callers possible from one registered app.
func (f *secFixture) connectAs(t *testing.T, oid, role string) *sql.DB {
	t.Helper()
	if oid != entra.DaemonClientID {
		if err := f.srv.Store.CreateRoleAssignment(&store.RoleAssignment{
			WorkspaceID: f.ws.ID, Principal: store.Principal{ID: oid, Type: "User"}, Role: role,
		}); err != nil {
			t.Fatal(err)
		}
	}
	token := forgeTokenAs(t, f.emu, "https://database.windows.net", oid)
	dsn := fmt.Sprintf("server=127.0.0.1;port=%d;database=%s;encrypt=disable;dial timeout=5",
		f.addr.Port, f.wh.ID)
	c, err := mssql.NewAccessTokenConnector(dsn, func() (string, error) { return token, nil })
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(c)
	t.Cleanup(func() { _ = db.Close() })
	// CONNECT EAGERLY. Provisioning happens on connect, and `sql.OpenDB` is
	// lazy — so without this the caller has no database principal yet and an
	// owner's `DENY … TO [caller]` fails with "Cannot find the user". That
	// ordering is a real property of the emulator, recorded as a boundary in
	// docs/55; here it is simply respected.
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatalf("connect as %s: %v", oid, err)
	}
	return db
}

func mustExec(t *testing.T, db *sql.DB, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if _, err := db.ExecContext(context.Background(), s); err != nil {
			t.Fatalf("exec %q: %v", strings.SplitN(s, "\n", 2)[0], err)
		}
	}
}

// Stage 3: RLS. A filter predicate keyed on the connected user gives two
// callers different rows from the same SELECT.
func TestRelayEnforcesRowLevelSecurity(t *testing.T) {
	f := newSecFixture(t)
	owner := f.connectAs(t, entra.DaemonClientID, store.RoleAdmin)
	alice := "aaaa1111-0000-0000-0000-00000000a1ce"
	bob := "bbbb2222-0000-0000-0000-00000000b0b0"
	asAlice := f.connectAs(t, alice, store.RoleContributor)
	asBob := f.connectAs(t, bob, store.RoleContributor)

	mustExec(t, owner,
		`CREATE TABLE dbo.rls_sales (owner_name sysname, amount int)`,
		`INSERT INTO dbo.rls_sales VALUES (N'`+alice+`', 10), (N'`+bob+`', 20)`,
		`CREATE SCHEMA rlssec`,
		`CREATE FUNCTION rlssec.fn_owner(@owner sysname) RETURNS TABLE WITH SCHEMABINDING
		   AS RETURN SELECT 1 AS ok WHERE @owner = USER_NAME()`,
		`CREATE SECURITY POLICY rlssec.p ADD FILTER PREDICATE rlssec.fn_owner(owner_name)
		   ON dbo.rls_sales WITH (STATE = ON)`)

	for who, db := range map[string]*sql.DB{alice: asAlice, bob: asBob} {
		var got string
		if err := db.QueryRowContext(context.Background(),
			`SELECT owner_name FROM dbo.rls_sales`).Scan(&got); err != nil {
			t.Fatalf("%s: %v", who[:8], err)
		}
		if got != who {
			t.Errorf("RLS: %s was shown the row of %s", who[:8], got[:8])
		}
	}
}

// Stage 4: CLS. A column denied to one caller is readable by the other, and the
// denial is an ERROR rather than a null — withholding the value, not blanking it.
func TestRelayEnforcesColumnLevelSecurity(t *testing.T) {
	f := newSecFixture(t)
	owner := f.connectAs(t, entra.DaemonClientID, store.RoleAdmin)
	alice := "aaaa1111-0000-0000-0000-00000000c15a"
	bob := "bbbb2222-0000-0000-0000-00000000c15b"
	asAlice := f.connectAs(t, alice, store.RoleContributor)
	asBob := f.connectAs(t, bob, store.RoleContributor)

	mustExec(t, owner,
		`CREATE TABLE dbo.cls_pay (name varchar(20), salary int)`,
		`INSERT INTO dbo.cls_pay VALUES ('ada', 100)`,
		`DENY SELECT ON dbo.cls_pay(salary) TO [`+alice+`]`)

	if _, err := asAlice.QueryContext(context.Background(), `SELECT salary FROM dbo.cls_pay`); err == nil {
		t.Error("CLS: the denied caller read the column")
	}
	var salary int
	if err := asBob.QueryRowContext(context.Background(),
		`SELECT salary FROM dbo.cls_pay`).Scan(&salary); err != nil {
		t.Errorf("CLS: the undenied caller was refused: %v", err)
	} else if salary != 100 {
		t.Errorf("CLS: undenied caller read %d, want 100", salary)
	}
}

// Stage 5: dynamic data masking. The one feature with no OneLake-security
// counterpart at all — "Not supported in OneLake security" — so a warehouse is
// the only place it can be witnessed.
func TestRelayEnforcesDynamicDataMasking(t *testing.T) {
	f := newSecFixture(t)
	owner := f.connectAs(t, entra.DaemonClientID, store.RoleAdmin)
	alice := "aaaa1111-0000-0000-0000-0000000ma5c1"
	bob := "bbbb2222-0000-0000-0000-0000000ma5c2"
	asAlice := f.connectAs(t, alice, store.RoleContributor)
	asBob := f.connectAs(t, bob, store.RoleContributor)

	mustExec(t, owner,
		`CREATE TABLE dbo.ddm_people (name varchar(20), email varchar(50))`,
		`ALTER TABLE dbo.ddm_people ALTER COLUMN email ADD MASKED WITH (FUNCTION = 'email()')`,
		`INSERT INTO dbo.ddm_people VALUES ('ada', 'ada@example.test')`,
		// UNMASK to bob only: he is the control, and the grant is what proves
		// the mask is a permission rather than a column rewrite.
		`GRANT UNMASK TO [`+bob+`]`)

	var masked, plain string
	if err := asAlice.QueryRowContext(context.Background(),
		`SELECT email FROM dbo.ddm_people`).Scan(&masked); err != nil {
		t.Fatalf("masked read: %v", err)
	}
	if err := asBob.QueryRowContext(context.Background(),
		`SELECT email FROM dbo.ddm_people`).Scan(&plain); err != nil {
		t.Fatalf("unmasked read: %v", err)
	}
	if masked == plain {
		t.Fatalf("DDM: both callers read %q — masking did not apply", masked)
	}
	if plain != "ada@example.test" {
		t.Errorf("the UNMASK holder read %q, want the real value", plain)
	}
	if strings.Contains(masked, "example.test") {
		t.Errorf("the masked read %q still contains the domain", masked)
	}
	t.Logf("DDM: %q vs %q", masked, plain)
}

// forgeTokenAs mints a token whose principal is `oid`.
//
// The forge API needs a REGISTERED clientId, so the app stays the seeded daemon
// and the `oid` claim is overridden instead — that is the claim the emulator
// resolves a principal from (internal/auth: "oid claim (falls back to sub)").
// It is how one registered app produces the several distinct callers these
// tests need.
func forgeTokenAs(t *testing.T, emu *entra.Emulator, audience, oid string) string {
	t.Helper()
	body := map[string]any{"clientId": entra.DaemonClientID, "audience": audience}
	if oid != entra.DaemonClientID {
		body["extraClaims"] = map[string]any{"oid": oid, "sub": oid}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := emu.HTTPClient().Post(emu.Origin+"/admin/api/tokens", "application/json",
		bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	var tok struct{ AccessToken, Token string }
	if err := json.Unmarshal(out, &tok); err != nil {
		t.Fatalf("forge: %v %s", err, out)
	}
	if tok.AccessToken != "" {
		return tok.AccessToken
	}
	return tok.Token
}
