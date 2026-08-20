package api

// Storage-failure tests: the store lives on disk, so a second SQLite
// connection can drop tables out from under the handlers — reaching the
// 500 branches a healthy store never exercises.

import (
	"database/sql"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/clock"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

func newDiskAPI(t *testing.T) (*API, *store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir, clock.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return New(st, nil, 1, 0), st, dir
}

func dropTable(t *testing.T, dir, table string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "fabric-emulator.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("DROP TABLE " + table); err != nil {
		t.Fatal(err)
	}
}

func TestStorageFailure500s(t *testing.T) {
	a, st, dir := newDiskAPI(t)
	ws := seedWorkspace(t, st)
	wid := map[string]string{"wid": ws.ID}
	async := `{"displayName":"nb","type":"Notebook","definition":{"parts":[{"path":".platform","payload":"e30=","payloadType":"InlineBase64"}]}}`

	// operations table gone → startOperation 500 (RBAC + item insert still work).
	dropTable(t, dir, "operations")
	if w := do(a.createItem, admin, "POST", async, wid); w.Code != http.StatusInternalServerError {
		t.Fatalf("startOperation with no operations table = %d; want 500", w.Code)
	}

	// items table gone → list/create/delete item 500s.
	dropTable(t, dir, "items")
	if w := do(a.listItems, admin, "GET", "", wid); w.Code != http.StatusInternalServerError {
		t.Fatalf("listItems = %d; want 500", w.Code)
	}
	if w := do(a.createItem, admin, "POST", `{"displayName":"x","type":"Notebook"}`, wid); w.Code != http.StatusInternalServerError {
		t.Fatalf("createItem = %d; want 500", w.Code)
	}
	if w := do(a.deleteItem, admin, "DELETE", "", map[string]string{"wid": ws.ID, "iid": "any"}); w.Code != http.StatusInternalServerError {
		t.Fatalf("deleteItem = %d; want 500", w.Code)
	}

	// role_assignments gone → requireRole's RoleOf errors → 500.
	dropTable(t, dir, "role_assignments")
	if w := do(a.getWorkspace, admin, "GET", "", wid); w.Code != http.StatusInternalServerError {
		t.Fatalf("requireRole with no role table = %d; want 500", w.Code)
	}

	// workspaces gone → list/create workspace 500s.
	dropTable(t, dir, "workspaces")
	if w := do(a.listWorkspaces, admin, "GET", "", nil); w.Code != http.StatusInternalServerError {
		t.Fatalf("listWorkspaces = %d; want 500", w.Code)
	}
	if w := do(a.createWorkspace, admin, "POST", `{"displayName":"w"}`, nil); w.Code != http.StatusInternalServerError {
		t.Fatalf("createWorkspace = %d; want 500", w.Code)
	}
}

// exec runs arbitrary SQL against the on-disk database.
func exec(t *testing.T, dir, stmt string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "fabric-emulator.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(stmt); err != nil {
		t.Fatal(err)
	}
}

func TestStorageFailureMutations(t *testing.T) {
	a, st, dir := newDiskAPI(t)
	ws := seedWorkspace(t, st)
	wid := map[string]string{"wid": ws.ID}

	it := &store.Item{WorkspaceID: ws.ID, Type: "Notebook", DisplayName: "n"}
	if err := st.CreateItem(it, nil); err != nil {
		t.Fatal(err)
	}
	ras, err := st.ListRoleAssignments(ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	raid := map[string]string{"wid": ws.ID, "raid": ras[0].ID}

	// Triggers block writes while reads (RBAC lookups) stay healthy — the
	// only way to reach the post-RBAC 500 branches.
	for _, trg := range []string{
		`CREATE TRIGGER no_ws_del BEFORE DELETE ON workspaces BEGIN SELECT RAISE(ABORT, 'boom'); END`,
		`CREATE TRIGGER no_ws_upd BEFORE UPDATE ON workspaces BEGIN SELECT RAISE(ABORT, 'boom'); END`,
		`CREATE TRIGGER no_it_upd BEFORE UPDATE ON items BEGIN SELECT RAISE(ABORT, 'boom'); END`,
		`CREATE TRIGGER no_ra_upd BEFORE UPDATE ON role_assignments BEGIN SELECT RAISE(ABORT, 'boom'); END`,
		`CREATE TRIGGER no_ra_del BEFORE DELETE ON role_assignments BEGIN SELECT RAISE(ABORT, 'boom'); END`,
	} {
		exec(t, dir, trg)
	}

	if w := do(a.deleteWorkspace, admin, "DELETE", "", wid); w.Code != http.StatusInternalServerError {
		t.Fatalf("deleteWorkspace blocked = %d; want 500", w.Code)
	}
	if w := do(a.updateWorkspace, admin, "PATCH", `{"description":"x"}`, wid); w.Code != http.StatusInternalServerError {
		t.Fatalf("updateWorkspace blocked = %d; want 500", w.Code)
	}
	iid := map[string]string{"wid": ws.ID, "iid": it.ID}
	if w := do(a.updateItem, admin, "PATCH", `{"description":"x"}`, iid); w.Code != http.StatusInternalServerError {
		t.Fatalf("updateItem blocked = %d; want 500", w.Code)
	}
	if w := do(a.updateRoleAssignment, admin, "PATCH", `{"role":"Member"}`, raid); w.Code != http.StatusInternalServerError {
		t.Fatalf("updateRoleAssignment blocked = %d; want 500", w.Code)
	}
	if w := do(a.deleteRoleAssignment, admin, "DELETE", "", raid); w.Code != http.StatusInternalServerError {
		t.Fatalf("deleteRoleAssignment blocked = %d; want 500", w.Code)
	}
}

func TestListItemsEmptyWorkspace(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	w := do(a.listItems, viewer, "GET", "", map[string]string{"wid": ws.ID})
	if w.Code != http.StatusOK || w.Body.String() != `{"value":[]}`+"\n" {
		t.Fatalf("empty list = %d %q; want value:[]", w.Code, w.Body.String())
	}
}

// The admin surfaces added for governance have the same failure contract as
// the rest of the control plane: a storage error is a 500, not a partial or
// empty success. These paths are otherwise unreachable — nothing a client
// sends can make the store fail — so they are exercised by removing the table
// underneath them.
func TestAdminSurfaceStorageFailures(t *testing.T) {
	a, st, dir := newDiskAPI(t)
	ws := seedWorkspace(t, st)

	// A domain to address, created before its table is removed.
	d := &store.Domain{DisplayName: "Fin"}
	if err := st.CreateDomain(d); err != nil {
		t.Fatal(err)
	}
	did := map[string]string{"did": d.ID}

	dropTable(t, dir, "sensitivity_labels")
	if w := do(a.listLabels, admin, "GET", "", nil); w.Code != http.StatusInternalServerError {
		t.Fatalf("listLabels = %d; want 500", w.Code)
	}

	dropTable(t, dir, "activity_events")
	day := time.Now().UTC().Format("2006-01-02")
	w := doQuery(a.listActivityEvents, "GET",
		"startDateTime='"+day+"T00:00:00Z'&endDateTime='"+day+"T23:59:59Z'", nil)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("listActivityEvents = %d; want 500", w.Code)
	}

	dropTable(t, dir, "tenant_settings")
	if w := do(a.listTenantSettings, admin, "GET", "", nil); w.Code != http.StatusInternalServerError {
		t.Fatalf("listTenantSettings = %d; want 500", w.Code)
	}

	// Domain reads, writes and role assignment all fail loudly.
	dropTable(t, dir, "domain_role_assignments")
	if w := do(a.listDomainRoles, admin, "GET", "", did); w.Code != http.StatusInternalServerError {
		t.Fatalf("listDomainRoles = %d; want 500", w.Code)
	}
	if w := do(a.bulkAssignDomainRole, admin, "POST",
		`{"type":"Admins","principals":[{"id":"11111111-1111-4111-8111-111111111111","type":"User"}]}`,
		did); w.Code != http.StatusInternalServerError {
		t.Fatalf("bulkAssignDomainRole = %d; want 500", w.Code)
	}
	if w := do(a.bulkUnassignDomainRole, admin, "POST",
		`{"type":"Admins","principals":[{"id":"11111111-1111-4111-8111-111111111111","type":"User"}]}`,
		did); w.Code != http.StatusInternalServerError {
		t.Fatalf("bulkUnassignDomainRole = %d; want 500", w.Code)
	}

	dropTable(t, dir, "domain_workspaces")
	if w := do(a.listDomainWorkspaces, admin, "GET", "", did); w.Code != http.StatusInternalServerError {
		t.Fatalf("listDomainWorkspaces = %d; want 500", w.Code)
	}
	if w := do(a.assignDomainWorkspaces, admin, "POST",
		`{"workspacesIds":["`+ws.ID+`"]}`, did); w.Code != http.StatusInternalServerError {
		t.Fatalf("assignDomainWorkspaces = %d; want 500", w.Code)
	}

	dropTable(t, dir, "domains")
	if w := do(a.listDomains, admin, "GET", "", nil); w.Code != http.StatusInternalServerError {
		t.Fatalf("listDomains = %d; want 500", w.Code)
	}
	if w := do(a.updateDomain, admin, "PATCH", `{"description":"x"}`, did); w.Code != http.StatusInternalServerError {
		t.Fatalf("updateDomain = %d; want 500", w.Code)
	}
	if w := do(a.createDomain, admin, "POST", `{"displayName":"New"}`, nil); w.Code != http.StatusInternalServerError {
		t.Fatalf("createDomain = %d; want 500", w.Code)
	}
}
