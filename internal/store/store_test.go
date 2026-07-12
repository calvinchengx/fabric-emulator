package store

import (
	"regexp"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/clock"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestNewIDShape(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := map[string]bool{}
	for range 100 {
		id := NewID()
		if !re.MatchString(id) {
			t.Fatalf("NewID() = %q, not a lowercase UUIDv4", id)
		}
		if seen[id] {
			t.Fatalf("NewID() repeated %q", id)
		}
		seen[id] = true
	}
}

func TestWorkspaceLifecycleAndRBAC(t *testing.T) {
	s := newTestStore(t)
	creator := Principal{ID: "sp-1", Type: "ServicePrincipal"}

	ws := &Workspace{DisplayName: "Analytics"}
	if err := s.CreateWorkspace(ws, creator); err != nil {
		t.Fatal(err)
	}
	// Creator is Admin.
	role, err := s.RoleOf(ws.ID, "sp-1")
	if err != nil || role != RoleAdmin {
		t.Fatalf("creator role = %q, %v; want Admin", role, err)
	}
	// Stranger has no role.
	if role, _ := s.RoleOf(ws.ID, "nobody"); role != "" {
		t.Fatalf("stranger role = %q; want none", role)
	}
	// Listing is scoped to the principal.
	if ws2, _ := s.ListWorkspacesFor("sp-1"); len(ws2) != 1 {
		t.Fatalf("creator sees %d workspaces; want 1", len(ws2))
	}
	if ws2, _ := s.ListWorkspacesFor("nobody"); len(ws2) != 0 {
		t.Fatalf("stranger sees %d workspaces; want 0", len(ws2))
	}

	// Grant + duplicate rejection.
	ra := &RoleAssignment{WorkspaceID: ws.ID, Principal: Principal{ID: "u-1", Type: "User"}, Role: RoleViewer}
	if err := s.CreateRoleAssignment(ra); err != nil {
		t.Fatal(err)
	}
	dup := &RoleAssignment{WorkspaceID: ws.ID, Principal: Principal{ID: "u-1", Type: "User"}, Role: RoleAdmin}
	if err := s.CreateRoleAssignment(dup); err == nil {
		t.Fatal("duplicate principal grant succeeded; want unique violation")
	}

	// Delete cascades assignments and items.
	it := &Item{WorkspaceID: ws.ID, Type: "Notebook", DisplayName: "nb"}
	if err := s.CreateItem(it, []DefinitionPart{{Path: ".platform", Payload: "e30=", PayloadType: "InlineBase64"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteWorkspace(ws.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetItemByID(it.ID); err == nil {
		t.Fatal("item survived workspace delete; want cascade")
	}
	if role, _ := s.RoleOf(ws.ID, "sp-1"); role != "" {
		t.Fatal("role assignment survived workspace delete; want cascade")
	}
}

func TestItemDefinitionRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ws := &Workspace{DisplayName: "w"}
	if err := s.CreateWorkspace(ws, Principal{ID: "p", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	parts := []DefinitionPart{
		{Path: "notebook-content.py", Payload: "cHJpbnQoMSk=", PayloadType: "InlineBase64"},
		{Path: ".platform", Payload: "e30=", PayloadType: "InlineBase64"},
	}
	it := &Item{WorkspaceID: ws.ID, Type: "Notebook", DisplayName: "nb"}
	if err := s.CreateItem(it, parts); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetDefinition(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != parts[0] || got[1] != parts[1] {
		t.Fatalf("definition did not round-trip: %+v", got)
	}
	// Replace and re-read.
	parts[0].Payload = "cHJpbnQoMik="
	if err := s.SetDefinition(it.ID, parts[:1]); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetDefinition(it.ID)
	if len(got) != 1 || got[0].Payload != "cHJpbnQoMik=" {
		t.Fatalf("SetDefinition did not replace: %+v", got)
	}
	// Item without definition reads nil.
	it2 := &Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lh"}
	if err := s.CreateItem(it2, nil); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.GetDefinition(it2.ID); d != nil {
		t.Fatalf("empty definition = %+v; want nil", d)
	}
}

func TestOperationStatusDerivation(t *testing.T) {
	s := newTestStore(t)
	s.Clock.Freeze()
	now := s.Now()

	op := &Operation{Kind: "CreateItem", CompleteAt: now + 60, ResultRef: "x"}
	if err := s.CreateOperation(op); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetOperation(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st := got.StatusAt(now); st != OpNotStarted {
		t.Fatalf("status at creation = %q; want NotStarted", st)
	}
	if st := got.StatusAt(now + 30); st != OpRunning {
		t.Fatalf("status mid-flight = %q; want Running", st)
	}
	if st := got.StatusAt(now + 60); st != OpSucceeded {
		t.Fatalf("status at completeAt = %q; want Succeeded", st)
	}

	fail := &Operation{Kind: "CreateItem", CompleteAt: now, FailWith: "OperationFailed"}
	if err := s.CreateOperation(fail); err != nil {
		t.Fatal(err)
	}
	if st := fail.StatusAt(now + 1); st != OpFailed {
		t.Fatalf("failed op status = %q; want Failed", st)
	}
}

func TestRoleRank(t *testing.T) {
	order := []string{RoleViewer, RoleContributor, RoleMember, RoleAdmin}
	for i := 1; i < len(order); i++ {
		if RoleRank(order[i]) <= RoleRank(order[i-1]) {
			t.Fatalf("RoleRank(%s) not above RoleRank(%s)", order[i], order[i-1])
		}
	}
	if RoleRank("Owner") != -1 {
		t.Fatal("unknown role should rank -1")
	}
}
