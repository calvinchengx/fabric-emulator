package store

import (
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/clock"
)

// TestClosedDBErrors drives every repository method over a closed database,
// covering the error-return paths a healthy store never takes.
func TestClosedDBErrors(t *testing.T) {
	s, err := Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	p := Principal{ID: "p", Type: "User"}
	if err := s.CreateWorkspace(&Workspace{DisplayName: "w"}, p); err == nil {
		t.Error("CreateWorkspace on closed DB succeeded")
	}
	if _, err := s.GetWorkspace("x"); err == nil {
		t.Error("GetWorkspace on closed DB succeeded")
	}
	if _, err := s.ListWorkspacesFor("p"); err == nil {
		t.Error("ListWorkspacesFor on closed DB succeeded")
	}
	if err := s.UpdateWorkspace(&Workspace{ID: "x"}); err == nil {
		t.Error("UpdateWorkspace on closed DB succeeded")
	}
	if err := s.DeleteWorkspace("x"); err == nil {
		t.Error("DeleteWorkspace on closed DB succeeded")
	}
	if _, err := s.RoleOf("w", "p"); err == nil {
		t.Error("RoleOf on closed DB succeeded")
	}
	if _, err := s.ListRoleAssignments("w"); err == nil {
		t.Error("ListRoleAssignments on closed DB succeeded")
	}
	if _, err := s.GetRoleAssignment("w", "x"); err == nil {
		t.Error("GetRoleAssignment on closed DB succeeded")
	}
	if err := s.CreateRoleAssignment(&RoleAssignment{WorkspaceID: "w", Principal: p, Role: RoleViewer}); err == nil {
		t.Error("CreateRoleAssignment on closed DB succeeded")
	}
	if err := s.UpdateRoleAssignment("w", "x", RoleViewer); err == nil {
		t.Error("UpdateRoleAssignment on closed DB succeeded")
	}
	if err := s.DeleteRoleAssignment("w", "x"); err == nil {
		t.Error("DeleteRoleAssignment on closed DB succeeded")
	}
	if err := s.CreateItem(&Item{WorkspaceID: "w", Type: "t", DisplayName: "d"}, nil); err == nil {
		t.Error("CreateItem on closed DB succeeded")
	}
	if _, err := s.GetItem("w", "x"); err == nil {
		t.Error("GetItem on closed DB succeeded")
	}
	if _, err := s.GetItemByID("x"); err == nil {
		t.Error("GetItemByID on closed DB succeeded")
	}
	if _, err := s.ListItems("w", ""); err == nil {
		t.Error("ListItems on closed DB succeeded")
	}
	if err := s.UpdateItem(&Item{ID: "x", WorkspaceID: "w"}); err == nil {
		t.Error("UpdateItem on closed DB succeeded")
	}
	if err := s.DeleteItem("w", "x"); err == nil {
		t.Error("DeleteItem on closed DB succeeded")
	}
	if _, err := s.GetDefinition("x"); err == nil {
		t.Error("GetDefinition on closed DB succeeded")
	}
	if err := s.SetDefinition("x", nil); err == nil {
		t.Error("SetDefinition on closed DB succeeded")
	}
	if err := s.CreateOperation(&Operation{Kind: "k"}); err == nil {
		t.Error("CreateOperation on closed DB succeeded")
	}
	if _, err := s.GetOperation("x"); err == nil {
		t.Error("GetOperation on closed DB succeeded")
	}
}
