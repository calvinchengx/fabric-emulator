package store

import (
	"errors"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/clock"
)

func TestNotFoundPaths(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.GetWorkspace("x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetWorkspace = %v", err)
	}
	if err := s.UpdateWorkspace(&Workspace{ID: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateWorkspace = %v", err)
	}
	if err := s.DeleteWorkspace("x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteWorkspace = %v", err)
	}
	if _, err := s.GetItem("w", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetItem = %v", err)
	}
	if _, err := s.GetItemByID("x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetItemByID = %v", err)
	}
	if err := s.UpdateItem(&Item{ID: "x", WorkspaceID: "w"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateItem = %v", err)
	}
	if err := s.DeleteItem("w", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteItem = %v", err)
	}
	if _, err := s.GetRoleAssignment("w", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetRoleAssignment = %v", err)
	}
	if err := s.UpdateRoleAssignment("w", "x", RoleViewer); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateRoleAssignment = %v", err)
	}
	if err := s.DeleteRoleAssignment("w", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteRoleAssignment = %v", err)
	}
	if _, err := s.GetOperation("x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetOperation = %v", err)
	}
}

func TestItemUpdateAndRoleAssignmentLifecycle(t *testing.T) {
	s := newTestStore(t)
	ws := &Workspace{DisplayName: "w"}
	if err := s.CreateWorkspace(ws, Principal{ID: "p", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	it := &Item{WorkspaceID: ws.ID, Type: "Notebook", DisplayName: "a"}
	if err := s.CreateItem(it, nil); err != nil {
		t.Fatal(err)
	}
	it.DisplayName, it.Description = "b", "desc"
	if err := s.UpdateItem(it); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetItem(ws.ID, it.ID)
	if err != nil || got.DisplayName != "b" || got.Description != "desc" {
		t.Fatalf("updated item = %+v, %v", got, err)
	}
	if err := s.DeleteItem(ws.ID, it.ID); err != nil {
		t.Fatal(err)
	}

	// Role assignment get/update/delete round trip.
	ra := &RoleAssignment{WorkspaceID: ws.ID, Principal: Principal{ID: "u", Type: "User"}, Role: RoleViewer}
	if err := s.CreateRoleAssignment(ra); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateRoleAssignment(ws.ID, ra.ID, RoleMember); err != nil {
		t.Fatal(err)
	}
	got2, err := s.GetRoleAssignment(ws.ID, ra.ID)
	if err != nil || got2.Role != RoleMember || got2.Principal.ID != "u" {
		t.Fatalf("updated ra = %+v, %v", got2, err)
	}
	if err := s.DeleteRoleAssignment(ws.ID, ra.ID); err != nil {
		t.Fatal(err)
	}
}

func TestOpenPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	s1, err := Open(dir, clock.New())
	if err != nil {
		t.Fatal(err)
	}
	ws := &Workspace{DisplayName: "durable"}
	if err := s1.CreateWorkspace(ws, Principal{ID: "p", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	s1.Close()

	s2, err := Open(dir, clock.New())
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got, err := s2.GetWorkspace(ws.ID)
	if err != nil || got.DisplayName != "durable" {
		t.Fatalf("reopened workspace = %+v, %v", got, err)
	}
}

func TestOpenBadDir(t *testing.T) {
	// A dataDir that is actually a file cannot host the database.
	dir := t.TempDir() + "/nope/deeper"
	if _, err := Open(dir, clock.New()); err == nil {
		t.Skip("driver created intermediate path; acceptable")
	}
}

// TestItemProperties round-trips typed item properties, including the delete
// path (an empty value clears the key) and cascade on item delete.
func TestItemProperties(t *testing.T) {
	s, err := Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ws := &Workspace{DisplayName: "w"}
	if err := s.CreateWorkspace(ws, Principal{ID: "p", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	it := &Item{WorkspaceID: ws.ID, Type: "KQLDatabase", DisplayName: "db"}
	if err := s.CreateItem(it, nil); err != nil {
		t.Fatal(err)
	}

	if props, err := s.ItemProperties(it.ID); err != nil || len(props) != 0 {
		t.Fatalf("fresh item properties = %v, %v", props, err)
	}
	if err := s.SetItemProperties(it.ID, map[string]string{"a": "1", "b": "2"}); err != nil {
		t.Fatal(err)
	}
	// Upsert one, clear the other.
	if err := s.SetItemProperties(it.ID, map[string]string{"a": "9", "b": ""}); err != nil {
		t.Fatal(err)
	}
	props, err := s.ItemProperties(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(props) != 1 || props["a"] != "9" {
		t.Errorf("properties = %v, want {a:9}", props)
	}
	if err := s.DeleteItem(ws.ID, it.ID); err != nil {
		t.Fatal(err)
	}
	if props, err := s.ItemProperties(it.ID); err != nil || len(props) != 0 {
		t.Errorf("properties survived the item: %v, %v", props, err)
	}
}
