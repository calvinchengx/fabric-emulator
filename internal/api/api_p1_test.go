package api

// P1 error-branch tests: definitions, git, connections, jobs — driven
// directly with fake principals; storage failures injected by dropping
// tables under the live handlers.

import (
	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// doQuery is do() with a query string (admin principal).
func doQuery(h handler, method, rawQuery string, pathVals map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/x?"+rawQuery, nil)
	for k, v := range pathVals {
		r.SetPathValue(k, v)
	}
	w := httptest.NewRecorder()
	h(w, r, admin)
	return w
}

// userAdmin is a human Admin — git connect with Automatic credentials is
// User-only (service principals must use a ConfiguredConnection).
var userAdmin = &auth.Principal{ID: "user-admin-1", Type: "User"}

func connectGit(t *testing.T, a *API, wid string) {
	t.Helper()
	_ = a.Store.CreateRoleAssignment(&store.RoleAssignment{
		WorkspaceID: wid, Principal: store.Principal{ID: userAdmin.ID, Type: "User"}, Role: store.RoleAdmin,
	})
	body := `{"gitProviderDetails":{"gitProviderType":"GitHub","ownerName":"o","repositoryName":"r","branchName":"main","directoryName":"/"}}`
	if w := do(a.gitConnect, userAdmin, "POST", body, map[string]string{"wid": wid}); w.Code != http.StatusOK {
		t.Fatalf("connect = %d %s", w.Code, w.Body.Bytes())
	}
}

func TestDefinitionHandlerErrors(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	missing := map[string]string{"wid": ws.ID, "iid": "missing"}

	if w := do(a.getDefinition, admin, "POST", "", missing); w.Code != http.StatusNotFound {
		t.Fatalf("getDefinition missing item = %d", w.Code)
	}
	if w := do(a.getDefinition, viewer, "POST", "", missing); w.Code != http.StatusForbidden {
		t.Fatalf("getDefinition as viewer = %d (definitions expose source)", w.Code)
	}
	if w := do(a.updateDefinition, admin, "POST", "{}", missing); w.Code != http.StatusNotFound {
		t.Fatalf("updateDefinition missing item = %d", w.Code)
	}

	// Item without a definition reads back empty parts.
	it := &store.Item{WorkspaceID: ws.ID, Type: "Notebook", DisplayName: "bare"}
	if err := st.CreateItem(it, nil); err != nil {
		t.Fatal(err)
	}
	iid := map[string]string{"wid": ws.ID, "iid": it.ID}
	if w := do(a.getDefinition, admin, "POST", "", iid); w.Code != http.StatusOK || w.Body.String() != `{"definition":{"parts":[]}}`+"\n" {
		t.Fatalf("bare definition = %d %q", w.Code, w.Body.String())
	}
	// Malformed / empty updateDefinition bodies.
	for _, body := range []string{`{`, `{}`, `{"definition":{"parts":[]}}`} {
		if w := do(a.updateDefinition, admin, "POST", body, iid); w.Code != http.StatusBadRequest {
			t.Fatalf("updateDefinition %q = %d", body, w.Code)
		}
	}
}

func TestTypedCreateMalformed(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	h := a.typedCreate("Notebook")
	if w := do(h, admin, "POST", `{nope`, map[string]string{"wid": ws.ID}); w.Code != http.StatusBadRequest {
		t.Fatalf("typed create malformed = %d", w.Code)
	}
}

func TestGitHandlerErrors(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	wid := map[string]string{"wid": ws.ID}

	// Malformed / incomplete connect bodies.
	for _, body := range []string{`{`, `{}`, `{"gitProviderDetails":{"repositoryName":"r"}}`} {
		if w := do(a.gitConnect, admin, "POST", body, wid); w.Code != http.StatusBadRequest {
			t.Fatalf("connect %q = %d", body, w.Code)
		}
	}
	// Not connected → every git route 400s.
	for name, h := range map[string]handler{
		"initialize": a.gitInitializeConnection, "status": a.gitStatus,
		"commit": a.gitCommitToGit, "update": a.gitUpdateFromGit,
		"disconnect": a.gitDisconnect, "myCreds": a.gitMyCredentials,
	} {
		if w := do(h, admin, "POST", "{}", wid); w.Code != http.StatusBadRequest {
			t.Fatalf("%s while unconnected = %d", name, w.Code)
		}
	}
	// Automatic credentials echo without a connectionId.
	connectGit(t, a, ws.ID)
	if w := do(a.gitMyCredentials, admin, "GET", "", wid); w.Code != http.StatusOK ||
		w.Body.String() != `{"source":"Automatic"}`+"\n" {
		t.Fatalf("myCreds Automatic = %d %q", w.Code, w.Body.String())
	}
}

func TestGitUpdateFromGitMirrors(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	wid := map[string]string{"wid": ws.ID}
	connectGit(t, a, ws.ID)

	// Remote has nb(v2); workspace has nb(v1) + stale item.
	partsV2 := []store.DefinitionPart{{Path: "f", Payload: "djI=", PayloadType: "InlineBase64"}}
	if _, err := st.CommitRemoteItems("GitHub|o||r|/", "main", []*store.RemoteItem{
		{Type: "Notebook", DisplayName: "nb", Parts: partsV2},
	}); err != nil {
		t.Fatal(err)
	}
	nb := &store.Item{WorkspaceID: ws.ID, Type: "Notebook", DisplayName: "nb"}
	if err := st.CreateItem(nb, []store.DefinitionPart{{Path: "f", Payload: "djE=", PayloadType: "InlineBase64"}}); err != nil {
		t.Fatal(err)
	}
	stale := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "stale"}
	if err := st.CreateItem(stale, nil); err != nil {
		t.Fatal(err)
	}

	if w := do(a.gitUpdateFromGit, admin, "POST", "{}", wid); w.Code != http.StatusAccepted {
		t.Fatalf("updateFromGit = %d %s", w.Code, w.Body.Bytes())
	}
	// nb definition replaced with v2; stale item deleted.
	parts, _ := st.GetDefinition(nb.ID)
	if len(parts) != 1 || parts[0].Payload != "djI=" {
		t.Fatalf("definition not mirrored: %+v", parts)
	}
	if _, err := st.GetItem(ws.ID, stale.ID); err == nil {
		t.Fatal("stale item survived updateFromGit mirror")
	}
}

func TestPartsEqual(t *testing.T) {
	x := store.DefinitionPart{Path: "a", Payload: "1", PayloadType: "InlineBase64"}
	y := store.DefinitionPart{Path: "a", Payload: "2", PayloadType: "InlineBase64"}
	if !partsEqual([]store.DefinitionPart{x}, []store.DefinitionPart{x}) {
		t.Fatal("identical parts unequal")
	}
	if partsEqual([]store.DefinitionPart{x}, []store.DefinitionPart{x, y}) {
		t.Fatal("different lengths equal")
	}
	if partsEqual([]store.DefinitionPart{x}, []store.DefinitionPart{y}) {
		t.Fatal("different payloads equal")
	}
}

func TestConnectionHandlerErrors(t *testing.T) {
	a, _ := newAPI(t)
	for _, body := range []string{`{`, `{}`} {
		if w := do(a.createConnection, admin, "POST", body, nil); w.Code != http.StatusBadRequest {
			t.Fatalf("createConnection %q = %d", body, w.Code)
		}
	}
	// Empty list is value:[].
	if w := do(a.listConnections, admin, "GET", "", nil); w.Code != http.StatusOK || w.Body.String() != `{"value":[]}`+"\n" {
		t.Fatalf("empty connections = %d %q", w.Code, w.Body.String())
	}
}

func TestJobHandlerErrors(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	missing := map[string]string{"wid": ws.ID, "iid": "missing"}

	if w := do(a.createJobInstance, admin, "POST", "", missing); w.Code != http.StatusNotFound {
		t.Fatalf("job on missing item = %d", w.Code)
	}
	it := &store.Item{WorkspaceID: ws.ID, Type: "Notebook", DisplayName: "nb"}
	if err := st.CreateItem(it, nil); err != nil {
		t.Fatal(err)
	}
	pv := map[string]string{"wid": ws.ID, "iid": it.ID, "jid": "missing"}
	if w := do(a.getJobInstance, admin, "GET", "", pv); w.Code != http.StatusNotFound {
		t.Fatalf("get missing job = %d", w.Code)
	}
	if w := do(a.cancelJobInstance, admin, "POST", "", pv); w.Code != http.StatusNotFound {
		t.Fatalf("cancel missing job = %d", w.Code)
	}
	if w := do(a.cancelJobInstance, viewer, "POST", "", pv); w.Code != http.StatusForbidden {
		t.Fatalf("viewer cancel = %d", w.Code)
	}
}

func TestP1StorageFailure500s(t *testing.T) {
	a, st, dir := newDiskAPI(t)
	ws := seedWorkspace(t, st)
	wid := map[string]string{"wid": ws.ID}
	connectGit(t, a, ws.ID)
	it := &store.Item{WorkspaceID: ws.ID, Type: "Notebook", DisplayName: "nb"}
	if err := st.CreateItem(it, nil); err != nil {
		t.Fatal(err)
	}
	iid := map[string]string{"wid": ws.ID, "iid": it.ID}

	// job_instances gone → schedule 500.
	dropTable(t, dir, "job_instances")
	q := map[string]string{"wid": ws.ID, "iid": it.ID}
	w := doQuery(a.createJobInstance, "POST", "jobType=RunNotebook", q)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("createJob with no table = %d", w.Code)
	}

	// item_definitions gone → getDefinition/updateDefinition-side 500s.
	dropTable(t, dir, "item_definitions")
	if w := do(a.getDefinition, admin, "POST", "", iid); w.Code != http.StatusInternalServerError {
		t.Fatalf("getDefinition = %d", w.Code)
	}
	if w := do(a.updateDefinition, admin, "POST",
		`{"definition":{"parts":[{"path":"p","payload":"e30=","payloadType":"InlineBase64"}]}}`, iid); w.Code != http.StatusInternalServerError {
		t.Fatalf("updateDefinition = %d", w.Code)
	}
	// gitStatus needs definitions for the modified diff → 500.
	if _, err := st.CommitRemoteItems("GitHub|o||r|/", "main", []*store.RemoteItem{
		{Type: "Notebook", DisplayName: "nb"},
	}); err != nil {
		t.Fatal(err)
	}
	if w := do(a.gitStatus, admin, "GET", "", wid); w.Code != http.StatusInternalServerError {
		t.Fatalf("gitStatus = %d", w.Code)
	}

	// git remote tables gone → initialize/commit/update 500s.
	dropTable(t, dir, "git_remote_heads")
	if w := do(a.gitInitializeConnection, admin, "POST", "{}", wid); w.Code != http.StatusInternalServerError {
		t.Fatalf("initialize = %d", w.Code)
	}
	dropTable(t, dir, "git_remote_items")
	if w := do(a.gitCommitToGit, admin, "POST", "{}", wid); w.Code != http.StatusInternalServerError {
		t.Fatalf("commit = %d", w.Code)
	}
	if w := do(a.gitUpdateFromGit, admin, "POST", "{}", wid); w.Code != http.StatusInternalServerError {
		t.Fatalf("update = %d", w.Code)
	}

	// connections table gone → list/create 500s; connect with configured
	// connection can't resolve → 400 path already covered, use list here.
	dropTable(t, dir, "connections")
	if w := do(a.listConnections, admin, "GET", "", nil); w.Code != http.StatusInternalServerError {
		t.Fatalf("listConnections = %d", w.Code)
	}
	if w := do(a.createConnection, admin, "POST", `{"displayName":"c"}`, nil); w.Code != http.StatusInternalServerError {
		t.Fatalf("createConnection = %d", w.Code)
	}

	// git_connections gone → connect 500 (write fails after validation;
	// userAdmin because SPs cannot use Automatic credentials).
	dropTable(t, dir, "git_connections")
	body := `{"gitProviderDetails":{"gitProviderType":"GitHub","ownerName":"o","repositoryName":"r","branchName":"main","directoryName":"/"}}`
	if w := do(a.gitConnect, userAdmin, "POST", body, wid); w.Code != http.StatusInternalServerError {
		t.Fatalf("connect = %d", w.Code)
	}
}
