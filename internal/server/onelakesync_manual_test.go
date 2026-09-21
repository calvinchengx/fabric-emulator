package server_test

// "Manual changes to these roles are not supported and are overwritten during the
// next sync cycle. If there are no changes to sync, security sync does not
// override manual changes." (Learn, OneLake security for SQL analytics endpoints.)
// Against a real SQL Server: a permission authored on an OLS_ role by hand
// survives a sync with nothing to sync, and is gone after one that has.

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	entra "github.com/calvinchengx/entra-emulator/emulator"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

func TestAnUnchangedSyncKeepsAManualChangeAndAChangedOneOverwritesIt(t *testing.T) {
	f := newSecFixture(t)
	web := httptest.NewServer(f.srv.Handler())
	t.Cleanup(web.Close)
	lake, endpoint := f.lakehouse(t)
	alice := "aaaa1111-0000-0000-0000-0000000000a1"
	f.grantRole(t, alice, store.RoleViewer)
	svc, err := f.srv.API.LakehouseDB(context.Background(), lake.ID)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, svc,
		`CREATE TABLE dbo.sales (region varchar(10), amount int)`,
		`INSERT INTO dbo.sales VALUES ('west', 1)`,
		`CREATE TABLE dbo.hr (name varchar(10))`,
		`INSERT INTO dbo.hr VALUES ('ada')`)
	if code, body := f.switchMode(t, web, endpoint, entra.DaemonClientID, "UserIdentity"); code != http.StatusOK {
		t.Fatalf("switch = %d %s", code, body)
	}
	put := func(columns ...string) {
		t.Helper()
		role := oneLakeRole("Readers", "sales", alice, columns...)
		role.ItemID = lake.ID
		if err := f.srv.Store.PutOneLakeRoles(lake.ID, []store.OneLakeRole{role}); err != nil {
			t.Fatal(err)
		}
	}
	// aliceReads syncs, as every connection does, and reports whether alice can
	// read hr and whether she can read sales' region.
	aliceReads := func() (hr, region bool) {
		t.Helper()
		db, err := f.open(t, alice, lake.ID)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		return canRead(db, "SELECT name FROM dbo.hr") == nil, canRead(db, "SELECT region FROM dbo.sales") == nil
	}

	put("region", "amount")
	if hr, region := aliceReads(); hr || !region {
		t.Fatalf("after the first sync: hr=%v region=%v, want hr refused and region readable", hr, region)
	}

	// A manual change. The emulator refuses table GRANTs in T-SQL in this mode
	// (Fabric says only that they "aren't supported"), so it is authored with the
	// guard off, as the one thing the sync must not be assumed to know about.
	mustExec(t, svc,
		`DISABLE TRIGGER OLS_guard ON DATABASE`,
		`GRANT SELECT ON dbo.hr TO OLS_Readers`,
		`ENABLE TRIGGER OLS_guard ON DATABASE`)

	// Nothing has changed in OneLake, so nothing is synced and the change stands.
	if hr, _ := aliceReads(); !hr {
		t.Fatal("a sync with nothing to sync overwrote the manual change")
	}
	if hr, _ := aliceReads(); !hr {
		t.Fatal("a second sync with nothing to sync overwrote the manual change")
	}

	// OneLake changes, so the role is rewritten and the manual grant goes with it.
	put("region")
	hr, region := aliceReads()
	if hr {
		t.Error("a sync with something to sync left the manual change in place")
	}
	if !region {
		t.Error("the changed sync lost the grant OneLake still gives")
	}
	assertNoManualGrant(t, svc)
}

func assertNoManualGrant(t *testing.T, db *sql.DB) {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM sys.database_permissions p
		WHERE p.class = 1 AND p.major_id = OBJECT_ID(N'dbo.hr') AND p.grantee_principal_id = DATABASE_PRINCIPAL_ID(N'OLS_Readers')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("OLS_Readers still holds %d permission(s) on dbo.hr", n)
	}
}
