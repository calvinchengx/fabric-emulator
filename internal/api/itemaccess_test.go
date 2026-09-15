package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// Item permissions on the control plane. The weight is on the SHARING RULES —
// who may grant, and how much — because a surface that stored whatever it was
// sent would pass every happy-path test while letting anyone grant themselves
// ReadAll.

var (
	stranger    = &auth.Principal{ID: "stranger-1", Type: "User"}
	contributor = &auth.Principal{ID: "contrib-1", Type: "User"}
)

func assignRole(t *testing.T, st *store.Store, wsID string, p *auth.Principal, role string) {
	t.Helper()
	if err := st.CreateRoleAssignment(&store.RoleAssignment{WorkspaceID: wsID,
		Principal: store.Principal{ID: p.ID, Type: p.Type}, Role: role}); err != nil {
		t.Fatal(err)
	}
}

func typedIn(t *testing.T, st *store.Store, wsID, typ string) *store.Item {
	t.Helper()
	it := &store.Item{WorkspaceID: wsID, DisplayName: "it-" + typ, Type: typ}
	if err := st.CreateItem(it, nil); err != nil {
		t.Fatal(err)
	}
	return it
}

func accessPV(ws *store.Workspace, it *store.Item, principalID string) map[string]string {
	return map[string]string{"wid": ws.ID, "iid": it.ID, "principalId": principalID}
}

func effective(t *testing.T, st *store.Store, it *store.Item, id string) store.Access {
	t.Helper()
	a, err := st.EffectiveItemAccess(it, id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// ---- the emulator-native surface ---------------------------------------------

func TestAnOwnerSharesALakehouseWithAStranger(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lake := typedIn(t, st, ws.ID, "Lakehouse")

	if effective(t, st, lake, stranger.ID).Has(store.PermRead) {
		t.Fatal("the stranger had access before the grant")
	}
	w := do(a.putItemAccess, admin, "PUT",
		`{"principalType":"User","permissions":[],"additionalPermissions":["ReadAll"]}`, accessPV(ws, lake, stranger.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", w.Code, w.Body)
	}
	var got itemAccessEntry
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	// "Read permission is always granted during sharing", even when not asked for.
	if !reflect.DeepEqual(got.Permissions, []string{"Read"}) || !reflect.DeepEqual(got.AdditionalPermissions, []string{"ReadAll"}) {
		t.Fatalf("stored = %+v", got)
	}
	if acc := effective(t, st, lake, stranger.ID); !acc.Has(store.PermReadAll) || acc.Role != "" {
		t.Fatalf("the grant did not reach the stranger: %+v", acc)
	}

	list := do(a.listItemAccess, admin, "GET", "", accessPV(ws, lake, ""))
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), stranger.ID) {
		t.Fatalf("list = %d %s", list.Code, list.Body)
	}
}

// The resharing rule, both halves: a grantor with Reshare shares what they hold,
// and is refused what they do not. A surface that let Reshare holders grant
// anything would pass the first half alone.
func TestAResharerGrantsOnlyWhatTheyHold(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lake := typedIn(t, st, ws.ID, "Lakehouse")

	// A Viewer holds no Reshare, so may not share at all.
	w := do(a.putItemAccess, viewer, "PUT", `{"principalType":"User"}`, accessPV(ws, lake, stranger.ID))
	if w.Code != http.StatusForbidden || errorCode(t, w) != "InsufficientPrivileges" {
		t.Fatalf("viewer without Reshare = %d %s", w.Code, w.Body)
	}

	// Given Reshare, the Viewer shares ReadData — which a Viewer inherits…
	if w := do(a.putItemAccess, admin, "PUT", `{"principalType":"User","permissions":["Reshare"]}`,
		accessPV(ws, lake, viewer.ID)); w.Code != http.StatusOK {
		t.Fatalf("granting the viewer Reshare = %d %s", w.Code, w.Body)
	}
	if w := do(a.putItemAccess, viewer, "PUT", `{"principalType":"User","additionalPermissions":["ReadData"]}`,
		accessPV(ws, lake, stranger.ID)); w.Code != http.StatusOK {
		t.Fatalf("resharing a held permission = %d %s", w.Code, w.Body)
	}
	// …but not ReadAll, which a Viewer does not hold.
	w = do(a.putItemAccess, viewer, "PUT", `{"principalType":"User","additionalPermissions":["ReadAll"]}`,
		accessPV(ws, lake, stranger.ID))
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "ReadAll") {
		t.Fatalf("resharing an unheld permission = %d %s", w.Code, w.Body)
	}
}

func TestTheNativeSurfaceRefusesWhatItCannotHonour(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lake := typedIn(t, st, ws.ID, "Lakehouse")
	for name, tc := range map[string]struct {
		principal, body, code string
	}{
		"write":            {stranger.ID, `{"principalType":"User","permissions":["Write"]}`, "PermissionNotGrantable"},
		"execute":          {stranger.ID, `{"principalType":"User","permissions":["Execute"]}`, "PermissionNotModelled"},
		"explore":          {stranger.ID, `{"principalType":"User","permissions":["Explore"]}`, "PermissionNotModelled"},
		"an unknown extra": {stranger.ID, `{"principalType":"User","additionalPermissions":["viewOutput"]}`, "PermissionNotModelled"},
		"a principal type": {stranger.ID, `{"principalType":"App"}`, "InvalidInput"},
		"a malformed body": {stranger.ID, `{`, "InvalidInput"},
		"a UPN":            {"someone@contoso.com", `{"principalType":"User"}`, "PrincipalNotResolvable"},
	} {
		w := do(a.putItemAccess, admin, "PUT", tc.body, accessPV(ws, lake, tc.principal))
		if w.Code != http.StatusBadRequest || errorCode(t, w) != tc.code {
			t.Errorf("%s = %d %s, want 400 %s", name, w.Code, w.Body, tc.code)
		}
	}
	if grants, _ := st.ListItemAccess(lake.ID); len(grants) != 0 {
		t.Fatalf("a refused PUT stored %+v", grants)
	}
}

func TestAServicePrincipalAndAGroupCanBeGrantedNatively(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	wh := typedIn(t, st, ws.ID, "Warehouse")
	for id, typ := range map[string]string{"sp-1": "ServicePrincipal", "group-1": "Group"} {
		if w := do(a.putItemAccess, admin, "PUT", `{"principalType":"`+typ+`","additionalPermissions":["ReadData"]}`,
			accessPV(ws, wh, id)); w.Code != http.StatusOK {
			t.Errorf("%s = %d %s", typ, w.Code, w.Body)
		}
	}
}

func TestSemanticModelsAreSentToTheirDocumentedAPI(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	sm := typedIn(t, st, ws.ID, "SemanticModel")
	w := do(a.putItemAccess, admin, "PUT", `{"principalType":"User"}`, accessPV(ws, sm, stranger.ID))
	if w.Code != http.StatusBadRequest || errorCode(t, w) != "UseDatasetUsersAPI" {
		t.Fatalf("PUT on a semantic model = %d %s", w.Code, w.Body)
	}
}

// Revoking takes the direct grant and nothing else: removing an item permission
// "isn't enough" to take away what a workspace role gives.
func TestRevokingAGrantLeavesTheRole(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lake := typedIn(t, st, ws.ID, "Lakehouse")
	if w := do(a.putItemAccess, admin, "PUT", `{"principalType":"User","additionalPermissions":["ReadAll"]}`,
		accessPV(ws, lake, viewer.ID)); w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	if !effective(t, st, lake, viewer.ID).Has(store.PermReadAll) {
		t.Fatal("grant not effective")
	}
	if w := do(a.deleteItemAccess, admin, "DELETE", "", accessPV(ws, lake, viewer.ID)); w.Code != http.StatusOK {
		t.Fatalf("DELETE = %d %s", w.Code, w.Body)
	}
	after := effective(t, st, lake, viewer.ID)
	if after.Has(store.PermReadAll) || !after.Has(store.PermRead) || !after.Has(store.PermReadData) {
		t.Fatalf("after revoke = %+v, want the Viewer role's access and no ReadAll", after)
	}
	// Revoking again has nothing to revoke — and says so.
	w := do(a.deleteItemAccess, admin, "DELETE", "", accessPV(ws, lake, viewer.ID))
	if w.Code != http.StatusNotFound || errorCode(t, w) != "ItemAccessNotFound" {
		t.Fatalf("second DELETE = %d %s", w.Code, w.Body)
	}
}

func TestTheNativeSurfaceOnAMissingItem(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	w := do(a.listItemAccess, admin, "GET", "", map[string]string{"wid": ws.ID, "iid": "no-such-item"})
	if w.Code != http.StatusNotFound || errorCode(t, w) != "ItemNotFound" {
		t.Fatalf("GET = %d %s", w.Code, w.Body)
	}
}

// ---- store failures ----------------------------------------------------------
//
// A store that cannot answer fails the request; it never reads as "no grants".
// Failures are injected precisely, so each reaches the branch it names rather
// than failing earlier on the caller's own lookup: a trigger refuses one kind of
// write, and a corrupt row belonging to SOMEBODY ELSE breaks a listing while the
// caller's own single-row lookup still resolves.

func execDisk(t *testing.T, dir string, stmts ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "fabric-emulator.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
}

const (
	refuseInserts = `CREATE TRIGGER refuse_insert BEFORE INSERT ON item_access BEGIN SELECT RAISE(ABORT, 'injected'); END`
	refuseDeletes = `CREATE TRIGGER refuse_delete BEFORE DELETE ON item_access BEGIN SELECT RAISE(ABORT, 'injected'); END`
	// corruptAfterInsert lets a write succeed and makes reading it back fail.
	corruptAfterInsert = `CREATE TRIGGER corrupt AFTER INSERT ON item_access BEGIN
		UPDATE item_access SET permissions = 'not json' WHERE principal_id = NEW.principal_id; END`
)

// corruptGrantFor stores a grant for someone and then makes it undecodable.
func corruptGrantFor(t *testing.T, st *store.Store, dir string, it *store.Item, principalID string) {
	t.Helper()
	if err := st.PutItemAccess(store.ItemAccess{ItemID: it.ID, PrincipalID: principalID, PrincipalType: "User",
		Permissions: []string{"Read"}}); err != nil {
		t.Fatal(err)
	}
	execDisk(t, dir, `UPDATE item_access SET permissions = 'not json' WHERE principal_id = '`+principalID+`'`)
}

// breakRoleListing leaves each principal's RoleOf lookup working while
// ListRoleAssignments fails: the table is rebuilt without NOT NULL and given a
// row whose principal_type cannot be scanned into a string.
func breakRoleListing(t *testing.T, dir, wsID string) {
	t.Helper()
	execDisk(t, dir,
		`CREATE TABLE ra_copy AS SELECT * FROM role_assignments`,
		`DROP TABLE role_assignments`,
		`CREATE TABLE role_assignments (id TEXT PRIMARY KEY, workspace_id TEXT, principal_id TEXT,
		   principal_type TEXT, role TEXT, UNIQUE (workspace_id, principal_id))`,
		`INSERT INTO role_assignments SELECT * FROM ra_copy`,
		`INSERT INTO role_assignments VALUES ('broken', '`+wsID+`', 'ghost', NULL, 'Viewer')`)
}

func TestTheNativeSurfaceReportsStoreFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(t *testing.T, st *store.Store, dir string, ws *store.Workspace, it *store.Item)
		call  func(a *API, ws *store.Workspace, it *store.Item) *httptest.ResponseRecorder
	}{
		"the item": {
			func(t *testing.T, _ *store.Store, dir string, _ *store.Workspace, _ *store.Item) {
				dropTable(t, dir, "items")
			},
			func(a *API, ws *store.Workspace, it *store.Item) *httptest.ResponseRecorder {
				return do(a.listItemAccess, admin, "GET", "", accessPV(ws, it, ""))
			}},
		"the caller's access": {
			func(t *testing.T, _ *store.Store, dir string, _ *store.Workspace, _ *store.Item) {
				dropTable(t, dir, "role_assignments")
			},
			func(a *API, ws *store.Workspace, it *store.Item) *httptest.ResponseRecorder {
				return do(a.listItemAccess, admin, "GET", "", accessPV(ws, it, ""))
			}},
		"the listing": {
			func(t *testing.T, st *store.Store, dir string, _ *store.Workspace, it *store.Item) {
				corruptGrantFor(t, st, dir, it, "someone-else")
			},
			func(a *API, ws *store.Workspace, it *store.Item) *httptest.ResponseRecorder {
				return do(a.listItemAccess, admin, "GET", "", accessPV(ws, it, ""))
			}},
		"the write": {
			func(t *testing.T, _ *store.Store, dir string, _ *store.Workspace, _ *store.Item) {
				execDisk(t, dir, refuseInserts)
			},
			func(a *API, ws *store.Workspace, it *store.Item) *httptest.ResponseRecorder {
				return do(a.putItemAccess, admin, "PUT", `{"principalType":"User"}`, accessPV(ws, it, stranger.ID))
			}},
		"reading the write back": {
			func(t *testing.T, _ *store.Store, dir string, _ *store.Workspace, _ *store.Item) {
				execDisk(t, dir, corruptAfterInsert)
			},
			func(a *API, ws *store.Workspace, it *store.Item) *httptest.ResponseRecorder {
				return do(a.putItemAccess, admin, "PUT", `{"principalType":"User"}`, accessPV(ws, it, stranger.ID))
			}},
		"the revoke": {
			func(t *testing.T, st *store.Store, dir string, _ *store.Workspace, it *store.Item) {
				if err := st.PutItemAccess(store.ItemAccess{ItemID: it.ID, PrincipalID: stranger.ID,
					PrincipalType: "User", Permissions: []string{"Read"}}); err != nil {
					t.Fatal(err)
				}
				execDisk(t, dir, refuseDeletes)
			},
			func(a *API, ws *store.Workspace, it *store.Item) *httptest.ResponseRecorder {
				return do(a.deleteItemAccess, admin, "DELETE", "", accessPV(ws, it, stranger.ID))
			}},
	} {
		t.Run(name, func(t *testing.T) {
			a, st, dir := newDiskAPI(t)
			ws := seedWorkspace(t, st)
			lake := typedIn(t, st, ws.ID, "Lakehouse")
			tc.setup(t, st, dir, ws, lake)
			if w := tc.call(a, ws, lake); w.Code != http.StatusInternalServerError {
				t.Fatalf("= %d %s, want 500", w.Code, w.Body)
			}
		})
	}
}

// ---- Power BI dataset users ------------------------------------------------------

func datasetPV(sm *store.Item) map[string]string { return map[string]string{"datasetId": sm.ID} }

func datasetUsers(t *testing.T, w *httptest.ResponseRecorder) map[string]datasetUser {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("GET users = %d %s", w.Code, w.Body)
	}
	var body struct{ Value []datasetUser }
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	out := map[string]datasetUser{}
	for _, u := range body.Value {
		out[u.Identifier] = u
	}
	return out
}

// Get Dataset Users lists inherited access beside direct grants, in the
// documented enum and principal types.
func TestDatasetUsersListInheritedAndDirectAccess(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	sm := typedIn(t, st, ws.ID, "SemanticModel")

	if w := do(a.postDatasetUser, admin, "POST",
		`{"identifier":"stranger-1","principalType":"User","datasetUserAccessRight":"ReadReshare"}`, datasetPV(sm)); w.Code != http.StatusOK {
		t.Fatalf("POST = %d %s", w.Code, w.Body)
	}
	users := datasetUsers(t, do(a.getDatasetUsers, admin, "GET", "", datasetPV(sm)))
	for id, want := range map[string]datasetUser{
		admin.ID:    {admin.ID, "App", "ReadWriteReshareExplore"}, // the owner, inherited
		viewer.ID:   {viewer.ID, "User", "Read"},                  // a Viewer, inherited
		stranger.ID: {stranger.ID, "User", "ReadReshare"},         // direct
	} {
		if users[id] != want {
			t.Errorf("%s = %+v, want %+v", id, users[id], want)
		}
	}
}

func TestDatasetUsersHonourTheGroupInThePath(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	sm := typedIn(t, st, ws.ID, "SemanticModel")
	right := map[string]string{"datasetId": sm.ID, "groupId": ws.ID}
	if w := do(a.getDatasetUsers, admin, "GET", "", right); w.Code != http.StatusOK {
		t.Fatalf("in its own group = %d %s", w.Code, w.Body)
	}
	wrong := map[string]string{"datasetId": sm.ID, "groupId": "another-workspace"}
	if w := do(a.getDatasetUsers, admin, "GET", "", wrong); w.Code != http.StatusNotFound {
		t.Fatalf("in another group = %d, want 404", w.Code)
	}
	// And something that is not a semantic model is not a dataset.
	lake := typedIn(t, st, ws.ID, "Lakehouse")
	if w := do(a.getDatasetUsers, admin, "GET", "", map[string]string{"datasetId": lake.ID}); w.Code != http.StatusNotFound {
		t.Fatalf("a lakehouse as a dataset = %d, want 404", w.Code)
	}
}

// Post needs ReadReshare, Get and Put need ReadWriteReshare — "folder admins,
// members and contributors with Reshare permissions".
func TestDatasetUserOperationsRequireTheirDocumentedPermissions(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	assignRole(t, st, ws.ID, contributor, store.RoleContributor)
	sm := typedIn(t, st, ws.ID, "SemanticModel")
	grant := `{"identifier":"stranger-1","principalType":"User","datasetUserAccessRight":"Read"}`

	if w := do(a.postDatasetUser, viewer, "POST", grant, datasetPV(sm)); w.Code != http.StatusForbidden {
		t.Errorf("a Viewer POSTing = %d, want 403", w.Code)
	}
	if w := do(a.getDatasetUsers, contributor, "GET", "", datasetPV(sm)); w.Code != http.StatusForbidden {
		t.Errorf("a Contributor without Reshare listing = %d, want 403", w.Code)
	}
	// A Contributor granted Reshare has ReadWriteReshare, and may.
	if w := do(a.postDatasetUser, admin, "POST",
		`{"identifier":"contrib-1","principalType":"User","datasetUserAccessRight":"ReadReshare"}`, datasetPV(sm)); w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	if w := do(a.getDatasetUsers, contributor, "GET", "", datasetPV(sm)); w.Code != http.StatusOK {
		t.Errorf("a Contributor with Reshare listing = %d %s", w.Code, w.Body)
	}
	if w := do(a.putDatasetUser, contributor, "PUT", grant, datasetPV(sm)); w.Code != http.StatusOK {
		t.Errorf("a Contributor with Reshare updating = %d %s", w.Code, w.Body)
	}
}

// Post grants ON TOP of an existing direct grant; Put replaces it; Put None
// removes it, and removing what is not there is not an error.
func TestPostAddsPutReplacesNoneRemoves(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	sm := typedIn(t, st, ws.ID, "SemanticModel")
	send := func(h handler, method, right string) {
		t.Helper()
		body := `{"identifier":"stranger-1","principalType":"User","datasetUserAccessRight":"` + right + `"}`
		if w := do(h, admin, method, body, datasetPV(sm)); w.Code != http.StatusOK {
			t.Fatalf("%s %s = %d %s", method, right, w.Code, w.Body)
		}
	}
	rightOf := func() string {
		return datasetUsers(t, do(a.getDatasetUsers, admin, "GET", "", datasetPV(sm)))[stranger.ID].DatasetUserAccessRight
	}

	send(a.postDatasetUser, "POST", "ReadReshare")
	send(a.postDatasetUser, "POST", "ReadExplore")
	if got := rightOf(); got != "ReadReshareExplore" {
		t.Fatalf("after two POSTs = %q, want the union", got)
	}
	send(a.putDatasetUser, "PUT", "Read")
	if got := rightOf(); got != "Read" {
		t.Fatalf("after PUT Read = %q, want exactly Read", got)
	}
	send(a.putDatasetUser, "PUT", "None")
	if got := rightOf(); got != "" {
		t.Fatalf("after PUT None = %q, want no access", got)
	}
	send(a.putDatasetUser, "PUT", "None") // nothing left to remove
}

func TestDatasetUserRefusals(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	sm := typedIn(t, st, ws.ID, "SemanticModel")
	for name, tc := range map[string]struct {
		h    handler
		body string
		code string
	}{
		"an App":           {a.postDatasetUser, `{"identifier":"app-1","principalType":"App","datasetUserAccessRight":"Read"}`, "InvalidRequest"},
		"org-wide access":  {a.postDatasetUser, `{"identifier":"x","principalType":"None","datasetUserAccessRight":"Read"}`, "InvalidRequest"},
		"a UPN":            {a.postDatasetUser, `{"identifier":"john@contoso.com","principalType":"User","datasetUserAccessRight":"Read"}`, "PrincipalNotResolvable"},
		"write on POST":    {a.postDatasetUser, `{"identifier":"x","principalType":"User","datasetUserAccessRight":"ReadWrite"}`, "InvalidRequest"},
		"write on PUT":     {a.putDatasetUser, `{"identifier":"x","principalType":"User","datasetUserAccessRight":"ReadWriteReshare"}`, "InvalidRequest"},
		"None on POST":     {a.postDatasetUser, `{"identifier":"x","principalType":"User","datasetUserAccessRight":"None"}`, "InvalidRequest"},
		"an invalid right": {a.postDatasetUser, `{"identifier":"x","principalType":"User","datasetUserAccessRight":"Owner"}`, "InvalidRequest"},
		"no identifier":    {a.postDatasetUser, `{"principalType":"User","datasetUserAccessRight":"Read"}`, "InvalidRequest"},
		"a malformed body": {a.postDatasetUser, `{`, "InvalidRequest"},
	} {
		w := do(tc.h, admin, "POST", tc.body, datasetPV(sm))
		if w.Code != http.StatusBadRequest || errorCode(t, w) != tc.code {
			t.Errorf("%s = %d %s, want 400 %s", name, w.Code, w.Body, tc.code)
		}
	}
}

// A grantor holding ReadReshare directly, with no role, cannot grant Explore.
func TestADatasetResharerGrantsOnlyWhatTheyHold(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	sm := typedIn(t, st, ws.ID, "SemanticModel")
	if w := do(a.postDatasetUser, admin, "POST",
		`{"identifier":"stranger-1","principalType":"User","datasetUserAccessRight":"ReadReshare"}`, datasetPV(sm)); w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	w := do(a.postDatasetUser, stranger, "POST",
		`{"identifier":"someone-2","principalType":"User","datasetUserAccessRight":"ReadExplore"}`, datasetPV(sm))
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "Explore") {
		t.Fatalf("resharing an unheld Explore = %d %s", w.Code, w.Body)
	}
	if w := do(a.postDatasetUser, stranger, "POST",
		`{"identifier":"someone-2","principalType":"Group","datasetUserAccessRight":"Read"}`, datasetPV(sm)); w.Code != http.StatusOK {
		t.Fatalf("resharing a held Read = %d %s", w.Code, w.Body)
	}
}

func TestDatasetRightRoundTrips(t *testing.T) {
	for _, right := range []string{"Read", "ReadWrite", "ReadReshare", "ReadWriteReshare", "ReadExplore",
		"ReadReshareExplore", "ReadWriteExplore", "ReadWriteReshareExplore"} {
		perms, ok := parseDatasetRight(right)
		if !ok {
			t.Errorf("%s did not parse", right)
			continue
		}
		if back := datasetRight(store.Access{Permissions: perms}); back != right {
			t.Errorf("%s -> %v -> %s", right, perms, back)
		}
	}
	// Out-of-order and unknown spellings are not the enum.
	for _, bad := range []string{"ReadExploreReshare", "Write", "ReadOwner", ""} {
		if _, ok := parseDatasetRight(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestDatasetUsersReportStoreFailures(t *testing.T) {
	grant := `{"identifier":"stranger-1","principalType":"User","datasetUserAccessRight":"Read"}`
	none := `{"identifier":"stranger-1","principalType":"User","datasetUserAccessRight":"None"}`
	for name, tc := range map[string]struct {
		setup  func(t *testing.T, st *store.Store, dir string, ws *store.Workspace, sm *store.Item)
		method string
		body   string
	}{
		"the caller's access": {func(t *testing.T, _ *store.Store, dir string, _ *store.Workspace, _ *store.Item) {
			dropTable(t, dir, "role_assignments")
		}, "GET", ""},
		"listing roles": {func(t *testing.T, _ *store.Store, dir string, ws *store.Workspace, _ *store.Item) {
			breakRoleListing(t, dir, ws.ID)
		}, "GET", ""},
		"listing grants": {func(t *testing.T, st *store.Store, dir string, _ *store.Workspace, sm *store.Item) {
			corruptGrantFor(t, st, dir, sm, "someone-else")
		}, "GET", ""},
		"reading the existing grant": {func(t *testing.T, st *store.Store, dir string, _ *store.Workspace, sm *store.Item) {
			corruptGrantFor(t, st, dir, sm, stranger.ID)
		}, "POST", grant},
		"the write": {func(t *testing.T, _ *store.Store, dir string, _ *store.Workspace, _ *store.Item) {
			execDisk(t, dir, refuseInserts)
		}, "PUT", grant},
		"the removal": {func(t *testing.T, st *store.Store, dir string, _ *store.Workspace, sm *store.Item) {
			if err := st.PutItemAccess(store.ItemAccess{ItemID: sm.ID, PrincipalID: stranger.ID,
				PrincipalType: "User", Permissions: []string{"Read"}}); err != nil {
				t.Fatal(err)
			}
			execDisk(t, dir, refuseDeletes)
		}, "PUT", none},
	} {
		t.Run(name, func(t *testing.T) {
			a, st, dir := newDiskAPI(t)
			ws := seedWorkspace(t, st)
			sm := typedIn(t, st, ws.ID, "SemanticModel")
			tc.setup(t, st, dir, ws, sm)
			h := map[string]handler{"GET": a.getDatasetUsers, "POST": a.postDatasetUser, "PUT": a.putDatasetUser}[tc.method]
			if w := do(h, admin, tc.method, tc.body, datasetPV(sm)); w.Code != http.StatusInternalServerError {
				t.Fatalf("= %d %s, want 500", w.Code, w.Body)
			}
		})
	}
}

// ---- admin list ------------------------------------------------------------------

func adminUsers(t *testing.T, a *API, ws *store.Workspace, it *store.Item, query string) (int, []adminItemAccess, string) {
	t.Helper()
	r := httptest.NewRequest("GET", "/x"+query, nil)
	r.SetPathValue("wid", ws.ID)
	r.SetPathValue("iid", it.ID)
	w := httptest.NewRecorder()
	a.adminItemUsers(w, r, admin)
	var body struct{ AccessDetails []adminItemAccess }
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w.Code, body.AccessDetails, w.Body.String()
}

func TestAdminItemUsersReportsEffectiveAccess(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lake := typedIn(t, st, ws.ID, "Lakehouse")
	if err := st.PutItemAccess(store.ItemAccess{ItemID: lake.ID, PrincipalID: stranger.ID, PrincipalType: "User",
		Permissions: []string{"Read"}, Additional: []string{"ReadAll"}}); err != nil {
		t.Fatal(err)
	}
	// A Viewer granted ReadAll directly shows the union of both halves.
	if err := st.PutItemAccess(store.ItemAccess{ItemID: lake.ID, PrincipalID: viewer.ID, PrincipalType: "User",
		Permissions: []string{"Read"}, Additional: []string{"ReadAll"}}); err != nil {
		t.Fatal(err)
	}
	code, details, raw := adminUsers(t, a, ws, lake, "")
	if code != http.StatusOK {
		t.Fatalf("= %d %s", code, raw)
	}
	byID := map[string]adminItemAccess{}
	for _, d := range details {
		byID[d.Principal.ID] = d
	}
	if d := byID[viewer.ID]; !reflect.DeepEqual(d.ItemAccessDetails.AdditionalPermissions, []string{"ReadAll", "ReadData"}) {
		t.Errorf("viewer = %+v, want inherited ReadData unioned with granted ReadAll", d)
	}
	if d := byID[stranger.ID]; d.Principal.Type != "User" || d.ItemAccessDetails.Type != "Lakehouse" ||
		!reflect.DeepEqual(d.ItemAccessDetails.Permissions, []string{"Read"}) {
		t.Errorf("stranger = %+v", d)
	}
	if d := byID[admin.ID]; d.Principal.Type != "ServicePrincipal" ||
		!reflect.DeepEqual(d.ItemAccessDetails.Permissions, []string{"Execute", "Read", "Reshare", "Write"}) {
		t.Errorf("owner = %+v", d)
	}
	if details[0].Principal.ID > details[len(details)-1].Principal.ID {
		t.Error("accessDetails are not ordered by principal")
	}
}

func TestAdminItemUsersTypeParameter(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	sm := typedIn(t, st, ws.ID, "SemanticModel")
	lake := typedIn(t, st, ws.ID, "Lakehouse")

	// "When querying for the following types, this parameter is required."
	if code, _, raw := adminUsers(t, a, ws, sm, ""); code != http.StatusBadRequest || !strings.Contains(raw, "InvalidItemType") {
		t.Errorf("a semantic model without type = %d %s", code, raw)
	}
	if code, details, raw := adminUsers(t, a, ws, sm, "?type=semanticmodel"); code != http.StatusOK || len(details) == 0 ||
		details[0].ItemAccessDetails.AdditionalPermissions == nil {
		t.Errorf("a semantic model with type = %d %s", code, raw)
	}
	if code, _, raw := adminUsers(t, a, ws, lake, "?type=Notebook"); code != http.StatusNotFound {
		t.Errorf("the wrong type = %d %s", code, raw)
	}
	if code, _, raw := adminUsers(t, a, ws, lake, "?type=Nonsense"); code != http.StatusBadRequest || !strings.Contains(raw, "InvalidItemType") {
		t.Errorf("an invalid type = %d %s", code, raw)
	}
	missing := &store.Item{ID: "no-such-item"}
	if code, _, _ := adminUsers(t, a, ws, missing, ""); code != http.StatusNotFound {
		t.Errorf("a missing item = %d", code)
	}
}

func TestAdminItemUsersReportsStoreFailures(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T, st *store.Store, dir string, ws *store.Workspace, it *store.Item){
		"the item": func(t *testing.T, _ *store.Store, dir string, _ *store.Workspace, _ *store.Item) {
			dropTable(t, dir, "items")
		},
		"listing roles": func(t *testing.T, _ *store.Store, dir string, _ *store.Workspace, _ *store.Item) {
			dropTable(t, dir, "role_assignments")
		},
		"listing grants": func(t *testing.T, st *store.Store, dir string, _ *store.Workspace, it *store.Item) {
			corruptGrantFor(t, st, dir, it, "someone-else")
		},
	} {
		t.Run(name, func(t *testing.T) {
			a, st, dir := newDiskAPI(t)
			ws := seedWorkspace(t, st)
			lake := typedIn(t, st, ws.ID, "Lakehouse")
			setup(t, st, dir, ws, lake)
			if code, _, raw := adminUsers(t, a, ws, lake, ""); code != http.StatusInternalServerError {
				t.Fatalf("= %d %s, want 500", code, raw)
			}
		})
	}
}

// ---- routing --------------------------------------------------------------------

// Every surface is reachable at its documented path, and the native one is NOT
// under the unauthenticated /_emulator/ prefix.
func TestItemAccessRoutesAreRegistered(t *testing.T) {
	a, _ := newAPI(t)
	mux := http.NewServeMux()
	a.registerItemAccess(mux)
	for _, route := range []struct{ method, path string }{
		{"GET", "/v1/workspaces/w/items/i/_emulator/access"},
		{"PUT", "/v1/workspaces/w/items/i/_emulator/access/p"},
		{"DELETE", "/v1/workspaces/w/items/i/_emulator/access/p"},
		{"GET", "/v1.0/myorg/datasets/d/users"},
		{"POST", "/v1.0/myorg/datasets/d/users"},
		{"PUT", "/v1.0/myorg/datasets/d/users"},
		{"GET", "/v1.0/myorg/groups/g/datasets/d/users"},
		{"POST", "/v1.0/myorg/groups/g/datasets/d/users"},
		{"PUT", "/v1.0/myorg/groups/g/datasets/d/users"},
		{"GET", "/v1/admin/workspaces/w/items/i/users"},
	} {
		if _, pattern := mux.Handler(httptest.NewRequest(route.method, route.path, nil)); pattern == "" {
			t.Errorf("%s %s is not routed", route.method, route.path)
		}
	}
}

// A revoke, and a dataset write, against something that is not there refuse
// before touching the store — the same resolution the reads use.
func TestWritesAgainstAMissingTargetAreNotFound(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	if w := do(a.deleteItemAccess, admin, "DELETE", "",
		map[string]string{"wid": ws.ID, "iid": "no-such-item", "principalId": stranger.ID}); w.Code != http.StatusNotFound {
		t.Errorf("revoke on a missing item = %d, want 404", w.Code)
	}
	if w := do(a.postDatasetUser, admin, "POST",
		`{"identifier":"stranger-1","principalType":"User","datasetUserAccessRight":"Read"}`,
		map[string]string{"datasetId": "no-such-dataset"}); w.Code != http.StatusNotFound {
		t.Errorf("grant on a missing dataset = %d, want 404", w.Code)
	}
}
