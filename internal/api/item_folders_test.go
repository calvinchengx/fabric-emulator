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
