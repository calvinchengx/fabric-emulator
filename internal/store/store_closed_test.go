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
	if err := s.CreateConnection(&Connection{DisplayName: "c"}); err == nil {
		t.Error("CreateConnection on closed DB succeeded")
	}
	if _, err := s.GetConnection("x"); err == nil {
		t.Error("GetConnection on closed DB succeeded")
	}
	if _, err := s.ListConnections(); err == nil {
		t.Error("ListConnections on closed DB succeeded")
	}
	if err := s.SetGitConnection(&GitConnection{WorkspaceID: "w"}); err == nil {
		t.Error("SetGitConnection on closed DB succeeded")
	}
	if _, err := s.GetGitConnection("w"); err == nil {
		t.Error("GetGitConnection on closed DB succeeded")
	}
	if err := s.DeleteGitConnection("w"); err == nil {
		t.Error("DeleteGitConnection on closed DB succeeded")
	}
	if _, err := s.GetRemoteHead("rk", "b"); err == nil {
		t.Error("GetRemoteHead on closed DB succeeded")
	}
	if _, err := s.ListRemoteItems("rk", "b"); err == nil {
		t.Error("ListRemoteItems on closed DB succeeded")
	}
	if _, err := s.CommitRemoteItems("rk", "b", nil); err == nil {
		t.Error("CommitRemoteItems on closed DB succeeded")
	}
	if err := s.CreateJobInstance(&JobInstance{ItemID: "i", JobType: "j"}); err == nil {
		t.Error("CreateJobInstance on closed DB succeeded")
	}
	if _, err := s.GetJobInstance("i", "x"); err == nil {
		t.Error("GetJobInstance on closed DB succeeded")
	}
	if err := s.CancelJobInstance("i", "x"); err == nil {
		t.Error("CancelJobInstance on closed DB succeeded")
	}
}

func TestClosedDBFolderErrors(t *testing.T) {
	s, err := Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := s.CreateFolder(&Folder{WorkspaceID: "w", DisplayName: "f"}); err == nil {
		t.Error("CreateFolder on closed DB succeeded")
	}
	if _, err := s.ListFolders("w"); err == nil {
		t.Error("ListFolders on closed DB succeeded")
	}
}

func TestClosedDBIdentityErrors(t *testing.T) {
	s, err := Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := s.SetWorkspaceIdentity(&WorkspaceIdentity{WorkspaceID: "w", IdentityID: "i", AppID: "a"}); err == nil {
		t.Error("SetWorkspaceIdentity on closed DB succeeded")
	}
	if _, err := s.GetWorkspaceIdentity("w"); err == nil {
		t.Error("GetWorkspaceIdentity on closed DB succeeded")
	}
	if err := s.DeleteWorkspaceIdentity("w"); err == nil {
		t.Error("DeleteWorkspaceIdentity on closed DB succeeded")
	}
}

func TestClosedDBCapacityErrors(t *testing.T) {
	s, err := Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := s.GetCapacity("x"); err == nil {
		t.Error("GetCapacity on closed DB succeeded")
	}
	if _, err := s.ListCapacities(); err == nil {
		t.Error("ListCapacities on closed DB succeeded")
	}
}

func TestClosedDBShortcutErrors(t *testing.T) {
	s, err := Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := s.CreateShortcut(&Shortcut{ItemID: "i", Path: "p", Name: "n"}); err == nil {
		t.Error("CreateShortcut on closed DB succeeded")
	}
	if _, err := s.GetShortcut("i", "p", "n"); err == nil {
		t.Error("GetShortcut on closed DB succeeded")
	}
	if _, err := s.ListShortcuts("i"); err == nil {
		t.Error("ListShortcuts on closed DB succeeded")
	}
	if err := s.DeleteShortcut("i", "p", "n"); err == nil {
		t.Error("DeleteShortcut on closed DB succeeded")
	}
	if _, _, err := s.ShortcutFor("i", "p"); err == nil {
		t.Error("ShortcutFor on closed DB succeeded")
	}
}
