package server

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/clock"
	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/calvinchengx/fabric-emulator/internal/tds"
)

type countingCloser struct{ closed []string }

func (c *countingCloser) CloseSessions(dbs ...string) int {
	c.closed = append(c.closed, dbs...)
	return len(dbs)
}

// modeStore is an on-disk store with a lakehouse, its endpoint and a warehouse,
// and a raw handle for breaking it.
func modeStore(t *testing.T) (*store.Store, *sql.DB, *store.Item, *store.Item, *store.Item) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir, clock.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	raw, err := sql.Open("sqlite", filepath.Join(dir, "fabric-emulator.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	ws := &store.Workspace{DisplayName: "w"}
	if err := st.CreateWorkspace(ws, store.Principal{ID: "u", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	lake := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lake"}
	ep := &store.Item{WorkspaceID: ws.ID, Type: "SQLEndpoint", DisplayName: "lake"}
	wh := &store.Item{WorkspaceID: ws.ID, Type: "Warehouse", DisplayName: "wh"}
	for _, it := range []*store.Item{lake, ep, wh} {
		if err := st.CreateItem(it, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SetItemProperties(ep.ID, map[string]string{store.PropParentLakehouse: lake.ID}); err != nil {
		t.Fatal(err)
	}
	return st, raw, lake, ep, wh
}

// A switch whose SQL cannot run leaves the mode as it was — here the engine is
// SQLite, which has no sys.security_policies — but the sessions it had to end
// are still ended, first, as Fabric takes the endpoints offline before
// applying the change.
func TestDataAccessModeSwitchFailures(t *testing.T) {
	ctx := context.Background()
	st, raw, lake, ep, wh := modeStore(t)
	closer := &countingCloser{}
	sqlite, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlite.Close() })

	for name, tc := range map[string]struct {
		be   *fakeWH
		to   string
		prep func()
		want string
	}{
		"the database cannot be prepared":   {&fakeWH{db: sqlite, ensureErr: errors.New("boom")}, store.AccessModeUserIdentity, func() {}, "preparing database"},
		"the engine refuses the switch in":  {&fakeWH{db: sqlite}, store.AccessModeUserIdentity, func() {}, "switching to user identity"},
		"the engine refuses the switch out": {&fakeWH{db: sqlite}, store.AccessModeDelegated, func() {}, "switching to delegated identity"},
		"the remembered policies are unreadable": {&fakeWH{db: sqlite}, store.AccessModeDelegated, func() {
			if err := st.SetItemProperties(ep.ID, map[string]string{propDisabledPolicies: "not json"}); err != nil {
				t.Fatal(err)
			}
		}, "reading the policies a switch turned off"},
	} {
		tc.prep()
		err := dataAccessModeSwitch(tc.be, st, closer)(ctx, ep, lake, tc.to)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
		if m, _ := st.DataAccessMode(lake); m != store.AccessModeDelegated {
			t.Errorf("%s: a failed switch changed the mode to %q", name, m)
		}
	}
	// Every SQL item in the workspace had its sessions ended, each time.
	if got := strings.Join(closer.closed[:2], ","); !strings.Contains(got, lake.ID) || !strings.Contains(got, wh.ID) {
		t.Errorf("closed %v, want the lakehouse and the warehouse", closer.closed)
	}

	if _, err := raw.Exec(`DROP TABLE item_properties`); err != nil {
		t.Fatal(err)
	}
	if err := dataAccessModeSwitch(&fakeWH{db: sqlite}, st, closer)(ctx, ep, lake, store.AccessModeUserIdentity); err == nil {
		t.Error("unreadable endpoint properties did not fail the switch")
	}
	if _, err := raw.Exec(`DROP TABLE items`); err != nil {
		t.Fatal(err)
	}
	if err := dataAccessModeSwitch(&fakeWH{db: sqlite}, st, closer)(ctx, ep, lake, store.AccessModeUserIdentity); err == nil {
		t.Error("an unreadable workspace did not fail the switch")
	}
}

// The access decision refuses a lakehouse in user identity mode — at the
// endpoint and as a neighbour's three-part name — and fails closed when the
// mode cannot be read. No SQL Server needed: it decides before the engine.
func TestSQLAccessInUserIdentityMode(t *testing.T) {
	st, raw, lake, ep, wh := modeStore(t)
	if err := st.SetItemProperties(ep.ID, map[string]string{store.PropDataAccessMode: store.AccessModeUserIdentity}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := sqlAccess(st, lake, "u"); !errors.Is(err, errUserIdentityNotServed) {
		t.Errorf("lakehouse in user identity = %v", err)
	}
	_, grants, err := sqlAccess(st, wh, "u")
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range grants {
		if g.Database == lake.ID && g.Role != tds.RoleNone {
			t.Errorf("the lakehouse sibling's rung = %v, want none", g.Role)
		}
		if g.Database == wh.ID && g.Role != tds.RoleOwner {
			t.Errorf("the warehouse's rung = %v", g.Role)
		}
	}
	if _, err := raw.Exec(`DROP TABLE item_properties`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := sqlAccess(st, lake, "u"); err == nil || errors.Is(err, errUserIdentityNotServed) {
		t.Errorf("an unreadable mode at the endpoint = %v", err)
	}
	if _, err := workspaceGrants(st, lake.WorkspaceID, "u", store.RoleAdmin); err == nil {
		t.Error("an unreadable mode for a sibling was granted")
	}
}
