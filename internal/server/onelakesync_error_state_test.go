package server_test

// "Row-level security policy references a column that no longer exists.
// Database enters error state until policy is fixed." — and the same for
// column-level security. Not a per-role narrowing, the way invalid RLS syntax
// is; the whole sync fails, so every read through the endpoint fails until the
// role is fixed at its source (docs/60).

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	entra "github.com/calvinchengx/entra-emulator/emulator"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// putSalesAndSwitch is the shared setup: a sales table, an endpoint switched to
// user identity mode, and roles is the ONLY thing that varies per test.
func (f *secFixture) putSalesAndSwitch(t *testing.T, web *httptest.Server, lake, endpoint *store.Item, roles []store.OneLakeRole) {
	t.Helper()
	svc, err := f.srv.API.LakehouseDB(t.Context(), lake.ID)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, svc, `IF OBJECT_ID('dbo.sales', 'U') IS NULL
BEGIN
  CREATE TABLE dbo.sales (region varchar(10), amount int);
  INSERT INTO dbo.sales VALUES ('west', 1);
END`)
	for i := range roles {
		roles[i].ItemID = lake.ID
	}
	if err := f.srv.Store.PutOneLakeRoles(lake.ID, roles); err != nil {
		t.Fatal(err)
	}
	if code, body := f.switchMode(t, web, endpoint, entra.DaemonClientID, "UserIdentity"); code != http.StatusOK {
		t.Fatalf("switch = %d %s", code, body)
	}
}

func TestARowLevelSecurityRoleNamingAMissingColumnFailsTheWholeSync(t *testing.T) {
	f := newSecFixture(t)
	web := httptest.NewServer(f.srv.Handler())
	t.Cleanup(web.Close)
	lake, endpoint := f.lakehouse(t)
	bystander := "aaaa1111-0000-0000-0000-0000000008a1" // in a role that grants sales whole
	broken := "bbbb2222-0000-0000-0000-0000000008b2"    // in the role naming the missing column
	f.grantRole(t, bystander, store.RoleViewer)
	f.grantRole(t, broken, store.RoleViewer)
	f.putSalesAndSwitch(t, web, lake, endpoint, []store.OneLakeRole{
		oneLakeRole("Whole", "sales", bystander),
		rowRoleFor(t, "sales", "SELECT * FROM sales WHERE gone = 'x'", broken),
	})

	for _, oid := range []string{bystander, broken} {
		_, err := f.open(t, oid, lake.ID)
		if err == nil || !strings.Contains(err.Error(), "Row-level security policy references a column that no longer exists.") {
			t.Errorf("%s: %v, want the documented RLS error — a bystander in an unrelated, valid role is refused too", oid[:8], err)
		}
	}

	// Fixed at the source: a fresh connect succeeds, for everyone.
	f.putSalesAndSwitch(t, web, lake, endpoint, []store.OneLakeRole{oneLakeRole("Whole", "sales", bystander, "region")})
	if _, err := f.open(t, bystander, lake.ID); err != nil {
		t.Errorf("after the role is fixed: %v", err)
	}
}

func TestAColumnLevelSecurityRoleNamingAMissingColumnFailsTheWholeSync(t *testing.T) {
	f := newSecFixture(t)
	web := httptest.NewServer(f.srv.Handler())
	t.Cleanup(web.Close)
	lake, endpoint := f.lakehouse(t)
	broken := "cccc3333-0000-0000-0000-0000000008c3"
	f.grantRole(t, broken, store.RoleViewer)
	f.putSalesAndSwitch(t, web, lake, endpoint, []store.OneLakeRole{
		oneLakeRole("Narrowed", "sales", broken, "region", "gone"),
	})
	_, err := f.open(t, broken, lake.ID)
	if err == nil || !strings.Contains(err.Error(), "Column-level security policy references a column that no longer exists.") {
		t.Errorf("%v, want the documented CLS error", err)
	}
}

// The two failures stay distinct: invalid RLS syntax (Microsoft's grammar
// refuses it outright) still narrows the role to no rows, exactly as before —
// it is not evidence of a column the schema lost.
func TestInvalidRLSSyntaxStillNarrowsRatherThanFailingTheSync(t *testing.T) {
	f := newSecFixture(t)
	web := httptest.NewServer(f.srv.Handler())
	t.Cleanup(web.Close)
	lake, endpoint := f.lakehouse(t)
	oid := "dddd4444-0000-0000-0000-0000000008d4"
	f.grantRole(t, oid, store.RoleContributor)
	f.putSalesAndSwitch(t, web, lake, endpoint, []store.OneLakeRole{
		rowRoleFor(t, "sales", "SELECT * FROM sales WHERE region = USER_NAME()", oid),
	})
	db, err := f.open(t, oid, lake.ID)
	if err != nil {
		t.Fatalf("invalid RLS syntax must not fail the sync: %v", err)
	}
	if n := scalar(t, db, `SELECT COUNT(*) FROM dbo.sales`); n != 0 {
		t.Errorf("%d row(s), want 0 — invalid syntax narrows to none", n)
	}
}

// A shortcut source's row filter is held to the same rule: a column its schema
// no longer has fails the whole sync on the CONSUMER's endpoint too, not only a
// narrowing of the shortcut's rows.
func TestARowLevelSecurityRoleAtAShortcutSourceNamingAMissingColumnFailsTheSync(t *testing.T) {
	f := newSecFixture(t)
	web := httptest.NewServer(f.srv.Handler())
	t.Cleanup(web.Close)
	consumer, endpoint := f.lakehouse(t)
	producer := f.producer(t, "producer")
	oid := "eeee5555-0000-0000-0000-0000000008e5"
	f.grantRole(t, oid, store.RoleViewer)
	f.commitDelta(t, producer, "orders", 0, []whRow{{"us", 80}})
	f.shortcutTo(t, consumer, producer, "orders", "orders")
	put(t, f, consumer, []store.OneLakeRole{oneLakeRole("Consumer", "orders", oid)})
	put(t, f, producer, []store.OneLakeRole{rowRoleFor(t, "orders", "SELECT * FROM orders WHERE gone = 'x'", oid)})
	if code, body := f.switchMode(t, web, endpoint, entra.DaemonClientID, "UserIdentity"); code != http.StatusOK {
		t.Fatalf("switch = %d %s", code, body)
	}
	_, err := f.open(t, oid, consumer.ID)
	if err == nil || !strings.Contains(err.Error(), "Row-level security policy references a column that no longer exists.") {
		t.Errorf("%v, want the documented RLS error naming the shortcut's source", err)
	}
}

func put(t *testing.T, f *secFixture, item *store.Item, roles []store.OneLakeRole) {
	t.Helper()
	for i := range roles {
		roles[i].ItemID = item.ID
	}
	if err := f.srv.Store.PutOneLakeRoles(item.ID, roles); err != nil {
		t.Fatal(err)
	}
}

// rowRoleFor is onelakesync_test.go's rowRole, usable from this file too: a
// role granting table, filtered by filter, to members.
func rowRoleFor(t *testing.T, table, filter string, members ...string) store.OneLakeRole {
	t.Helper()
	var ms []string
	for _, m := range members {
		ms = append(ms, fmt.Sprintf(`{"objectId":%q}`, m))
	}
	return store.OneLakeRole{Name: "R", Body: []byte(fmt.Sprintf(`{"name":"R","decisionRules":[{"effect":"Permit","permission":[
	  {"attributeName":"Path","attributeValueIncludedIn":["Tables/%s"]},
	  {"attributeName":"Action","attributeValueIncludedIn":["Read"]}],
	  "constraints":{"rows":[{"tablePath":"/Tables/%s","value":%q}]}}],
	  "members":{"microsoftEntraMembers":[%s]}}`, table, table, filter, strings.Join(ms, ",")))}
}
