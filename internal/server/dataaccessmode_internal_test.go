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
		}, "reading what the switch to user identity set aside"},
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

// In user identity mode a lakehouse's grant comes from OneLake security: below
// Contributor the rung is CONNECT with the principal's OLS_ memberships, and a
// principal without Read gets none — the same whether the lakehouse is the
// target or a sibling. Decided before the engine, so no SQL Server is needed.
func TestSQLAccessInUserIdentityMode(t *testing.T) {
	st, raw, lake, ep, wh := modeStore(t)
	if err := st.SetItemProperties(ep.ID, map[string]string{store.PropDataAccessMode: store.AccessModeUserIdentity}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutOneLakeRoles(lake.ID, []store.OneLakeRole{{ItemID: lake.ID, Name: "readers", Body: []byte(
		`{"name":"readers","decisionRules":[{"effect":"Permit","permission":[
		  {"attributeName":"Path","attributeValueIncludedIn":["Tables/sales"]},
		  {"attributeName":"Action","attributeValueIncludedIn":["Read"]}]}],
		  "members":{"microsoftEntraMembers":[{"objectId":"v"}]}}`)}}); err != nil {
		t.Fatal(err)
	}
	for id, role := range map[string]string{"v": store.RoleViewer, "c": store.RoleContributor} {
		if err := st.CreateRoleAssignment(&store.RoleAssignment{WorkspaceID: lake.WorkspaceID,
			Principal: store.Principal{ID: id, Type: "User"}, Role: role}); err != nil {
			t.Fatal(err)
		}
	}
	for who, want := range map[string]tds.Grant{
		"u": {Database: lake.ID, Role: tds.RoleOwner, OneLake: true},
		"c": {Database: lake.ID, Role: tds.RoleReader, OneLake: true},
		"v": {Database: lake.ID, Role: tds.RoleConnect, OneLake: true, OneLakeRoles: []string{"OLS_readers"}},
	} {
		for _, target := range []*store.Item{lake, wh} {
			_, grants, err := sqlAccess(st, target, who)
			if err != nil {
				t.Fatalf("%s to %s: %v", who, target.Type, err)
			}
			got := targetGrant(grants, lake.ID)
			if got.Role != want.Role || !got.OneLake || strings.Join(got.OneLakeRoles, ",") != strings.Join(want.OneLakeRoles, ",") {
				t.Errorf("%s via %s: lakehouse grant %+v, want %+v", who, target.Type, got, want)
			}
		}
	}
	// No Read on the lakehouse: none, even listed as a sibling.
	grants, err := workspaceGrants(st, lake.WorkspaceID, "stranger", "")
	if err != nil {
		t.Fatal(err)
	}
	if g := targetGrant(grants, lake.ID); g.Role != tds.RoleNone {
		t.Errorf("a stranger's lakehouse grant = %+v", g)
	}
	if g := targetGrant(nil, "missing"); g.Role != tds.RoleNone {
		t.Errorf("a database the sweep does not list = %+v", g)
	}
	if err := st.PutOneLakeRoles(lake.ID, []store.OneLakeRole{{ItemID: lake.ID, Name: strings.Repeat("r", 130), Body: []byte(
		`{"name":"x","decisionRules":[{"effect":"Permit","permission":[
		  {"attributeName":"Path","attributeValueIncludedIn":["*"]},
		  {"attributeName":"Action","attributeValueIncludedIn":["Read"]}]}],
		  "members":{"microsoftEntraMembers":[{"objectId":"v"}]}}`)}}); err == nil {
		grants, _ = workspaceGrants(st, lake.WorkspaceID, "v", store.RoleViewer)
		if g := targetGrant(grants, lake.ID); len(g.OneLakeRoles) != 0 {
			t.Errorf("a role name no SQL role can carry was synced: %+v", g)
		}
	}

	if _, err := raw.Exec(`DROP TABLE onelake_roles`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := sqlAccess(st, wh, "u"); err == nil {
		t.Error("unreadable OneLake roles were granted")
	}
	if _, err := raw.Exec(`DROP TABLE item_properties`); err != nil {
		t.Fatal(err)
	}
	if _, err := workspaceGrants(st, lake.WorkspaceID, "u", store.RoleAdmin); err == nil {
		t.Error("an unreadable mode for a sibling was granted")
	}
}

// The sync fails closed when the store cannot answer.
func TestOneLakeSyncFailsClosed(t *testing.T) {
	ctx := context.Background()
	st, raw, lake, ep, _ := modeStore(t)
	sqlite, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlite.Close() })
	if err := st.SetItemProperties(ep.ID, map[string]string{store.PropDataAccessMode: store.AccessModeUserIdentity}); err != nil {
		t.Fatal(err)
	}
	// SQLite has no sys.tables: the endpoint cannot be listed.
	if err := syncOneLakeRoles(ctx, sqlite, st, lake); err == nil || !strings.Contains(err.Error(), "listing the endpoint's tables") {
		t.Errorf("an endpoint that cannot be listed: %v", err)
	}
	if _, err := raw.Exec(`DROP TABLE onelake_roles`); err != nil {
		t.Fatal(err)
	}
	if err := syncOneLakeRoles(ctx, sqlite, st, lake); err == nil {
		t.Error("unreadable OneLake roles synced")
	}
	if _, err := raw.Exec(`DROP TABLE item_properties`); err != nil {
		t.Fatal(err)
	}
	if err := syncIfUserIdentity(ctx, &fakeWH{db: sqlite}, st, lake); err == nil {
		t.Error("an unreadable access mode skipped the sync")
	}
}
