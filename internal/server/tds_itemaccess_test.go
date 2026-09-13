package server_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"

	entra "github.com/calvinchengx/entra-emulator/emulator"
	mssql "github.com/microsoft/go-mssqldb"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// Item permissions at the SQL endpoint, through the real relay, with a real TDS
// client, against a real SQL Server. Every admission is witnessed with the same
// connection refused before it and again after the revoke: a grant witnessed
// without its revoke proves storage, not permission.

const strangerOID = "5a5a5a5a-0000-0000-0000-00000000fe57"

// dial opens a connection as oid to one database WITHOUT granting any workspace
// role, and reports the connect error rather than failing on it — the refusal
// is often the assertion.
func (f *secFixture) dial(t *testing.T, oid, database string) (*sql.DB, error) {
	t.Helper()
	token := forgeTokenAs(t, f.emu, "https://database.windows.net", oid)
	dsn := fmt.Sprintf("server=127.0.0.1;port=%d;database=%s;encrypt=disable;dial timeout=5", f.addr.Port, database)
	c, err := mssql.NewAccessTokenConnector(dsn, func() (string, error) { return token, nil })
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(c)
	t.Cleanup(func() { _ = db.Close() })
	return db, db.PingContext(context.Background())
}

func (f *secFixture) grant(t *testing.T, it *store.Item, oid string, additional ...string) {
	t.Helper()
	if err := f.srv.Store.PutItemAccess(store.ItemAccess{ItemID: it.ID, PrincipalID: oid, PrincipalType: "User",
		Permissions: []string{store.PermRead}, Additional: additional}); err != nil {
		t.Fatal(err)
	}
}

func (f *secFixture) revoke(t *testing.T, it *store.Item, oid string) {
	t.Helper()
	if err := f.srv.Store.DeleteItemAccess(it.ID, oid); err != nil {
		t.Fatal(err)
	}
}

func (f *secFixture) secondWarehouse(t *testing.T, name string) *store.Item {
	t.Helper()
	wh := &store.Item{WorkspaceID: f.ws.ID, Type: "Warehouse", DisplayName: name}
	if err := f.srv.Store.CreateItem(wh, nil); err != nil {
		t.Fatal(err)
	}
	return wh
}

func count(db *sql.DB, query string) (int, error) {
	var n int
	err := db.QueryRowContext(context.Background(), query).Scan(&n)
	return n, err
}

// A warehouse shared for Read and ReadData is readable by somebody with no role
// in its workspace — read-only, as sharing grants no write — and stops being so
// when the grant is revoked.
func TestRelayAdmitsAStrangerSharedAWarehouse(t *testing.T) {
	f := newSecFixture(t)
	owner := f.connectAs(t, entra.DaemonClientID, store.RoleAdmin)
	mustExec(t, owner, `CREATE TABLE dbo.shared (v int)`, `INSERT INTO dbo.shared VALUES (1), (2)`)

	if _, err := f.dial(t, strangerOID, f.wh.ID); err == nil {
		t.Fatal("a stranger with no grant connected")
	}

	f.grant(t, f.wh, strangerOID, store.PermReadData)
	db, err := f.dial(t, strangerOID, f.wh.ID)
	if err != nil {
		t.Fatalf("connect with Read and ReadData: %v", err)
	}
	if n, err := count(db, `SELECT COUNT(*) FROM dbo.shared`); err != nil || n != 2 {
		t.Fatalf("read with ReadData = %d, %v; want 2", n, err)
	}
	if _, err := db.ExecContext(context.Background(), `INSERT INTO dbo.shared VALUES (3)`); err == nil {
		t.Fatal("a share granted write")
	}

	f.revoke(t, f.wh, strangerOID)
	if _, err := f.dial(t, strangerOID, f.wh.ID); err == nil {
		t.Fatal("the stranger connected after the revoke")
	}
}

// Read without ReadData connects and reads nothing — until an owner authors a
// T-SQL GRANT, which is Fabric's own model: Read lets a principal connect, and
// data comes from ReadData or from SQL permissions.
func TestRelayReadAloneConnectsButCannotSelect(t *testing.T) {
	f := newSecFixture(t)
	owner := f.connectAs(t, entra.DaemonClientID, store.RoleAdmin)
	mustExec(t, owner, `CREATE TABLE dbo.guarded (v int)`, `INSERT INTO dbo.guarded VALUES (1)`)

	f.grant(t, f.wh, strangerOID)
	db, err := f.dial(t, strangerOID, f.wh.ID)
	if err != nil {
		t.Fatalf("connect with Read: %v", err)
	}
	if _, err := count(db, `SELECT COUNT(*) FROM dbo.guarded`); err == nil {
		t.Fatal("Read alone selected data")
	}
	mustExec(t, owner, `GRANT SELECT ON dbo.guarded TO [`+strangerOID+`]`)
	if n, err := count(db, `SELECT COUNT(*) FROM dbo.guarded`); err != nil || n != 1 {
		t.Fatalf("after an owner's T-SQL GRANT = %d, %v; want 1", n, err)
	}
}

// The case syncing exists for. A principal shared two warehouses reads the second
// by three-part name from the first. Revoke the second and reconnect to the first:
// the lingering database user in the second must lose CONNECT, which also
// neutralises an explicit GRANT an owner once authored there.
func TestRelayARevokedGrantStopsWorkingAcrossDatabases(t *testing.T) {
	f := newSecFixture(t)
	other := f.secondWarehouse(t, "wh2")
	ownerA := f.connectAs(t, entra.DaemonClientID, store.RoleAdmin)
	ownerB, err := f.dial(t, entra.DaemonClientID, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, ownerA, `CREATE TABLE dbo.a (v int)`)
	mustExec(t, ownerB, `CREATE TABLE dbo.b (v int)`, `INSERT INTO dbo.b VALUES (1), (2), (3)`)

	f.grant(t, f.wh, strangerOID, store.PermReadData)
	f.grant(t, other, strangerOID, store.PermReadData)
	// Connect to B once so the principal exists there, then read B from A.
	if _, err := f.dial(t, strangerOID, other.ID); err != nil {
		t.Fatal(err)
	}
	fromA, err := f.dial(t, strangerOID, f.wh.ID)
	if err != nil {
		t.Fatal(err)
	}
	crossDB := `SELECT COUNT(*) FROM [` + other.ID + `].dbo.b`
	if n, err := count(fromA, crossDB); err != nil || n != 3 {
		t.Fatalf("cross-database read while granted both = %d, %v; want 3", n, err)
	}
	// An explicit grant in B, the kind a revoke of item access must also defeat.
	mustExec(t, ownerB, `GRANT SELECT ON dbo.b TO [`+strangerOID+`]`)

	f.revoke(t, other, strangerOID)
	again, err := f.dial(t, strangerOID, f.wh.ID) // still granted A: this connect syncs B
	if err != nil {
		t.Fatalf("reconnecting to the still-granted warehouse: %v", err)
	}
	if n, err := count(again, crossDB); err == nil {
		t.Fatalf("after revoking B, a three-part name from A still read %d rows", n)
	}
	if _, err := f.dial(t, strangerOID, other.ID); err == nil {
		t.Fatal("after the revoke, the stranger connected to B directly")
	}
}

// A workspace role change that lowers the rung takes the old memberships away at
// the next connect. Asserted on the engine's own catalogue, because the relay's
// read-only guard would refuse a Viewer's INSERT whether or not the database
// still held db_datawriter — and it is the database membership that a
// three-part name or a future path would lean on.
func TestRelayDemotionTakesTheOldRungAway(t *testing.T) {
	f := newSecFixture(t)
	const oid = "de0de0de-0000-0000-0000-0000000de0de"
	f.connectAs(t, oid, store.RoleContributor)

	sa, err := sql.Open("sqlserver", os.Getenv("WAREHOUSE_MSSQL_DSN")+";database="+f.wh.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sa.Close() })
	member := func(role string) int {
		t.Helper()
		n, err := count(sa, `SELECT ISNULL(IS_ROLEMEMBER(N'`+role+`', N'`+oid+`'), 0)`)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if member("db_datawriter") != 1 {
		t.Fatal("fixture: a Contributor was not provisioned as a writer")
	}

	assignments, err := f.srv.Store.ListRoleAssignments(f.ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, ra := range assignments {
		if ra.Principal.ID == oid {
			if err := f.srv.Store.UpdateRoleAssignment(f.ws.ID, ra.ID, store.RoleViewer); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := f.dial(t, oid, f.wh.ID); err != nil {
		t.Fatalf("reconnect as a Viewer: %v", err)
	}
	if member("db_datawriter") != 0 || member("db_ddladmin") != 0 {
		t.Fatal("a demoted Contributor kept its writer memberships")
	}
	if member("db_datareader") != 1 {
		t.Fatal("the demotion took away the read a Viewer keeps")
	}
}
