package server_test

// OneLake security synced into a lakehouse's SQL analytics endpoint in user
// identity access mode, against a real SQL Server over the relay (docs/60).
// Viewers in different OneLake roles, one in none, and a Contributor, reading
// the same tables in the same run — and the same Viewers under delegated
// identity, where the roles do not apply.

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	entra "github.com/calvinchengx/entra-emulator/emulator"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

func (f *secFixture) grantRole(t *testing.T, oid, role string) {
	t.Helper()
	if err := f.srv.Store.CreateRoleAssignment(&store.RoleAssignment{WorkspaceID: f.ws.ID,
		Principal: store.Principal{ID: oid, Type: "User"}, Role: role}); err != nil {
		t.Fatal(err)
	}
}

// oneLakeRole is a role granting one table, optionally narrowed to columns.
func oneLakeRole(name, table, member string, columns ...string) store.OneLakeRole {
	constraints := ""
	if len(columns) > 0 {
		constraints = fmt.Sprintf(`,"constraints":{"columns":[{"tablePath":"/Tables/%s","columnNames":["%s"],"columnEffect":"Permit","columnAction":["Read"]}]}`,
			table, strings.Join(columns, `","`))
	}
	return store.OneLakeRole{Name: name, Body: []byte(fmt.Sprintf(`{"name":%q,"decisionRules":[{"effect":"Permit","permission":[
	  {"attributeName":"Path","attributeValueIncludedIn":["Tables/%s"]},
	  {"attributeName":"Action","attributeValueIncludedIn":["Read"]}]%s}],
	  "members":{"microsoftEntraMembers":[{"objectId":%q}]}}`, name, table, constraints, member))}
}

func canRead(db *sql.DB, q string) error {
	rows, err := db.QueryContext(context.Background(), q)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
	}
	return rows.Err()
}

func TestUserIdentityModeAppliesOneLakeSecurityOnTheEndpoint(t *testing.T) {
	f := newSecFixture(t)
	web := httptest.NewServer(f.srv.Handler())
	t.Cleanup(web.Close)
	lake, endpoint := f.lakehouse(t)
	alice := "aaaa1111-0000-0000-0000-00000000015a" // Viewer, may read sales' region and amount
	bob := "bbbb2222-0000-0000-0000-00000000015b"   // Viewer, may read hr
	carol := "cccc3333-0000-0000-0000-00000000015c" // Viewer, in no role
	dave := "dddd4444-0000-0000-0000-00000000015d"  // Contributor, in no role
	for oid, role := range map[string]string{alice: store.RoleViewer, bob: store.RoleViewer, carol: store.RoleViewer, dave: store.RoleContributor} {
		f.grantRole(t, oid, role)
	}
	svc, err := f.srv.API.LakehouseDB(context.Background(), lake.ID)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, svc,
		`CREATE TABLE dbo.sales (region varchar(10), amount int, secret varchar(10))`,
		`INSERT INTO dbo.sales VALUES ('west', 1, 's')`,
		`CREATE TABLE dbo.hr (name varchar(10))`,
		`INSERT INTO dbo.hr VALUES ('ada')`)
	// Beside the two roles under test: one filtering hr's rows for carol, which
	// is not synced until row filters are (stage 3), and one naming a column
	// sales does not have for bob, which grants nothing on sales — neither may
	// read as no restriction at all.
	filtered := store.OneLakeRole{Name: "FilteredHR", Body: []byte(fmt.Sprintf(`{"name":"FilteredHR","decisionRules":[{"effect":"Permit","permission":[
	  {"attributeName":"Path","attributeValueIncludedIn":["Tables/hr"]},
	  {"attributeName":"Action","attributeValueIncludedIn":["Read"]}],
	  "constraints":{"rows":[{"tablePath":"/Tables/hr","value":"SELECT * FROM hr WHERE name = 'ada'"}]}}],
	  "members":{"microsoftEntraMembers":[{"objectId":%q}]}}`, carol))}
	roles := []store.OneLakeRole{oneLakeRole("SalesReaders", "sales", alice, "region", "amount"), oneLakeRole("HR", "hr", bob),
		filtered, oneLakeRole("Renamed", "sales", bob, "region", "no_such_column")}
	for i := range roles {
		roles[i].ItemID = lake.ID
	}
	if err := f.srv.Store.PutOneLakeRoles(lake.ID, roles); err != nil {
		t.Fatal(err)
	}

	queries := []string{"SELECT region, amount FROM dbo.sales", "SELECT secret FROM dbo.sales", "SELECT name FROM dbo.hr"}
	expect := func(phase string, want map[string][]bool) {
		t.Helper()
		for oid, allowed := range want {
			db, err := f.open(t, oid, lake.ID)
			if err != nil {
				t.Fatalf("%s: %s connect: %v", phase, oid[:8], err)
			}
			for i, q := range queries {
				if err := canRead(db, q); (err == nil) != allowed[i] {
					t.Errorf("%s: %s %q: err=%v, want allowed=%v", phase, oid[:8], q, err, allowed[i])
				}
			}
		}
	}

	// The endpoint is where its SQL objects and security are authored, and still
	// read-only for data — however the write is dressed.
	author, err := f.open(t, entra.DaemonClientID, lake.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.open(t, carol, lake.ID) // provisions carol, so the GRANT has a grantee
	mustExec(t, author, `CREATE VIEW dbo.sales_regions AS SELECT region FROM dbo.sales`,
		`GRANT SELECT ON dbo.sales_regions TO [`+carol+`]`)
	for _, write := range []string{
		`INSERT INTO dbo.hr VALUES ('eve')`,
		`/* note */ INSERT INTO dbo.hr VALUES ('eve')`,
		`GRANT SELECT ON dbo.sales_regions TO [` + carol + `] INSERT INTO dbo.hr VALUES ('eve')`,
		`CREATE TABLE dbo.extra (x int)`,
	} {
		if _, err := author.ExecContext(context.Background(), write); err == nil || !strings.Contains(err.Error(), "read-only") {
			t.Errorf("a write on the endpoint (%q): %v", write, err)
		}
	}
	if n := scalar(t, svc, `SELECT COUNT(*) FROM dbo.hr`); n != 1 {
		t.Fatalf("the endpoint's table was written: %d rows", n)
	}

	// Delegated identity: SQL permissions only, and a Viewer reads every table.
	expect("delegated", map[string][]bool{alice: {true, true, true}, carol: {true, true, true}})

	// A DENY authored under delegated identity, to be set aside and restored.
	_, _ = f.open(t, dave, lake.ID) // provisions dave, so the DENY has a grantee
	mustExec(t, svc, `DENY SELECT ON dbo.hr TO [`+dave+`]`)

	if code, body := f.switchMode(t, web, endpoint, entra.DaemonClientID, "UserIdentity"); code != http.StatusOK {
		t.Fatalf("switch = %d %s", code, body)
	}
	// User identity: each Viewer reads exactly what their OneLake role grants,
	// a Viewer in no role reads nothing, and a Contributor — whose table-level
	// DENY is ignored — reads everything.
	expect("user identity", map[string][]bool{
		alice: {true, false, false},
		bob:   {false, false, true},
		carol: {false, false, false},
		dave:  {true, true, true},
	})

	// The same holds by three-part name from the warehouse beside it: the
	// memberships travel with the caller into every database they reach.
	fromWarehouse, err := f.open(t, alice, f.wh.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := canRead(fromWarehouse, "SELECT region FROM ["+lake.ID+"].dbo.sales"); err != nil {
		t.Errorf("alice by three-part name: %v", err)
	}
	if err := canRead(fromWarehouse, "SELECT name FROM ["+lake.ID+"].dbo.hr"); err == nil {
		t.Error("alice read hr by three-part name")
	}

	// Table security cannot be authored in T-SQL; a view's can.
	owner, err := f.open(t, entra.DaemonClientID, lake.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.ExecContext(context.Background(), `GRANT SELECT ON dbo.hr TO [`+carol+`]`); err == nil ||
		!strings.Contains(err.Error(), "governed by OneLake security") {
		t.Errorf("a table GRANT in user identity mode: %v", err)
	}
	mustExec(t, owner, `CREATE VIEW dbo.hr_names AS SELECT name FROM dbo.hr`, `GRANT SELECT ON dbo.hr_names TO [`+carol+`]`)

	// Membership moves with the OneLake role on the next connection: the role
	// stays, alice leaves it, carol joins it.
	moved := []store.OneLakeRole{oneLakeRole("SalesReaders", "sales", carol, "region", "amount"), roles[1]}
	moved[0].ItemID = lake.ID
	if err := f.srv.Store.PutOneLakeRoles(lake.ID, moved); err != nil {
		t.Fatal(err)
	}
	expect("after the role moves to carol", map[string][]bool{alice: {false, false, false}, carol: {true, false, false}})

	// A role that is removed is gone from the endpoint too.
	if err := f.srv.Store.PutOneLakeRoles(lake.ID, roles[1:2]); err != nil {
		t.Fatal(err)
	}
	expect("after the role is removed", map[string][]bool{carol: {false, false, false}, bob: {false, false, true}})

	// Back to delegated: the synced roles are gone, a Viewer reads every table
	// again, and the DENY set aside is enforced again.
	if code, body := f.switchMode(t, web, endpoint, entra.DaemonClientID, "DelegatedIdentity"); code != http.StatusOK {
		t.Fatalf("switch back = %d %s", code, body)
	}
	if n := scalar(t, svc, `SELECT COUNT(*) FROM sys.database_principals WHERE name LIKE 'OLS[_]%'`); n != 0 {
		t.Errorf("%d synced role(s) survived the switch back", n)
	}
	expect("delegated again", map[string][]bool{alice: {true, true, true}, dave: {true, true, false}})
}

// A sync that cannot be applied fails the connection rather than serving the
// endpoint under whatever state it last had.
func TestAOneLakeSyncThatFailsRefusesTheConnection(t *testing.T) {
	f := newSecFixture(t)
	web := httptest.NewServer(f.srv.Handler())
	t.Cleanup(web.Close)
	lake, endpoint := f.lakehouse(t)
	svc, err := f.srv.API.LakehouseDB(context.Background(), lake.ID)
	if err != nil {
		t.Fatal(err)
	}
	if code, body := f.switchMode(t, web, endpoint, entra.DaemonClientID, "UserIdentity"); code != http.StatusOK {
		t.Fatalf("switch = %d %s", code, body)
	}
	long := oneLakeRole(strings.Repeat("r", 125), "sales", entra.DaemonClientID)
	long.ItemID = lake.ID
	if err := f.srv.Store.PutOneLakeRoles(lake.ID, []store.OneLakeRole{long}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.open(t, entra.DaemonClientID, lake.ID); err == nil || !strings.Contains(err.Error(), "cannot be synced") {
		t.Errorf("a role name no SQL role can carry: %v", err)
	}
	// A hand-made OLS_ role owning a schema cannot be dropped by the sync —
	// "manual changes to these roles are not supported".
	if err := f.srv.Store.PutOneLakeRoles(lake.ID, nil); err != nil {
		t.Fatal(err)
	}
	mustExec(t, svc, `CREATE ROLE OLS_handmade`, `CREATE SCHEMA handmade AUTHORIZATION OLS_handmade`)
	if _, err := f.open(t, entra.DaemonClientID, lake.ID); err == nil || !strings.Contains(err.Error(), "syncing OneLake security") {
		t.Errorf("a sync the engine refuses: %v", err)
	}
}
