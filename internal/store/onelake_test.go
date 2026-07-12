package store

import (
	"bytes"
	"errors"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/clock"
)

// newOneLakeItem creates a workspace and an item to host OneLake paths
// (onelake_paths has foreign keys onto both).
func newOneLakeItem(t *testing.T, s *Store) (workspaceID, itemID string) {
	t.Helper()
	ws := &Workspace{DisplayName: "onelake-" + NewID()}
	if err := s.CreateWorkspace(ws, Principal{ID: "p", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	it := &Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lh"}
	if err := s.CreateItem(it, nil); err != nil {
		t.Fatal(err)
	}
	return ws.ID, it.ID
}

func TestOneLakePathLifecycle(t *testing.T) {
	s := newTestStore(t)
	wsID, itID := newOneLakeItem(t, s)

	// Create a file and read it back. Content is non-nil even when empty,
	// matching the DFS handler (io.ReadAll of the request body).
	f := &OneLakePath{WorkspaceID: wsID, ItemID: itID, RelPath: "Files/raw/a.txt", Content: []byte{}}
	if err := s.CreateOneLakePath(f, false); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetOneLakePath(itID, "Files/raw/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got.WorkspaceID != wsID || got.ItemID != itID || got.IsDir || len(got.Content) != 0 {
		t.Fatalf("created file = %+v", got)
	}

	// Create a directory.
	d := &OneLakePath{WorkspaceID: wsID, ItemID: itID, RelPath: "Files/raw", IsDir: true, Content: []byte{}}
	if err := s.CreateOneLakePath(d, false); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetOneLakePath(itID, "Files/raw")
	if err != nil || !got.IsDir {
		t.Fatalf("created dir = %+v, %v", got, err)
	}

	// Re-creating an existing path overwrites (ADLS create truncates).
	if _, err := s.AppendOneLakePath(itID, "Files/raw/a.txt", 0, []byte("data")); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateOneLakePath(&OneLakePath{WorkspaceID: wsID, ItemID: itID, RelPath: "Files/raw/a.txt", Content: []byte{}}, false); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetOneLakePath(itID, "Files/raw/a.txt")
	if len(got.Content) != 0 {
		t.Fatalf("re-create did not truncate: %q", got.Content)
	}

	// Missing path is ErrNotFound.
	if _, err := s.GetOneLakePath(itID, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetOneLakePath missing = %v", err)
	}
}

func TestAppendOneLakePath(t *testing.T) {
	s := newTestStore(t)
	wsID, itID := newOneLakeItem(t, s)
	if err := s.CreateOneLakePath(&OneLakePath{WorkspaceID: wsID, ItemID: itID, RelPath: "f.txt", Content: []byte{}}, false); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateOneLakePath(&OneLakePath{WorkspaceID: wsID, ItemID: itID, RelPath: "d", IsDir: true, Content: []byte{}}, false); err != nil {
		t.Fatal(err)
	}

	n, err := s.AppendOneLakePath(itID, "f.txt", 0, []byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("first append = %d, %v; want 5", n, err)
	}
	n, err = s.AppendOneLakePath(itID, "f.txt", 5, []byte(" world"))
	if err != nil || n != 11 {
		t.Fatalf("second append = %d, %v; want 11", n, err)
	}
	got, _ := s.GetOneLakePath(itID, "f.txt")
	if !bytes.Equal(got.Content, []byte("hello world")) {
		t.Fatalf("content = %q; want %q", got.Content, "hello world")
	}

	// Wrong position rejected.
	if _, err := s.AppendOneLakePath(itID, "f.txt", 3, []byte("x")); err == nil {
		t.Fatal("append at wrong position succeeded")
	}
	// Directories reject appends.
	if _, err := s.AppendOneLakePath(itID, "d", 0, []byte("x")); err == nil {
		t.Fatal("append to directory succeeded")
	}
	// Missing path is ErrNotFound.
	if _, err := s.AppendOneLakePath(itID, "nope", 0, []byte("x")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("append to missing path = %v", err)
	}
}

func TestListOneLakePaths(t *testing.T) {
	s := newTestStore(t)
	wsID, itID := newOneLakeItem(t, s)
	// Parent directories stay implicit, as in ADLS.
	for _, p := range []OneLakePath{
		{RelPath: "Files/raw/a.txt"},
		{RelPath: "Files/raw/b.txt"},
		{RelPath: "Files/top.txt"},
		{RelPath: "Tables", IsDir: true},
	} {
		p.WorkspaceID, p.ItemID, p.Content = wsID, itID, []byte{}
		if err := s.CreateOneLakePath(&p, false); err != nil {
			t.Fatal(err)
		}
	}

	// Recursive over the whole item sees every path.
	all, err := s.ListOneLakePaths(itID, "", true)
	if err != nil || len(all) != 4 {
		t.Fatalf("recursive list = %d paths, %v; want 4", len(all), err)
	}

	// Recursive under a prefix excludes the prefix itself and non-descendants.
	under, err := s.ListOneLakePaths(itID, "Files/raw", true)
	if err != nil || len(under) != 2 {
		t.Fatalf("recursive prefix list = %d paths, %v; want 2", len(under), err)
	}
	if under[0].RelPath != "Files/raw/a.txt" || under[1].RelPath != "Files/raw/b.txt" {
		t.Fatalf("prefix list paths = %q, %q", under[0].RelPath, under[1].RelPath)
	}

	// A trailing slash on the prefix is tolerated.
	under2, err := s.ListOneLakePaths(itID, "Files/raw/", true)
	if err != nil || len(under2) != 2 {
		t.Fatalf("trailing-slash prefix list = %d paths, %v; want 2", len(under2), err)
	}

	// Non-recursive collapses deeper entries to their first-level directory.
	top, err := s.ListOneLakePaths(itID, "Files", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 2 || top[0].RelPath != "Files/raw" || !top[0].IsDir || top[1].RelPath != "Files/top.txt" {
		t.Fatalf("non-recursive list = %+v; want [Files/raw dir, Files/top.txt]", top)
	}

	// Unknown item lists empty.
	if got, err := s.ListOneLakePaths("other", "", true); err != nil || len(got) != 0 {
		t.Fatalf("unknown item list = %d paths, %v; want 0", len(got), err)
	}
}

func TestListOneLakePathsExplicitDirDedupe(t *testing.T) {
	s := newTestStore(t)
	wsID, itID := newOneLakeItem(t, s)
	// Unlike TestListOneLakePaths (where parent directories stay implicit),
	// create an explicit directory row *and* a child beneath it: the
	// explicit row and the child's collapsed entry must merge, not repeat.
	for _, p := range []OneLakePath{
		{RelPath: "Files/dir", IsDir: true},
		{RelPath: "Files/dir/child.txt"},
	} {
		p.WorkspaceID, p.ItemID, p.Content = wsID, itID, []byte{}
		if err := s.CreateOneLakePath(&p, false); err != nil {
			t.Fatal(err)
		}
	}

	top, err := s.ListOneLakePaths(itID, "Files", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 1 || top[0].RelPath != "Files/dir" || !top[0].IsDir {
		t.Fatalf("non-recursive list = %+v; want just [Files/dir dir]", top)
	}

	// Recursive listings still see both rows.
	all, err := s.ListOneLakePaths(itID, "Files", true)
	if err != nil || len(all) != 2 {
		t.Fatalf("recursive list = %d paths, %v; want 2", len(all), err)
	}
}

func TestDeleteOneLakePath(t *testing.T) {
	s := newTestStore(t)
	wsID, itID := newOneLakeItem(t, s)
	for _, rel := range []string{"Files/raw/a.txt", "Files/raw/b.txt", "Files/top.txt"} {
		if err := s.CreateOneLakePath(&OneLakePath{WorkspaceID: wsID, ItemID: itID, RelPath: rel, Content: []byte{}}, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateOneLakePath(&OneLakePath{WorkspaceID: wsID, ItemID: itID, RelPath: "Files/raw", IsDir: true, Content: []byte{}}, false); err != nil {
		t.Fatal(err)
	}

	// Deleting a file leaves siblings alone.
	if err := s.DeleteOneLakePath(itID, "Files/raw/a.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetOneLakePath(itID, "Files/raw/b.txt"); err != nil {
		t.Fatalf("sibling deleted: %v", err)
	}

	// Deleting a directory removes its subtree.
	if err := s.DeleteOneLakePath(itID, "Files/raw"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetOneLakePath(itID, "Files/raw/b.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("subtree survived directory delete: %v", err)
	}
	if _, err := s.GetOneLakePath(itID, "Files/top.txt"); err != nil {
		t.Fatalf("unrelated path deleted: %v", err)
	}

	// Missing path is ErrNotFound.
	if err := s.DeleteOneLakePath(itID, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing = %v", err)
	}
}

func TestGetWorkspaceAndItemByName(t *testing.T) {
	s := newTestStore(t)
	ws := &Workspace{DisplayName: "Analytics"}
	if err := s.CreateWorkspace(ws, Principal{ID: "p", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	it := &Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lake"}
	if err := s.CreateItem(it, nil); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetWorkspaceByName("Analytics")
	if err != nil || got.ID != ws.ID {
		t.Fatalf("GetWorkspaceByName = %+v, %v", got, err)
	}
	if _, err := s.GetWorkspaceByName("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetWorkspaceByName missing = %v", err)
	}

	gotIt, err := s.GetItemByName(ws.ID, "lake", "Lakehouse")
	if err != nil || gotIt.ID != it.ID {
		t.Fatalf("GetItemByName = %+v, %v", gotIt, err)
	}
	// Item type matches case-insensitively (name.Type addressing).
	if _, err := s.GetItemByName(ws.ID, "lake", "lakehouse"); err != nil {
		t.Fatalf("GetItemByName case-insensitive type = %v", err)
	}
	if _, err := s.GetItemByName(ws.ID, "nope", "Lakehouse"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetItemByName missing = %v", err)
	}
}

func TestListFoldersRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ws := &Workspace{DisplayName: "w"}
	if err := s.CreateWorkspace(ws, Principal{ID: "p", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	parent := &Folder{WorkspaceID: ws.ID, DisplayName: "parent"}
	if err := s.CreateFolder(parent); err != nil {
		t.Fatal(err)
	}
	child := &Folder{WorkspaceID: ws.ID, DisplayName: "child", ParentFolderID: parent.ID}
	if err := s.CreateFolder(child); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListFolders(ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].DisplayName != "parent" || got[1].ParentFolderID != parent.ID {
		t.Fatalf("ListFolders = %+v", got)
	}
	// Other workspaces list empty.
	if got, err := s.ListFolders("other"); err != nil || len(got) != 0 {
		t.Fatalf("ListFolders other workspace = %d folders, %v; want 0", len(got), err)
	}
}

func TestListRoleAssignmentsAndItems(t *testing.T) {
	s := newTestStore(t)
	ws := &Workspace{DisplayName: "w"}
	if err := s.CreateWorkspace(ws, Principal{ID: "admin", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRoleAssignment(&RoleAssignment{
		WorkspaceID: ws.ID, Principal: Principal{ID: "viewer", Type: "User"}, Role: RoleViewer,
	}); err != nil {
		t.Fatal(err)
	}

	ras, err := s.ListRoleAssignments(ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ras) != 2 || ras[0].Principal.ID != "admin" || ras[0].Role != RoleAdmin ||
		ras[1].Principal.ID != "viewer" || ras[1].Role != RoleViewer {
		t.Fatalf("ListRoleAssignments = %+v", ras)
	}

	nb := &Item{WorkspaceID: ws.ID, Type: "Notebook", DisplayName: "nb"}
	lh := &Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lh"}
	for _, it := range []*Item{nb, lh} {
		if err := s.CreateItem(it, nil); err != nil {
			t.Fatal(err)
		}
	}
	all, err := s.ListItems(ws.ID, "")
	if err != nil || len(all) != 2 {
		t.Fatalf("unfiltered ListItems = %d items, %v; want 2", len(all), err)
	}
	only, err := s.ListItems(ws.ID, "Lakehouse")
	if err != nil || len(only) != 1 || only[0].ID != lh.ID {
		t.Fatalf("filtered ListItems = %+v, %v; want just the lakehouse", only, err)
	}
}

func TestClosedDBOneLakeErrors(t *testing.T) {
	s, err := Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := s.CreateOneLakePath(&OneLakePath{WorkspaceID: "w", ItemID: "i", RelPath: "f", Content: []byte{}}, false); err == nil {
		t.Error("CreateOneLakePath on closed DB succeeded")
	}
	if _, err := s.GetOneLakePath("i", "f"); err == nil {
		t.Error("GetOneLakePath on closed DB succeeded")
	}
	if _, err := s.AppendOneLakePath("i", "f", 0, nil); err == nil {
		t.Error("AppendOneLakePath on closed DB succeeded")
	}
	if _, err := s.ListOneLakePaths("i", "", true); err == nil {
		t.Error("ListOneLakePaths on closed DB succeeded")
	}
	if err := s.DeleteOneLakePath("i", "f"); err == nil {
		t.Error("DeleteOneLakePath on closed DB succeeded")
	}
	if _, err := s.GetWorkspaceByName("w"); err == nil {
		t.Error("GetWorkspaceByName on closed DB succeeded")
	}
	if _, err := s.GetItemByName("w", "d", "t"); err == nil {
		t.Error("GetItemByName on closed DB succeeded")
	}
}

func TestConditionalCreateAndRename(t *testing.T) {
	s := newTestStore(t)
	wsID, itID := newOneLakeItem(t, s)

	// put-if-absent: first wins, second is ErrPathExists and does not write.
	p1 := &OneLakePath{WorkspaceID: wsID, ItemID: itID, RelPath: "Tables/t/_delta_log/0.json", Content: []byte("v1")}
	if err := s.CreateOneLakePath(p1, true); err != nil {
		t.Fatal(err)
	}
	p2 := &OneLakePath{WorkspaceID: wsID, ItemID: itID, RelPath: "Tables/t/_delta_log/0.json", Content: []byte("v2")}
	if err := s.CreateOneLakePath(p2, true); !errors.Is(err, ErrPathExists) {
		t.Fatalf("second conditional create err = %v; want ErrPathExists", err)
	}
	got, _ := s.GetOneLakePath(itID, "Tables/t/_delta_log/0.json")
	if string(got.Content) != "v1" || got.ETag == "" || got.ModifiedAt == 0 {
		t.Fatalf("winner clobbered: %q etag=%q mod=%d", got.Content, got.ETag, got.ModifiedAt)
	}

	// Rename moves a subtree and rotates etags; source disappears.
	for _, rel := range []string{"Files/stage/a", "Files/stage/deep/b"} {
		if err := s.CreateOneLakePath(&OneLakePath{WorkspaceID: wsID, ItemID: itID, RelPath: rel, Content: []byte(rel)}, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RenameOneLakePath(itID, "Files/stage", "Files/final"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetOneLakePath(itID, "Files/stage/a"); !errors.Is(err, ErrNotFound) {
		t.Fatal("source survived rename")
	}
	moved, err := s.GetOneLakePath(itID, "Files/final/deep/b")
	if err != nil || string(moved.Content) != "Files/stage/deep/b" {
		t.Fatalf("moved = %+v, %v", moved, err)
	}
	// Rename over an existing destination overwrites it.
	if err := s.CreateOneLakePath(&OneLakePath{WorkspaceID: wsID, ItemID: itID, RelPath: "Files/final2", Content: []byte("old")}, false); err != nil {
		t.Fatal(err)
	}
	if err := s.RenameOneLakePath(itID, "Files/final", "Files/final2"); err != nil {
		t.Fatal(err)
	}
	if p, err := s.GetOneLakePath(itID, "Files/final2/deep/b"); err != nil || string(p.Content) != "Files/stage/deep/b" {
		t.Fatalf("overwrite rename = %+v, %v", p, err)
	}
	// Renaming a missing source errors.
	if err := s.RenameOneLakePath(itID, "Files/ghost", "Files/x"); err == nil {
		t.Fatal("rename of missing source succeeded")
	}
}
