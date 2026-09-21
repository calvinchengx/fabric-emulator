package server_test

// Shortcuts on a lakehouse's SQL analytics endpoint (docs/61). Learn: "Shortcuts
// function as tables in the SQL analytics endpoint" — so a OneLake shortcut under
// Tables/ must read as a table, from its source.

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	entra "github.com/calvinchengx/entra-emulator/emulator"
	"github.com/parquet-go/parquet-go"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// commitDelta writes one Delta commit of rows into item's table.
func (f *secFixture) commitDelta(t *testing.T, item *store.Item, table string, version int, rows []whRow) {
	t.Helper()
	var buf bytes.Buffer
	pw := parquet.NewGenericWriter[whRow](&buf)
	if _, err := pw.Write(rows); err != nil {
		t.Fatal(err)
	}
	_ = pw.Close()
	part := fmt.Sprintf("part-%d.parquet", version)
	for rel, content := range map[string][]byte{
		"Tables/" + table + "/" + part:                                 buf.Bytes(),
		fmt.Sprintf("Tables/%s/_delta_log/%020d.json", table, version): []byte(fmt.Sprintf(`{"add":{"path":%q}}`, part)),
	} {
		if err := f.srv.Store.CreateOneLakePath(&store.OneLakePath{WorkspaceID: item.WorkspaceID, ItemID: item.ID, RelPath: rel, Content: content}, false); err != nil {
			t.Fatal(err)
		}
	}
}

// shortcutTo makes consumer's Tables/<name> a shortcut to producer's Tables/<table>.
func (f *secFixture) shortcutTo(t *testing.T, consumer, producer *store.Item, name, table string) {
	t.Helper()
	if err := f.srv.Store.CreateShortcut(&store.Shortcut{ItemID: consumer.ID, Path: "Tables", Name: name,
		TargetWorkspace: producer.WorkspaceID, TargetItem: producer.ID, TargetPath: "Tables/" + table, TargetType: "OneLake"}); err != nil {
		t.Fatal(err)
	}
}

func (f *secFixture) producer(t *testing.T, name string) *store.Item {
	t.Helper()
	it := &store.Item{WorkspaceID: f.ws.ID, Type: "Lakehouse", DisplayName: name}
	if err := f.srv.Store.CreateItem(it, nil); err != nil {
		t.Fatal(err)
	}
	return it
}

func TestAShortcutUnderTablesReadsAsATableOnTheEndpoint(t *testing.T) {
	f := newSecFixture(t)
	consumer, _ := f.lakehouse(t)
	producer := f.producer(t, "producer")
	f.commitDelta(t, producer, "orders", 0, []whRow{{"us", 80}, {"eu", 60}})
	f.shortcutTo(t, consumer, producer, "orders", "orders")

	db, err := f.open(t, entra.DaemonClientID, consumer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n := scalar(t, db, `SELECT COUNT(*) FROM dbo.orders`); n != 2 {
		t.Errorf("the shortcut reads %d row(s), want 2", n)
	}

	// The source changes; the shortcut follows it, as it is not a copy. A commit
	// that only adds a file adds its rows to the table's.
	f.commitDelta(t, producer, "orders", 1, []whRow{{"ap", 5}, {"ap", 6}, {"ap", 7}})
	db, err = f.open(t, entra.DaemonClientID, consumer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n := scalar(t, db, `SELECT COUNT(*) FROM dbo.orders`); n != 5 {
		t.Errorf("after the source changed the shortcut reads %d row(s), want 5", n)
	}
}

// "Users must have valid access on both the shortcut source … and the
// destination where the data physically resides. If the user lacks permission on
// either side, queries fail with an access error." The consumer's side is its
// OneLake roles; the source's is the source item's own — its roles, or its
// ReadAll. A Contributor is never narrowed by OneLake security.
func TestAShortcutTableNeedsAccessOnTheSourceToo(t *testing.T) {
	f := newSecFixture(t)
	web := httptest.NewServer(f.srv.Handler())
	t.Cleanup(web.Close)
	consumer, endpoint := f.lakehouse(t)
	producer := f.producer(t, "producer")
	alice := "aaaa1111-0000-0000-0000-0000000005c1" // Viewer; in the consumer's role only
	bob := "bbbb2222-0000-0000-0000-0000000005c2"   // Viewer; in both sides' roles
	carol := "cccc3333-0000-0000-0000-0000000005c3" // Contributor; in no role
	f.grantRole(t, alice, store.RoleViewer)
	f.grantRole(t, bob, store.RoleViewer)
	f.grantRole(t, carol, store.RoleContributor)

	f.commitDelta(t, producer, "orders", 0, []whRow{{"us", 80}, {"eu", 60}})
	f.shortcutTo(t, consumer, producer, "orders", "orders")
	put := func(item *store.Item, role store.OneLakeRole) {
		t.Helper()
		role.ItemID = item.ID
		if err := f.srv.Store.PutOneLakeRoles(item.ID, []store.OneLakeRole{role}); err != nil {
			t.Fatal(err)
		}
	}
	both := store.OneLakeRole{Name: "Readers", Body: []byte(`{"name":"Readers","decisionRules":[{"effect":"Permit","permission":[
	  {"attributeName":"Path","attributeValueIncludedIn":["Tables/orders"]},
	  {"attributeName":"Action","attributeValueIncludedIn":["Read"]}]}],
	  "members":{"microsoftEntraMembers":[{"objectId":"` + alice + `"},{"objectId":"` + bob + `"}]}}`)}
	put(consumer, both)
	put(producer, oneLakeRole("SourceReaders", "orders", bob))
	if code, body := f.switchMode(t, web, endpoint, entra.DaemonClientID, "UserIdentity"); code != http.StatusOK {
		t.Fatalf("switch = %d %s", code, body)
	}

	reads := func(who string) error {
		t.Helper()
		db, err := f.open(t, who, consumer.ID)
		if err != nil {
			t.Fatalf("%s connects: %v", who[:8], err)
		}
		return canRead(db, "SELECT region FROM dbo.orders")
	}
	if err := reads(alice); err == nil {
		t.Error("alice reads the shortcut table with no access at its source")
	}
	if err := reads(bob); err != nil {
		t.Errorf("bob, with access on both sides: %v", err)
	}
	if err := reads(carol); err != nil {
		t.Errorf("a Contributor, whom OneLake security never narrows: %v", err)
	}

	// Access at the source arrives; the refusal is lifted on the next connect.
	put(producer, store.OneLakeRole{Name: "SourceReaders", Body: []byte(`{"name":"SourceReaders","decisionRules":[{"effect":"Permit","permission":[
	  {"attributeName":"Path","attributeValueIncludedIn":["Tables/orders"]},
	  {"attributeName":"Action","attributeValueIncludedIn":["Read"]}]}],
	  "members":{"microsoftEntraMembers":[{"objectId":"` + alice + `"},{"objectId":"` + bob + `"}]}}`)})
	if err := reads(alice); err != nil {
		t.Errorf("alice after access at the source: %v", err)
	}
}

// tableRole is a OneLake role granting one table, narrowed to columns and/or a
// row filter, to the given members.
func tableRole(name, table, rows string, columns []string, members ...string) store.OneLakeRole {
	var ms []string
	for _, m := range members {
		ms = append(ms, fmt.Sprintf(`{"objectId":%q}`, m))
	}
	constraints := ""
	var parts []string
	if rows != "" {
		parts = append(parts, fmt.Sprintf(`"rows":[{"tablePath":"/Tables/%s","value":%q}]`, table, rows))
	}
	if len(columns) > 0 {
		parts = append(parts, fmt.Sprintf(`"columns":[{"tablePath":"/Tables/%s","columnNames":["%s"],"columnEffect":"Permit","columnAction":["Read"]}]`,
			table, strings.Join(columns, `","`)))
	}
	if len(parts) > 0 {
		constraints = `,"constraints":{` + strings.Join(parts, ",") + `}`
	}
	return store.OneLakeRole{Name: name, Body: []byte(fmt.Sprintf(`{"name":%q,"decisionRules":[{"effect":"Permit","permission":[
	  {"attributeName":"Path","attributeValueIncludedIn":["Tables/%s"]},
	  {"attributeName":"Action","attributeValueIncludedIn":["Read"]}]%s}],
	  "members":{"microsoftEntraMembers":[%s]}}`, name, table, constraints, strings.Join(ms, ",")))}
}

// "Enforced at the source of truth": what the source's roles narrow — columns and
// rows — narrows the consumer's read of a shortcut table too, on top of whatever
// the consumer's own roles say, and the most restrictive outcome wins.
func TestAShortcutTableIsNarrowedByTheSourcesColumnsAndRows(t *testing.T) {
	f := newSecFixture(t)
	web := httptest.NewServer(f.srv.Handler())
	t.Cleanup(web.Close)
	consumer, endpoint := f.lakehouse(t)
	producer := f.producer(t, "producer")
	alice := "aaaa1111-0000-0000-0000-0000000006c1" // narrowed at the source to region 'us'; and at the consumer to everything but 'eu'
	bob := "bbbb2222-0000-0000-0000-0000000006c2"   // unrestricted at both
	dora := "dddd4444-0000-0000-0000-0000000006c4"  // narrowed at the source to the region column only
	carol := "cccc3333-0000-0000-0000-0000000006c3" // Contributor
	erin := "eeee5555-0000-0000-0000-0000000006c5"  // Contributor, in the source role that filters to region 'us'
	for _, m := range []string{alice, bob, dora} {
		f.grantRole(t, m, store.RoleViewer)
	}
	f.grantRole(t, carol, store.RoleContributor)
	f.grantRole(t, erin, store.RoleContributor)
	f.commitDelta(t, producer, "orders", 0, []whRow{{"us", 80}, {"eu", 60}, {"ap", 90}})
	f.shortcutTo(t, consumer, producer, "orders", "orders")
	put := func(item *store.Item, roles ...store.OneLakeRole) {
		t.Helper()
		for i := range roles {
			roles[i].ItemID = item.ID
		}
		if err := f.srv.Store.PutOneLakeRoles(item.ID, roles); err != nil {
			t.Fatal(err)
		}
	}
	put(consumer,
		tableRole("Plain", "orders", "", nil, bob, dora),
		tableRole("NotEurope", "orders", "SELECT * FROM orders WHERE region <> 'eu'", nil, alice))
	put(producer,
		tableRole("Everything", "orders", "", nil, bob),
		tableRole("US", "orders", "SELECT * FROM orders WHERE region = 'us'", nil, alice, erin),
		tableRole("RegionOnly", "orders", "", []string{"region"}, dora))
	if code, body := f.switchMode(t, web, endpoint, entra.DaemonClientID, "UserIdentity"); code != http.StatusOK {
		t.Fatalf("switch = %d %s", code, body)
	}

	count := func(who string) int {
		t.Helper()
		db, err := f.open(t, who, consumer.ID)
		if err != nil {
			t.Fatalf("%s connects: %v", who[:8], err)
		}
		return scalar(t, db, "SELECT COUNT(*) FROM dbo.orders")
	}
	if n := count(alice); n != 1 {
		t.Errorf("alice, narrowed to everything but 'eu' here and to 'us' at the source, sees %d row(s), want 1 — the AND of both", n)
	}
	if n := count(bob); n != 3 {
		t.Errorf("bob, unrestricted on both sides, sees %d row(s), want 3", n)
	}
	if n := count(carol); n != 3 {
		t.Errorf("a Contributor, whom OneLake security does not narrow, sees %d row(s), want 3", n)
	}
	if n := count(erin); n != 1 {
		t.Errorf("a Contributor in the source role that filters sees %d row(s), want 1: row-level security is enforced for them too", n)
	}
	dora1, err := f.open(t, dora, consumer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := canRead(dora1, "SELECT region FROM dbo.orders"); err != nil {
		t.Errorf("dora reads the column the source permits her: %v", err)
	}
	if err := canRead(dora1, "SELECT amount FROM dbo.orders"); err == nil {
		t.Error("dora reads a column the source withholds, because the consumer's own role grants the whole table")
	}
}

// SQL Server refuses a read that names no denied column when a row policy takes
// that column as an argument: the caller needs SELECT on the columns the
// predicate reads. So where a filter reads a column the source withholds from a
// reader, that reader's every read of the table is refused — closed, not open,
// and stricter than OneLake, which narrows them independently. Pinned so that it
// stays a refusal (docs/61).
func TestAColumnTheSourceWithholdsAndAFilterReadsFailsClosed(t *testing.T) {
	f := newSecFixture(t)
	web := httptest.NewServer(f.srv.Handler())
	t.Cleanup(web.Close)
	consumer, endpoint := f.lakehouse(t)
	producer := f.producer(t, "producer")
	alice := "aaaa1111-0000-0000-0000-0000000006d1" // filtered by amount at the consumer
	dora := "dddd4444-0000-0000-0000-0000000006d4"  // withheld the amount column at the source
	f.grantRole(t, alice, store.RoleViewer)
	f.grantRole(t, dora, store.RoleViewer)
	f.commitDelta(t, producer, "orders", 0, []whRow{{"us", 80}, {"eu", 60}})
	f.shortcutTo(t, consumer, producer, "orders", "orders")
	for item, roles := range map[*store.Item][]store.OneLakeRole{
		consumer: {tableRole("Plain", "orders", "", nil, dora), tableRole("Big", "orders", "SELECT * FROM orders WHERE amount >= 80", nil, alice)},
		producer: {tableRole("Everything", "orders", "", nil, alice), tableRole("RegionOnly", "orders", "", []string{"region"}, dora)},
	} {
		for i := range roles {
			roles[i].ItemID = item.ID
		}
		if err := f.srv.Store.PutOneLakeRoles(item.ID, roles); err != nil {
			t.Fatal(err)
		}
	}
	if code, body := f.switchMode(t, web, endpoint, entra.DaemonClientID, "UserIdentity"); code != http.StatusOK {
		t.Fatalf("switch = %d %s", code, body)
	}
	db, err := f.open(t, dora, consumer.ID)
	if err != nil {
		t.Fatal(err)
	}
	err = canRead(db, "SELECT region FROM dbo.orders")
	if err == nil || !strings.Contains(err.Error(), "SELECT permission was denied on the column 'amount'") {
		t.Errorf("dora's read, with a filter on the column the source withholds: %v, want the column refused", err)
	}
	if db2, err := f.open(t, alice, consumer.ID); err != nil {
		t.Fatal(err)
	} else if n := scalar(t, db2, "SELECT COUNT(*) FROM dbo.orders"); n != 1 {
		t.Errorf("alice sees %d row(s), want 1", n)
	}
}

// The same collision, with no shortcut: one role narrows a reader to the region
// column, another filters other readers' rows by amount. Found while building the
// shortcut case and pinned here because it is the merged design's, not the
// shortcut's: the policy's predicate takes `amount`, so the region-only reader is
// refused on it even for a query that names only region.
func TestAColumnNarrowedReaderAndAFilterOnThatColumnFailsClosed(t *testing.T) {
	f := newSecFixture(t)
	web := httptest.NewServer(f.srv.Handler())
	t.Cleanup(web.Close)
	lake, endpoint := f.lakehouse(t)
	alice := "aaaa1111-0000-0000-0000-0000000006e1"
	dora := "dddd4444-0000-0000-0000-0000000006e4"
	f.grantRole(t, alice, store.RoleViewer)
	f.grantRole(t, dora, store.RoleViewer)
	f.commitDelta(t, lake, "orders", 0, []whRow{{"us", 80}, {"eu", 60}})
	roles := []store.OneLakeRole{
		tableRole("RegionOnly", "orders", "", []string{"region"}, dora),
		tableRole("Big", "orders", "SELECT * FROM orders WHERE amount >= 80", nil, alice),
	}
	for i := range roles {
		roles[i].ItemID = lake.ID
	}
	if err := f.srv.Store.PutOneLakeRoles(lake.ID, roles); err != nil {
		t.Fatal(err)
	}
	if code, body := f.switchMode(t, web, endpoint, entra.DaemonClientID, "UserIdentity"); code != http.StatusOK {
		t.Fatalf("switch = %d %s", code, body)
	}
	db, err := f.open(t, dora, lake.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := canRead(db, "SELECT region FROM dbo.orders"); err == nil || !strings.Contains(err.Error(), "SELECT permission was denied on the column 'amount'") {
		t.Errorf("dora's read of the column she is granted: %v, want the predicate's column refused", err)
	}
}
