package server_test

// Shortcuts on a lakehouse's SQL analytics endpoint (docs/61). Learn: "Shortcuts
// function as tables in the SQL analytics endpoint" — so a OneLake shortcut under
// Tables/ must read as a table, from its source.

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
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
