package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// Items carry the folder they were created in, and report it back. fabric-cicd
// compares the deployed item's folderId against the repository's to decide
// whether a move is needed, so an item that always reported the root would be
// moved on every single publish.
func TestCreateItemInFolder(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	f := &store.Folder{WorkspaceID: ws.ID, DisplayName: "landing"}
	if err := st.CreateFolder(f); err != nil {
		t.Fatal(err)
	}

	w := do(a.createItem, admin, "POST",
		`{"displayName":"nb","type":"Notebook","folderId":"`+f.ID+`"}`,
		map[string]string{"wid": ws.ID})
	if w.Code != 201 {
		t.Fatalf("create = %d %s", w.Code, w.Body.String())
	}
	var created store.Item
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.FolderID != f.ID {
		t.Errorf("created folderId = %q, want %q", created.FolderID, f.ID)
	}

	// And it survives a round trip through the store, not just the response.
	got, err := st.GetItem(ws.ID, created.ID)
	if err != nil || got == nil {
		t.Fatalf("get: %v", err)
	}
	if got.FolderID != f.ID {
		t.Errorf("persisted folderId = %q, want %q", got.FolderID, f.ID)
	}

	// A root item has no folderId at all, matching Fabric (omitempty).
	w = do(a.createItem, admin, "POST", `{"displayName":"root-nb","type":"Notebook"}`,
		map[string]string{"wid": ws.ID})
	if w.Code != 201 {
		t.Fatalf("create at root = %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "folderId") {
		t.Errorf("root item should omit folderId, got %s", w.Body.String())
	}
}

// POST /items/{id}/move is what fabric-cicd calls when a redeploy finds an item
// in a different folder than the repository says. Without it, republishing any
// repository that nests items in folders fails on every nested item.
func TestMoveItemBetweenFolders(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	src := &store.Folder{WorkspaceID: ws.ID, DisplayName: "bronze"}
	dst := &store.Folder{WorkspaceID: ws.ID, DisplayName: "silver"}
	for _, f := range []*store.Folder{src, dst} {
		if err := st.CreateFolder(f); err != nil {
			t.Fatal(err)
		}
	}
	it := &store.Item{WorkspaceID: ws.ID, Type: "Notebook", DisplayName: "nb", FolderID: src.ID}
	if err := st.CreateItem(it, nil); err != nil {
		t.Fatal(err)
	}
	vals := map[string]string{"wid": ws.ID, "iid": it.ID}

	w := do(a.moveItem, admin, "POST", `{"targetFolderId":"`+dst.ID+`"}`, vals)
	if w.Code != 200 {
		t.Fatalf("move = %d %s", w.Code, w.Body.String())
	}
	got, _ := st.GetItem(ws.ID, it.ID)
	if got.FolderID != dst.ID {
		t.Errorf("after move folderId = %q, want %q", got.FolderID, dst.ID)
	}

	// Empty target means the workspace root.
	if w = do(a.moveItem, admin, "POST", `{"targetFolderId":""}`, vals); w.Code != 200 {
		t.Fatalf("move to root = %d %s", w.Code, w.Body.String())
	}
	if got, _ = st.GetItem(ws.ID, it.ID); got.FolderID != "" {
		t.Errorf("after move to root folderId = %q, want empty", got.FolderID)
	}
}

func TestMoveItemErrors(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	it := &store.Item{WorkspaceID: ws.ID, Type: "Notebook", DisplayName: "nb"}
	if err := st.CreateItem(it, nil); err != nil {
		t.Fatal(err)
	}

	// A move into a folder that does not exist must fail: silently accepting it
	// would let a broken deploy report success.
	w := do(a.moveItem, admin, "POST", `{"targetFolderId":"00000000-0000-4000-8000-000000000000"}`,
		map[string]string{"wid": ws.ID, "iid": it.ID})
	if w.Code != 404 || errorCode(t, w) != "FolderNotFound" {
		t.Errorf("move to unknown folder = %d %s", w.Code, w.Body.String())
	}
	// The item must not have moved.
	if got, _ := st.GetItem(ws.ID, it.ID); got.FolderID != "" {
		t.Errorf("item moved despite the error: %q", got.FolderID)
	}

	if w = do(a.moveItem, admin, "POST", `{"targetFolderId":""}`,
		map[string]string{"wid": ws.ID, "iid": "no-such-item"}); w.Code != 404 {
		t.Errorf("move of unknown item = %d, want 404", w.Code)
	}
	if w = do(a.moveItem, admin, "POST", `{bad json`,
		map[string]string{"wid": ws.ID, "iid": it.ID}); w.Code != 400 {
		t.Errorf("malformed body = %d, want 400", w.Code)
	}
	// A Viewer cannot reorganise a workspace.
	if w = do(a.moveItem, viewer, "POST", `{"targetFolderId":""}`,
		map[string]string{"wid": ws.ID, "iid": it.ID}); w.Code != 403 {
		t.Errorf("viewer move = %d, want 403", w.Code)
	}
}

func TestFolderGetUpdateDeleteMove(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	parent := &store.Folder{WorkspaceID: ws.ID, DisplayName: "parent"}
	if err := st.CreateFolder(parent); err != nil {
		t.Fatal(err)
	}
	child := &store.Folder{WorkspaceID: ws.ID, DisplayName: "child", ParentFolderID: parent.ID}
	if err := st.CreateFolder(child); err != nil {
		t.Fatal(err)
	}

	w := do(a.getFolder, viewer, "GET", "", map[string]string{"wid": ws.ID, "fid": child.ID})
	if w.Code != 200 {
		t.Fatalf("get = %d %s", w.Code, w.Body.Bytes())
	}
	w = do(a.updateFolder, admin, "PATCH", `{"displayName":"renamed"}`, map[string]string{"wid": ws.ID, "fid": child.ID})
	if w.Code != 200 {
		t.Fatalf("rename = %d %s", w.Code, w.Body.Bytes())
	}
	w = do(a.moveFolder, admin, "POST", `{"targetFolderId":""}`, map[string]string{"wid": ws.ID, "fid": child.ID})
	if w.Code != 200 {
		t.Fatalf("move = %d %s", w.Code, w.Body.Bytes())
	}
	if w := do(a.moveFolder, admin, "POST", `{"targetFolderId":"`+child.ID+`"}`,
		map[string]string{"wid": ws.ID, "fid": child.ID}); w.Code != 400 {
		t.Fatalf("cycle = %d", w.Code)
	}
	if w := do(a.deleteFolder, admin, "DELETE", "", map[string]string{"wid": ws.ID, "fid": parent.ID}); w.Code != 200 {
		t.Fatalf("delete empty parent = %d %s", w.Code, w.Body.Bytes())
	}
	it := &store.Item{WorkspaceID: ws.ID, Type: "Notebook", DisplayName: "nb", FolderID: child.ID}
	if err := st.CreateItem(it, nil); err != nil {
		t.Fatal(err)
	}
	if w := do(a.deleteFolder, admin, "DELETE", "", map[string]string{"wid": ws.ID, "fid": child.ID}); w.Code != 400 || errorCode(t, w) != "FolderNotEmpty" {
		t.Fatalf("delete nonempty = %d %s", w.Code, w.Body.Bytes())
	}
	if w := do(a.getFolder, admin, "GET", "", map[string]string{"wid": ws.ID, "fid": "missing"}); w.Code != 404 {
		t.Fatalf("missing get = %d", w.Code)
	}
}

func TestFolderHandlerErrors(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	left := &store.Folder{WorkspaceID: ws.ID, DisplayName: "left"}
	right := &store.Folder{WorkspaceID: ws.ID, DisplayName: "right"}
	for _, f := range []*store.Folder{left, right} {
		if err := st.CreateFolder(f); err != nil {
			t.Fatal(err)
		}
	}
	fid := map[string]string{"wid": ws.ID, "fid": left.ID}

	if w := do(a.getFolder, nobody, "GET", "", fid); w.Code != 403 {
		t.Fatalf("ungranted get = %d", w.Code)
	}
	if w := do(a.updateFolder, viewer, "PATCH", `{"displayName":"x"}`, fid); w.Code != 403 {
		t.Fatalf("viewer update = %d", w.Code)
	}
	if w := do(a.updateFolder, admin, "PATCH", `{"displayName":"x"}`, map[string]string{"wid": ws.ID, "fid": "missing"}); w.Code != 404 {
		t.Fatalf("update missing = %d", w.Code)
	}
	if w := do(a.updateFolder, admin, "PATCH", `{}`, fid); w.Code != 400 {
		t.Fatalf("update empty = %d", w.Code)
	}
	if w := do(a.updateFolder, admin, "PATCH", `{`, fid); w.Code != 400 {
		t.Fatalf("update malformed = %d", w.Code)
	}
	if w := do(a.updateFolder, admin, "PATCH", `{"displayName":"right"}`, fid); w.Code != 409 {
		t.Fatalf("rename conflict = %d %s", w.Code, w.Body.Bytes())
	}
	if w := do(a.deleteFolder, viewer, "DELETE", "", fid); w.Code != 403 {
		t.Fatalf("viewer delete = %d", w.Code)
	}
	if w := do(a.deleteFolder, admin, "DELETE", "", map[string]string{"wid": ws.ID, "fid": "missing"}); w.Code != 404 {
		t.Fatalf("delete missing = %d", w.Code)
	}
	if w := do(a.moveFolder, viewer, "POST", `{}`, fid); w.Code != 403 {
		t.Fatalf("viewer move = %d", w.Code)
	}
	if w := do(a.moveFolder, admin, "POST", `{`, fid); w.Code != 400 {
		t.Fatalf("move malformed = %d", w.Code)
	}
	if w := do(a.moveFolder, admin, "POST", `{"targetFolderId":"missing"}`, fid); w.Code != 404 {
		t.Fatalf("move missing target = %d", w.Code)
	}
	if w := do(a.moveFolder, admin, "POST", `{}`, map[string]string{"wid": ws.ID, "fid": "missing"}); w.Code != 404 {
		t.Fatalf("move missing source = %d", w.Code)
	}
	if w := do(a.moveFolder, admin, "POST", "", fid); w.Code != 200 {
		t.Fatalf("empty-body move to root = %d", w.Code)
	}
	if w := do(a.createFolder, admin, "POST", `{"displayName":"x","parentFolderId":"missing"}`, map[string]string{"wid": ws.ID}); w.Code != 404 {
		t.Fatalf("create under missing parent = %d", w.Code)
	}

	otherParent := &store.Folder{WorkspaceID: ws.ID, DisplayName: "other"}
	if err := st.CreateFolder(otherParent); err != nil {
		t.Fatal(err)
	}
	clash := &store.Folder{WorkspaceID: ws.ID, DisplayName: "left", ParentFolderID: otherParent.ID}
	if err := st.CreateFolder(clash); err != nil {
		t.Fatal(err)
	}
	if w := do(a.moveFolder, admin, "POST", `{"targetFolderId":"`+otherParent.ID+`"}`, fid); w.Code != 409 {
		t.Fatalf("move name clash = %d %s", w.Code, w.Body.Bytes())
	}
}

func TestBulkMoveItems(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	dst := &store.Folder{WorkspaceID: ws.ID, DisplayName: "dst"}
	if err := st.CreateFolder(dst); err != nil {
		t.Fatal(err)
	}
	a1 := &store.Item{WorkspaceID: ws.ID, Type: "Notebook", DisplayName: "a"}
	a2 := &store.Item{WorkspaceID: ws.ID, Type: "Notebook", DisplayName: "b"}
	for _, it := range []*store.Item{a1, a2} {
		if err := st.CreateItem(it, nil); err != nil {
			t.Fatal(err)
		}
	}
	body := `{"targetFolderId":"` + dst.ID + `","items":["` + a1.ID + `","` + a2.ID + `"]}`
	w := do(a.bulkMoveItems, admin, "POST", body, map[string]string{"wid": ws.ID})
	if w.Code != 200 {
		t.Fatalf("bulk = %d %s", w.Code, w.Body.Bytes())
	}
	got, _ := st.GetItem(ws.ID, a1.ID)
	if got.FolderID != dst.ID {
		t.Fatalf("folder = %q", got.FolderID)
	}
	if w := do(a.bulkMoveItems, admin, "POST", `{"items":[]}`, map[string]string{"wid": ws.ID}); w.Code != 400 {
		t.Fatalf("empty = %d", w.Code)
	}
	if w := do(a.bulkMoveItems, admin, "POST", `{"items":["missing"]}`, map[string]string{"wid": ws.ID}); w.Code != 404 {
		t.Fatalf("missing item = %d", w.Code)
	}
	if w := do(a.bulkMoveItems, viewer, "POST", body, map[string]string{"wid": ws.ID}); w.Code != 403 {
		t.Fatalf("viewer = %d", w.Code)
	}
	if w := do(a.bulkMoveItems, admin, "POST", `{`, map[string]string{"wid": ws.ID}); w.Code != 400 {
		t.Fatalf("malformed = %d", w.Code)
	}
	if w := do(a.bulkMoveItems, admin, "POST", `{"items":["`+a1.ID+`"],"targetFolderId":"missing"}`, map[string]string{"wid": ws.ID}); w.Code != 404 {
		t.Fatalf("missing folder = %d", w.Code)
	}
	ids := make([]string, 51)
	for i := range ids {
		ids[i] = `"x"`
	}
	over := `{"items":[` + strings.Join(ids, ",") + `]}`
	if w := do(a.bulkMoveItems, admin, "POST", over, map[string]string{"wid": ws.ID}); w.Code != 400 {
		t.Fatalf("over 50 = %d", w.Code)
	}
	if w := do(a.bulkMoveItems, admin, "POST", `{"items":["`+a1.ID+`"]}`, map[string]string{"wid": ws.ID}); w.Code != 200 {
		t.Fatalf("move to root = %d %s", w.Code, w.Body.Bytes())
	}
}
