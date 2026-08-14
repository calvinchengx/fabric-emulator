package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/clock"
)

func TestFolderGetUpdateDeleteMove(t *testing.T) {
	s, err := Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ws := &Workspace{DisplayName: "w"}
	if err := s.CreateWorkspace(ws, Principal{ID: "a", Type: "User"}); err != nil {
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
	if err := s.MoveFolder(ws.ID, parent.ID, child.ID); !errors.Is(err, ErrFolderCycle) {
		t.Fatalf("descendant move = %v", err)
	}
	got, err := s.GetFolder(ws.ID, child.ID)
	if err != nil || got.DisplayName != "child" {
		t.Fatalf("get: %+v %v", got, err)
	}
	child.DisplayName = "renamed"
	if err := s.UpdateFolder(child); err != nil {
		t.Fatal(err)
	}
	if err := s.MoveFolder(ws.ID, child.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.MoveFolder(ws.ID, child.ID, child.ID); !errors.Is(err, ErrFolderCycle) {
		t.Fatalf("self move = %v", err)
	}
	if _, err := s.GetFolder(ws.ID, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
	it := &Item{WorkspaceID: ws.ID, Type: "Notebook", DisplayName: "nb", FolderID: child.ID}
	if err := s.CreateItem(it, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteFolder(ws.ID, child.ID); !errors.Is(err, ErrFolderNotEmpty) {
		t.Fatalf("nonempty item = %v", err)
	}
	if err := s.DeleteItem(ws.ID, it.ID); err != nil {
		t.Fatal(err)
	}
	nested := &Folder{WorkspaceID: ws.ID, DisplayName: "nested", ParentFolderID: child.ID}
	if err := s.CreateFolder(nested); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteFolder(ws.ID, child.ID); !errors.Is(err, ErrFolderNotEmpty) {
		t.Fatalf("nonempty child folder = %v", err)
	}
	if err := s.DeleteFolder(ws.ID, nested.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteFolder(ws.ID, child.ID); err != nil {
		t.Fatal(err)
	}
	dup := &Folder{WorkspaceID: ws.ID, DisplayName: "parent"}
	if err := s.CreateFolder(dup); !errors.Is(err, ErrNameConflict) {
		t.Fatalf("dup = %v", err)
	}

	sib := &Folder{WorkspaceID: ws.ID, DisplayName: "sib"}
	if err := s.CreateFolder(sib); err != nil {
		t.Fatal(err)
	}
	sib.DisplayName = "parent"
	if err := s.UpdateFolder(sib); !errors.Is(err, ErrNameConflict) {
		t.Fatalf("rename clash = %v", err)
	}
	under := &Folder{WorkspaceID: ws.ID, DisplayName: "sib", ParentFolderID: parent.ID}
	if err := s.CreateFolder(under); err != nil {
		t.Fatal(err)
	}
	if err := s.MoveFolder(ws.ID, sib.ID, parent.ID); !errors.Is(err, ErrNameConflict) {
		t.Fatalf("move clash = %v", err)
	}
	if err := s.DeleteFolder(ws.ID, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing = %v", err)
	}
	if err := s.MoveFolder(ws.ID, sib.ID, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("move missing target = %v", err)
	}
}

func TestFolderIsUnderDefensiveCycle(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, clock.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ws := &Workspace{DisplayName: "w"}
	if err := s.CreateWorkspace(ws, Principal{ID: "a", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	a := &Folder{WorkspaceID: ws.ID, DisplayName: "A"}
	b := &Folder{WorkspaceID: ws.ID, DisplayName: "B"}
	x := &Folder{WorkspaceID: ws.ID, DisplayName: "X"}
	for _, f := range []*Folder{a, b, x} {
		if err := s.CreateFolder(f); err != nil {
			t.Fatal(err)
		}
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "fabric-emulator.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE folders SET parent_id = ? WHERE id = ?`, b.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE folders SET parent_id = ? WHERE id = ?`, a.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.MoveFolder(ws.ID, x.ID, a.ID); !errors.Is(err, ErrFolderCycle) {
		t.Fatalf("cycle in target chain = %v", err)
	}

	ghost := &Folder{WorkspaceID: ws.ID, DisplayName: "ghost-parent"}
	if err := s.CreateFolder(ghost); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE folders SET parent_id = 'missing-parent' WHERE id = ?`, ghost.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.MoveFolder(ws.ID, x.ID, ghost.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("dangling parent = %v", err)
	}
	if _, err := db.Exec(`DROP TABLE items`); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteFolder(ws.ID, x.ID); err == nil {
		t.Fatal("DeleteFolder with no items table succeeded")
	}
}
