package server_test

// "In delegated mode … If the source table has any OneLake-level security rule
// applied — such as row-level security (RLS) or column-level security (CLS) — the
// SQL analytics endpoint blocks access to that shortcut", whatever the reader's
// SQL permissions. It is the item owner's identity that reads OneLake there, and
// it can only read a table whole. User identity mode reads as the caller instead.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	entra "github.com/calvinchengx/entra-emulator/emulator"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

func TestDelegatedModeBlocksAShortcutWhoseSourceIsSecured(t *testing.T) {
	f := newSecFixture(t)
	web := httptest.NewServer(f.srv.Handler())
	t.Cleanup(web.Close)
	consumer, endpoint := f.lakehouse(t)
	producer := f.producer(t, "producer")
	carol := "cccc3333-0000-0000-0000-0000000007c3" // Contributor
	f.grantRole(t, carol, store.RoleContributor)
	f.commitDelta(t, producer, "rowsfiltered", 0, []whRow{{"us", 80}, {"eu", 60}})
	f.commitDelta(t, producer, "colsnarrowed", 0, []whRow{{"us", 80}, {"eu", 60}})
	f.commitDelta(t, producer, "plain", 0, []whRow{{"us", 80}, {"eu", 60}})
	for _, name := range []string{"rowsfiltered", "colsnarrowed", "plain"} {
		f.shortcutTo(t, consumer, producer, name, name)
	}
	roles := []store.OneLakeRole{
		tableRole("Filtered", "rowsfiltered", "SELECT * FROM rowsfiltered WHERE region = 'us'", nil, carol),
		tableRole("Narrowed", "colsnarrowed", "", []string{"region"}, carol),
		tableRole("Whole", "plain", "", nil, carol),
	}
	for i := range roles {
		roles[i].ItemID = producer.ID
	}
	if err := f.srv.Store.PutOneLakeRoles(producer.ID, roles); err != nil {
		t.Fatal(err)
	}

	read := func(who, table string) error {
		t.Helper()
		db, err := f.open(t, who, consumer.ID)
		if err != nil {
			t.Fatalf("%s connects: %v", who[:8], err)
		}
		return canRead(db, "SELECT region FROM dbo."+table)
	}
	blocked := func(err error) bool { return err != nil && strings.Contains(err.Error(), "blocked") }

	// Delegated, the default: a shortcut with row or column security at its
	// source is blocked for everyone, the owner included; one without is not.
	for _, who := range []string{entra.DaemonClientID, carol} {
		for _, table := range []string{"rowsfiltered", "colsnarrowed"} {
			if err := read(who, table); !blocked(err) {
				t.Errorf("%s reading %s in delegated mode: %v, want it blocked", who[:8], table, err)
			}
		}
		if err := read(who, "plain"); err != nil {
			t.Errorf("%s reading a shortcut whose source has no row or column security: %v", who[:8], err)
		}
	}

	// User identity mode reads as the caller, so the block is lifted: the owner
	// reads the table, and the source's own rules narrow everyone else.
	if code, body := f.switchMode(t, web, endpoint, entra.DaemonClientID, "UserIdentity"); code != http.StatusOK {
		t.Fatalf("switch = %d %s", code, body)
	}
	if err := read(entra.DaemonClientID, "rowsfiltered"); err != nil {
		t.Errorf("the owner reading the shortcut in user identity mode: %v", err)
	}

	// And back: blocked again, and it is a change of mode that did it.
	if code, body := f.switchMode(t, web, endpoint, entra.DaemonClientID, "DelegatedIdentity"); code != http.StatusOK {
		t.Fatalf("switch back = %d %s", code, body)
	}
	if err := read(entra.DaemonClientID, "rowsfiltered"); !blocked(err) {
		t.Errorf("after switching back to delegated: %v, want it blocked again", err)
	}
	if err := read(entra.DaemonClientID, "plain"); err != nil {
		t.Errorf("an unsecured shortcut after switching back: %v", err)
	}
}
