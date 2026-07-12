package api

// Direct handler tests: construct requests with SetPathValue and a fake
// principal, bypassing bearer validation (covered in auth and server tests)
// to reach the error branches cheaply.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/clock"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

func newAPI(t *testing.T) (*API, *store.Store) {
	t.Helper()
	st, err := store.Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, nil, 1, 0), st
}

var (
	admin  = &auth.Principal{ID: "admin-1", Type: "ServicePrincipal"}
	viewer = &auth.Principal{ID: "viewer-1", Type: "User"}
	nobody = &auth.Principal{ID: "nobody-1", Type: "User"}
)

// seedWorkspace creates a workspace owned by admin with viewer granted Viewer.
func seedWorkspace(t *testing.T, st *store.Store) *store.Workspace {
	t.Helper()
	ws := &store.Workspace{DisplayName: "w"}
	if err := st.CreateWorkspace(ws, store.Principal{ID: admin.ID, Type: admin.Type}); err != nil {
		t.Fatal(err)
	}
	err := st.CreateRoleAssignment(&store.RoleAssignment{
		WorkspaceID: ws.ID, Principal: store.Principal{ID: viewer.ID, Type: "User"}, Role: store.RoleViewer,
	})
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

// do invokes a handler with path values and returns the recorder.
func do(h handler, p *auth.Principal, method, body string, pathVals map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/x", strings.NewReader(body))
	for k, v := range pathVals {
		r.SetPathValue(k, v)
	}
	w := httptest.NewRecorder()
	h(w, r, p)
	return w
}

func errorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var e struct{ ErrorCode string }
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatalf("not a fabric error: %s", w.Body.Bytes())
	}
	return e.ErrorCode
}

func TestWorkspaceHandlerErrors(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	wid := map[string]string{"wid": ws.ID}

	// createWorkspace: malformed / missing displayName.
	if w := do(a.createWorkspace, admin, "POST", `{`, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("malformed create = %d", w.Code)
	}
	if w := do(a.createWorkspace, admin, "POST", `{"description":"x"}`, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("no displayName = %d", w.Code)
	}

	// Unknown workspace 404s before RBAC.
	if w := do(a.getWorkspace, admin, "GET", "", map[string]string{"wid": "nope"}); w.Code != http.StatusNotFound {
		t.Fatalf("unknown ws = %d", w.Code)
	}

	// No grant → 403 with InsufficientPrivileges.
	if w := do(a.getWorkspace, nobody, "GET", "", wid); w.Code != http.StatusForbidden || errorCode(t, w) != "InsufficientPrivileges" {
		t.Fatalf("ungranted read = %d %s", w.Code, w.Body.Bytes())
	}

	// updateWorkspace: viewer forbidden; admin malformed body; admin patch works.
	if w := do(a.updateWorkspace, viewer, "PATCH", `{"description":"x"}`, wid); w.Code != http.StatusForbidden {
		t.Fatalf("viewer patch = %d", w.Code)
	}
	if w := do(a.updateWorkspace, admin, "PATCH", `{`, wid); w.Code != http.StatusBadRequest {
		t.Fatalf("malformed patch = %d", w.Code)
	}
	if w := do(a.updateWorkspace, admin, "PATCH", `{"displayName":"renamed","description":"d"}`, wid); w.Code != http.StatusOK {
		t.Fatalf("patch = %d %s", w.Code, w.Body.Bytes())
	}
	got, _ := st.GetWorkspace(ws.ID)
	if got.DisplayName != "renamed" || got.Description != "d" {
		t.Fatalf("patch not applied: %+v", got)
	}

	// deleteWorkspace: viewer forbidden.
	if w := do(a.deleteWorkspace, viewer, "DELETE", "", wid); w.Code != http.StatusForbidden {
		t.Fatalf("viewer delete = %d", w.Code)
	}
}

func TestRoleAssignmentHandlerErrors(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	wid := map[string]string{"wid": ws.ID}

	// Viewer cannot list or grant.
	if w := do(a.listRoleAssignments, viewer, "GET", "", wid); w.Code != http.StatusForbidden {
		t.Fatalf("viewer list = %d", w.Code)
	}
	// Malformed grant bodies.
	for _, body := range []string{`{`, `{"role":"Viewer"}`, `{"principal":{"id":"x"},"role":"Owner"}`} {
		if w := do(a.createRoleAssignment, admin, "POST", body, wid); w.Code != http.StatusBadRequest {
			t.Fatalf("bad grant %q = %d", body, w.Code)
		}
	}
	// Duplicate grant → 409.
	if w := do(a.createRoleAssignment, admin, "POST",
		`{"principal":{"id":"viewer-1","type":"User"},"role":"Viewer"}`, wid); w.Code != http.StatusConflict {
		t.Fatalf("duplicate grant = %d", w.Code)
	}
	// Grant with defaulted principal type.
	if w := do(a.createRoleAssignment, admin, "POST",
		`{"principal":{"id":"typed"},"role":"Viewer"}`, wid); w.Code != http.StatusCreated {
		t.Fatalf("defaulted type grant = %d", w.Code)
	}

	// Patch: malformed, invalid role, unknown id.
	raid := map[string]string{"wid": ws.ID, "raid": "missing"}
	if w := do(a.updateRoleAssignment, admin, "PATCH", `{"role":"Owner"}`, raid); w.Code != http.StatusBadRequest {
		t.Fatalf("bad role patch = %d", w.Code)
	}
	if w := do(a.updateRoleAssignment, admin, "PATCH", `{"role":"Member"}`, raid); w.Code != http.StatusNotFound {
		t.Fatalf("patch missing ra = %d", w.Code)
	}
	if w := do(a.deleteRoleAssignment, admin, "DELETE", "", raid); w.Code != http.StatusNotFound {
		t.Fatalf("delete missing ra = %d", w.Code)
	}
	// Viewer cannot patch/delete assignments.
	if w := do(a.updateRoleAssignment, viewer, "PATCH", `{"role":"Member"}`, raid); w.Code != http.StatusForbidden {
		t.Fatalf("viewer patch ra = %d", w.Code)
	}
}

func TestItemHandlerErrors(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	wid := map[string]string{"wid": ws.ID}

	// Malformed / incomplete create bodies.
	for _, body := range []string{`{`, `{"displayName":"x"}`, `{"type":"Notebook"}`, `{"displayName":" ","type":"Notebook"}`} {
		if w := do(a.createItem, admin, "POST", body, wid); w.Code != http.StatusBadRequest {
			t.Fatalf("bad item create %q = %d", body, w.Code)
		}
	}
	// Viewer cannot create.
	if w := do(a.createItem, viewer, "POST", `{"displayName":"x","type":"Notebook"}`, wid); w.Code != http.StatusForbidden {
		t.Fatalf("viewer create = %d", w.Code)
	}
	// Sync create, then get/update/delete happy paths + not-found branches.
	w := do(a.createItem, admin, "POST", `{"displayName":"nb","type":"Notebook"}`, wid)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d", w.Code)
	}
	var it struct{ ID string }
	_ = json.Unmarshal(w.Body.Bytes(), &it)
	iid := map[string]string{"wid": ws.ID, "iid": it.ID}
	missing := map[string]string{"wid": ws.ID, "iid": "missing"}

	if w := do(a.getItem, viewer, "GET", "", iid); w.Code != http.StatusOK {
		t.Fatalf("viewer get item = %d", w.Code)
	}
	if w := do(a.getItem, admin, "GET", "", missing); w.Code != http.StatusNotFound {
		t.Fatalf("get missing = %d", w.Code)
	}
	if w := do(a.updateItem, admin, "PATCH", `{`, iid); w.Code != http.StatusBadRequest {
		t.Fatalf("malformed item patch = %d", w.Code)
	}
	if w := do(a.updateItem, admin, "PATCH", `{"displayName":"nb2","description":"d"}`, iid); w.Code != http.StatusOK {
		t.Fatalf("item patch = %d", w.Code)
	}
	if w := do(a.updateItem, admin, "PATCH", `{"displayName":"x"}`, missing); w.Code != http.StatusNotFound {
		t.Fatalf("patch missing = %d", w.Code)
	}
	if w := do(a.updateItem, viewer, "PATCH", `{"displayName":"x"}`, iid); w.Code != http.StatusForbidden {
		t.Fatalf("viewer patch = %d", w.Code)
	}
	if w := do(a.deleteItem, admin, "DELETE", "", missing); w.Code != http.StatusNotFound {
		t.Fatalf("delete missing = %d", w.Code)
	}
	if w := do(a.deleteItem, admin, "DELETE", "", iid); w.Code != http.StatusOK {
		t.Fatalf("delete = %d", w.Code)
	}
	// List with type filter on the viewer path.
	r := httptest.NewRequest("GET", "/x?type=Notebook", nil)
	r.SetPathValue("wid", ws.ID)
	rec := httptest.NewRecorder()
	a.listItems(rec, r, viewer)
	if rec.Code != http.StatusOK {
		t.Fatalf("filtered list = %d", rec.Code)
	}
}

func TestOperationHandlerBranches(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)

	if w := do(a.getOperation, admin, "GET", "", map[string]string{"oid": "missing"}); w.Code != http.StatusNotFound {
		t.Fatalf("missing op = %d", w.Code)
	}
	if w := do(a.getOperationResult, admin, "GET", "", map[string]string{"oid": "missing"}); w.Code != http.StatusNotFound {
		t.Fatalf("missing op result = %d", w.Code)
	}

	// Result of an unknown kind: succeeds with empty body.
	op := &store.Operation{Kind: "Mystery"}
	if err := st.CreateOperation(op); err != nil {
		t.Fatal(err)
	}
	if w := do(a.getOperationResult, admin, "GET", "", map[string]string{"oid": op.ID}); w.Code != http.StatusOK {
		t.Fatalf("unknown kind result = %d", w.Code)
	}

	// CreateItem op whose item was deleted → result 404.
	gone := &store.Operation{Kind: "CreateItem", ResultRef: "deleted-item"}
	if err := st.CreateOperation(gone); err != nil {
		t.Fatal(err)
	}
	if w := do(a.getOperationResult, admin, "GET", "", map[string]string{"oid": gone.ID}); w.Code != http.StatusNotFound {
		t.Fatalf("gone result = %d", w.Code)
	}

	// Succeeded op with a Location header pointing at the result.
	it := &store.Item{WorkspaceID: ws.ID, Type: "Notebook", DisplayName: "n"}
	if err := st.CreateItem(it, nil); err != nil {
		t.Fatal(err)
	}
	ok := &store.Operation{Kind: "CreateItem", ResultRef: it.ID}
	if err := st.CreateOperation(ok); err != nil {
		t.Fatal(err)
	}
	st.Clock.Advance(1)
	w := do(a.getOperation, admin, "GET", "", map[string]string{"oid": ok.ID})
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Location"), ok.ID+"/result") {
		t.Fatalf("succeeded op: %d loc=%q", w.Code, w.Header().Get("Location"))
	}
}

func TestFaultRejectNextRequests(t *testing.T) {
	a, st := newAPI(t)
	_ = st
	a.SetFaults(-1, 2, -1)
	h := a.withAuth(func(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
		w.WriteHeader(http.StatusOK)
	})
	for i := range 2 {
		w := httptest.NewRecorder()
		h(w, httptest.NewRequest("GET", "/x", nil))
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("request %d during fault = %d; want 500", i, w.Code)
		}
	}
	// Faults exhausted → falls through to auth (401 with nil validator would
	// panic, so assert the fault window closed by checking state directly).
	a.mu.Lock()
	left := a.rejectAll
	a.mu.Unlock()
	if left != 0 {
		t.Fatalf("rejectAll = %d after two requests; want 0", left)
	}
}
