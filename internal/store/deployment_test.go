package store

// Deployment pipelines D0, store side (docs/23).

import (
	"errors"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/clock"
)

func newDeploymentStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// mkPipeline creates an n-stage pipeline owned by "p".
func mkPipeline(t *testing.T, s *Store, name string, n int) *DeploymentPipeline {
	t.Helper()
	stages := make([]*DeploymentStage, n)
	for i := range stages {
		stages[i] = &DeploymentStage{DisplayName: string(rune('A' + i))}
	}
	pl := &DeploymentPipeline{DisplayName: name}
	if err := s.CreateDeploymentPipeline(pl, stages, Principal{ID: "p", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	return pl
}

func TestDeploymentPipelineRoundTrip(t *testing.T) {
	s := newDeploymentStore(t)
	pl := mkPipeline(t, s, "release", 3)

	got, err := s.GetDeploymentPipeline(pl.ID)
	if err != nil || got.DisplayName != "release" {
		t.Fatalf("get = %+v, %v", got, err)
	}
	if _, err := s.GetDeploymentPipeline("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("get unknown = %v, want ErrNotFound", err)
	}

	pl.DisplayName, pl.Description = "renamed", "d"
	if err := s.UpdateDeploymentPipeline(pl); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetDeploymentPipeline(pl.ID)
	if got.DisplayName != "renamed" || got.Description != "d" {
		t.Fatalf("after update = %+v", got)
	}

	// Visible to its creator, invisible to anyone else.
	ps, err := s.ListDeploymentPipelinesFor("p")
	if err != nil || len(ps) != 1 {
		t.Fatalf("list = %+v, %v", ps, err)
	}
	if ps, _ := s.ListDeploymentPipelinesFor("other"); len(ps) != 0 {
		t.Errorf("list for a non-member = %+v", ps)
	}
	if role, err := s.DeploymentPipelineRole(pl.ID, "p"); err != nil || role != RoleAdmin {
		t.Errorf("creator role = %q, %v; want Admin", role, err)
	}
	if _, err := s.DeploymentPipelineRole(pl.ID, "other"); !errors.Is(err, ErrNotFound) {
		t.Errorf("non-member role = %v, want ErrNotFound", err)
	}
}

func TestDeploymentStageCountBounds(t *testing.T) {
	s := newDeploymentStore(t)
	for _, n := range []int{0, 1, MaxStages + 1} {
		stages := make([]*DeploymentStage, n)
		for i := range stages {
			stages[i] = &DeploymentStage{DisplayName: "s"}
		}
		err := s.CreateDeploymentPipeline(&DeploymentPipeline{DisplayName: "x"}, stages,
			Principal{ID: "p"})
		if !errors.Is(err, ErrStageCount) {
			t.Errorf("%d stages = %v, want ErrStageCount", n, err)
		}
	}
	// The bounds themselves are accepted.
	for _, n := range []int{MinStages, MaxStages} {
		mkPipeline(t, s, string(rune('a'+n)), n)
	}
}

func TestDeploymentStages(t *testing.T) {
	s := newDeploymentStore(t)
	pl := mkPipeline(t, s, "release", 3)

	sts, err := s.ListDeploymentStages(pl.ID)
	if err != nil || len(sts) != 3 {
		t.Fatalf("stages = %+v, %v", sts, err)
	}
	for i, st := range sts {
		if st.Order != i || st.PipelineID != pl.ID {
			t.Errorf("stage %d = %+v", i, st)
		}
	}

	one, err := s.GetDeploymentStage(pl.ID, sts[1].ID)
	if err != nil || one.Order != 1 {
		t.Fatalf("get stage = %+v, %v", one, err)
	}
	if _, err := s.GetDeploymentStage(pl.ID, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown stage = %v, want ErrNotFound", err)
	}
	// A stage id from another pipeline is not reachable through this one.
	other := mkPipeline(t, s, "other", 2)
	if _, err := s.GetDeploymentStage(other.ID, sts[0].ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-pipeline stage = %v, want ErrNotFound", err)
	}

	one.DisplayName, one.IsPublic, one.Description = "Test", true, "t"
	if err := s.UpdateDeploymentStage(one); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetDeploymentStage(pl.ID, one.ID)
	if got.DisplayName != "Test" || !got.IsPublic || got.Description != "t" || got.Order != 1 {
		t.Fatalf("after update = %+v", got)
	}
}

// TestStageWorkspaceAssignment: assignment resolves the workspace's *current*
// name at read time, unassignment clears it, and deleting the workspace
// unassigns the stage through the FK rather than dangling.
func TestStageWorkspaceAssignment(t *testing.T) {
	s := newDeploymentStore(t)
	pl := mkPipeline(t, s, "release", 2)
	sts, _ := s.ListDeploymentStages(pl.ID)

	ws := &Workspace{DisplayName: "dev-ws"}
	if err := s.CreateWorkspace(ws, Principal{ID: "p", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStageWorkspace(pl.ID, sts[0].ID, ws.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetDeploymentStage(pl.ID, sts[0].ID)
	if got.WorkspaceID != ws.ID || got.WorkspaceName != "dev-ws" {
		t.Fatalf("assigned stage = %+v", got)
	}

	// A rename shows through without a write to the stage.
	ws.DisplayName = "renamed-ws"
	if err := s.UpdateWorkspace(ws); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetDeploymentStage(pl.ID, sts[0].ID)
	if got.WorkspaceName != "renamed-ws" {
		t.Fatalf("stage workspace name went stale: %+v", got)
	}

	if err := s.SetStageWorkspace(pl.ID, sts[0].ID, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.GetDeploymentStage(pl.ID, sts[0].ID); got.WorkspaceID != "" {
		t.Fatalf("unassign left %+v", got)
	}

	// Re-assign, then delete the workspace: ON DELETE SET NULL unassigns.
	if err := s.SetStageWorkspace(pl.ID, sts[0].ID, ws.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteWorkspace(ws.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetDeploymentStage(pl.ID, sts[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.WorkspaceID != "" || got.WorkspaceName != "" {
		t.Fatalf("stage still points at a deleted workspace: %+v", got)
	}
}

// mkWorkspaceWith creates a workspace holding one item per name (all
// Notebooks) and returns the workspace plus the items by name.
func mkWorkspaceWith(t *testing.T, s *Store, wsName string, itemNames ...string) (*Workspace, map[string]*Item) {
	t.Helper()
	ws := &Workspace{DisplayName: wsName}
	if err := s.CreateWorkspace(ws, Principal{ID: "p", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	items := map[string]*Item{}
	for _, n := range itemNames {
		item := &Item{WorkspaceID: ws.ID, DisplayName: n, Type: "Notebook"}
		if err := s.CreateItem(item, nil); err != nil {
			t.Fatal(err)
		}
		items[n] = item
	}
	return ws, items
}

// TestAssignStageWorkspacePairsAdjacent: assigning pairs against the adjacent
// stage on both sides, and only on matching (name, type).
func TestAssignStageWorkspacePairsAdjacent(t *testing.T) {
	s := newDeploymentStore(t)
	pl := mkPipeline(t, s, "release", 3)
	sts, _ := s.ListDeploymentStages(pl.ID)

	devWS, dev := mkWorkspaceWith(t, s, "dev", "orders", "dev-only")
	testWS, tst := mkWorkspaceWith(t, s, "test", "orders", "test-only")

	if err := s.AssignStageWorkspace(pl.ID, sts[0].ID, devWS.ID); err != nil {
		t.Fatal(err)
	}
	// Nothing to pair against yet.
	if prs, _ := s.ListItemPairs(pl.ID, sts[0].ID, sts[1].ID); len(prs) != 0 {
		t.Fatalf("pairs before the neighbour exists = %+v", prs)
	}

	if err := s.AssignStageWorkspace(pl.ID, sts[1].ID, testWS.ID); err != nil {
		t.Fatal(err)
	}
	prs, err := s.ListItemPairs(pl.ID, sts[0].ID, sts[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 1 {
		t.Fatalf("pairs = %+v, want 1", prs)
	}
	if prs[0].EarlierItemID != dev["orders"].ID || prs[0].LaterItemID != tst["orders"].ID {
		t.Fatalf("paired the wrong items: %+v", prs[0])
	}
}

// TestAssignStageWorkspaceOnlyPairsAdjacentStages: stage 0 and stage 2 are
// not adjacent, so assigning them leaves no edge — deploys are adjacent-only.
func TestAssignStageWorkspaceOnlyPairsAdjacentStages(t *testing.T) {
	s := newDeploymentStore(t)
	pl := mkPipeline(t, s, "release", 3)
	sts, _ := s.ListDeploymentStages(pl.ID)

	devWS, _ := mkWorkspaceWith(t, s, "dev", "orders")
	prodWS, _ := mkWorkspaceWith(t, s, "prod", "orders")
	if err := s.AssignStageWorkspace(pl.ID, sts[0].ID, devWS.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.AssignStageWorkspace(pl.ID, sts[2].ID, prodWS.ID); err != nil {
		t.Fatal(err)
	}
	if prs, _ := s.ListItemPairs(pl.ID, sts[0].ID, sts[2].ID); len(prs) != 0 {
		t.Fatalf("non-adjacent stages paired: %+v", prs)
	}
}

// TestPairsSurviveRename is THE regression for docs/23: pairs are item-id
// edges, so renaming either side must not unpair them. A name-matching
// implementation passes every other test here and fails this one.
func TestPairsSurviveRename(t *testing.T) {
	s := newDeploymentStore(t)
	pl := mkPipeline(t, s, "release", 2)
	sts, _ := s.ListDeploymentStages(pl.ID)

	devWS, dev := mkWorkspaceWith(t, s, "dev", "orders")
	testWS, tst := mkWorkspaceWith(t, s, "test", "orders")
	if err := s.AssignStageWorkspace(pl.ID, sts[0].ID, devWS.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.AssignStageWorkspace(pl.ID, sts[1].ID, testWS.ID); err != nil {
		t.Fatal(err)
	}

	// Rename BOTH sides to names that no longer match each other at all.
	dev["orders"].DisplayName = "orders-renamed-source"
	if err := s.UpdateItem(dev["orders"]); err != nil {
		t.Fatal(err)
	}
	tst["orders"].DisplayName = "orders-renamed-target"
	if err := s.UpdateItem(tst["orders"]); err != nil {
		t.Fatal(err)
	}

	prs, err := s.ListItemPairs(pl.ID, sts[0].ID, sts[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 1 {
		t.Fatalf("rename unpaired the items: %+v", prs)
	}
	if prs[0].EarlierItemID != dev["orders"].ID || prs[0].LaterItemID != tst["orders"].ID {
		t.Fatalf("pair points at the wrong items after rename: %+v", prs[0])
	}
}

// TestItemsAddedAfterAssignAreNotPaired: "Items added after the workspace is
// assigned to a pipeline aren't automatically paired" — pairing happens at
// assign (and deploy), never lazily at read time.
func TestItemsAddedAfterAssignAreNotPaired(t *testing.T) {
	s := newDeploymentStore(t)
	pl := mkPipeline(t, s, "release", 2)
	sts, _ := s.ListDeploymentStages(pl.ID)

	devWS, _ := mkWorkspaceWith(t, s, "dev")
	testWS, _ := mkWorkspaceWith(t, s, "test")
	if err := s.AssignStageWorkspace(pl.ID, sts[0].ID, devWS.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.AssignStageWorkspace(pl.ID, sts[1].ID, testWS.ID); err != nil {
		t.Fatal(err)
	}
	for _, ws := range []*Workspace{devWS, testWS} {
		if err := s.CreateItem(&Item{WorkspaceID: ws.ID, DisplayName: "late", Type: "Notebook"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if prs, _ := s.ListItemPairs(pl.ID, sts[0].ID, sts[1].ID); len(prs) != 0 {
		t.Fatalf("identically-named items added after assign were paired: %+v", prs)
	}
}

// TestReassignDropsStalePairs: a stage's pairs describe its current
// workspace's items, so re-assigning replaces them wholesale.
func TestReassignDropsStalePairs(t *testing.T) {
	s := newDeploymentStore(t)
	pl := mkPipeline(t, s, "release", 2)
	sts, _ := s.ListDeploymentStages(pl.ID)

	devWS, _ := mkWorkspaceWith(t, s, "dev", "orders")
	testWS, _ := mkWorkspaceWith(t, s, "test", "orders")
	otherWS, _ := mkWorkspaceWith(t, s, "test2", "unrelated")
	if err := s.AssignStageWorkspace(pl.ID, sts[0].ID, devWS.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.AssignStageWorkspace(pl.ID, sts[1].ID, testWS.ID); err != nil {
		t.Fatal(err)
	}
	if prs, _ := s.ListItemPairs(pl.ID, sts[0].ID, sts[1].ID); len(prs) != 1 {
		t.Fatalf("setup pairs = %+v", prs)
	}
	// Point stage 1 at a workspace with nothing in common.
	if err := s.AssignStageWorkspace(pl.ID, sts[1].ID, otherWS.ID); err != nil {
		t.Fatal(err)
	}
	if prs, _ := s.ListItemPairs(pl.ID, sts[0].ID, sts[1].ID); len(prs) != 0 {
		t.Fatalf("stale pairs survived re-assignment: %+v", prs)
	}
}

// TestUnassignDropsPairs: unassigning clears both the column and the edges.
func TestUnassignDropsPairs(t *testing.T) {
	s := newDeploymentStore(t)
	pl := mkPipeline(t, s, "release", 2)
	sts, _ := s.ListDeploymentStages(pl.ID)

	devWS, _ := mkWorkspaceWith(t, s, "dev", "orders")
	testWS, _ := mkWorkspaceWith(t, s, "test", "orders")
	if err := s.AssignStageWorkspace(pl.ID, sts[0].ID, devWS.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.AssignStageWorkspace(pl.ID, sts[1].ID, testWS.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.UnassignStageWorkspace(pl.ID, sts[1].ID); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetDeploymentStage(pl.ID, sts[1].ID)
	if got.WorkspaceID != "" {
		t.Fatalf("unassign left the workspace: %+v", got)
	}
	if prs, _ := s.ListItemPairs(pl.ID, sts[0].ID, sts[1].ID); len(prs) != 0 {
		t.Fatalf("pairs survived unassign: %+v", prs)
	}
	if err := s.UnassignStageWorkspace(pl.ID, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unassign unknown stage = %v, want ErrNotFound", err)
	}
}

// TestDeletingAnItemDropsItsPair: the pair FK cascades, so a deleted item
// leaves no edge behind.
func TestDeletingAnItemDropsItsPair(t *testing.T) {
	s := newDeploymentStore(t)
	pl := mkPipeline(t, s, "release", 2)
	sts, _ := s.ListDeploymentStages(pl.ID)

	devWS, dev := mkWorkspaceWith(t, s, "dev", "orders")
	testWS, _ := mkWorkspaceWith(t, s, "test", "orders")
	if err := s.AssignStageWorkspace(pl.ID, sts[0].ID, devWS.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.AssignStageWorkspace(pl.ID, sts[1].ID, testWS.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteItem(devWS.ID, dev["orders"].ID); err != nil {
		t.Fatal(err)
	}
	if prs, _ := s.ListItemPairs(pl.ID, sts[0].ID, sts[1].ID); len(prs) != 0 {
		t.Fatalf("pair survived item delete: %+v", prs)
	}
}

func TestAssignStageWorkspaceNotFound(t *testing.T) {
	s := newDeploymentStore(t)
	pl := mkPipeline(t, s, "release", 2)
	sts, _ := s.ListDeploymentStages(pl.ID)
	ws, _ := mkWorkspaceWith(t, s, "dev")

	if err := s.AssignStageWorkspace(pl.ID, "nope", ws.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown stage = %v, want ErrNotFound", err)
	}
	if err := s.AssignStageWorkspace(pl.ID, sts[0].ID, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown workspace = %v, want ErrNotFound", err)
	}
}

// TestDeploymentPipelineRoles: grants are keyed by principal, so re-granting
// replaces rather than duplicating, and a revoked principal loses visibility.
func TestDeploymentPipelineRoles(t *testing.T) {
	s := newDeploymentStore(t)
	pl := mkPipeline(t, s, "release", 2)

	ras, err := s.ListDeploymentPipelineRoles(pl.ID)
	if err != nil || len(ras) != 1 || ras[0].Principal.ID != "p" || ras[0].Role != RoleAdmin {
		t.Fatalf("creator assignment = %+v, %v", ras, err)
	}

	bob := Principal{ID: "bob", Type: "ServicePrincipal"}
	if err := s.AddDeploymentPipelineRole(pl.ID, bob, RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if ps, _ := s.ListDeploymentPipelinesFor("bob"); len(ps) != 1 {
		t.Fatalf("bob cannot see the pipeline he was granted: %+v", ps)
	}

	// Re-granting the same principal replaces, never duplicates.
	if err := s.AddDeploymentPipelineRole(pl.ID, Principal{ID: "bob", Type: "User"}, RoleAdmin); err != nil {
		t.Fatal(err)
	}
	ras, _ = s.ListDeploymentPipelineRoles(pl.ID)
	if len(ras) != 2 {
		t.Fatalf("re-grant duplicated: %+v", ras)
	}
	for _, ra := range ras {
		if ra.Principal.ID == "bob" && ra.Principal.Type != "User" {
			t.Errorf("re-grant did not update the principal type: %+v", ra)
		}
	}

	if err := s.DeleteDeploymentPipelineRole(pl.ID, "bob"); err != nil {
		t.Fatal(err)
	}
	if ps, _ := s.ListDeploymentPipelinesFor("bob"); len(ps) != 0 {
		t.Fatalf("bob still sees the pipeline after revocation: %+v", ps)
	}
	if err := s.DeleteDeploymentPipelineRole(pl.ID, "bob"); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoking twice = %v, want ErrNotFound", err)
	}
}

func TestDeleteDeploymentPipelineCascadesStore(t *testing.T) {
	s := newDeploymentStore(t)
	pl := mkPipeline(t, s, "release", 3)
	if err := s.DeleteDeploymentPipeline(pl.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetDeploymentPipeline(pl.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("pipeline survived = %v", err)
	}
	if sts, _ := s.ListDeploymentStages(pl.ID); len(sts) != 0 {
		t.Errorf("stages survived: %+v", sts)
	}
	if _, err := s.DeploymentPipelineRole(pl.ID, "p"); !errors.Is(err, ErrNotFound) {
		t.Errorf("role survived = %v", err)
	}
}

// TestDeploymentClosedDBErrors drives every deployment repository method over
// a closed database.
func TestDeploymentClosedDBErrors(t *testing.T) {
	s, err := Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	pl := mkPipeline(t, s, "release", 2)
	sts, _ := s.ListDeploymentStages(pl.ID)
	stageID := sts[0].ID
	s.Close()

	two := []*DeploymentStage{{DisplayName: "a"}, {DisplayName: "b"}}
	if err := s.CreateDeploymentPipeline(&DeploymentPipeline{DisplayName: "x"}, two, Principal{ID: "p"}); err == nil {
		t.Error("CreateDeploymentPipeline on closed DB succeeded")
	}
	if _, err := s.GetDeploymentPipeline(pl.ID); err == nil {
		t.Error("GetDeploymentPipeline on closed DB succeeded")
	}
	if _, err := s.ListDeploymentPipelinesFor("p"); err == nil {
		t.Error("ListDeploymentPipelinesFor on closed DB succeeded")
	}
	if err := s.UpdateDeploymentPipeline(pl); err == nil {
		t.Error("UpdateDeploymentPipeline on closed DB succeeded")
	}
	if err := s.DeleteDeploymentPipeline(pl.ID); err == nil {
		t.Error("DeleteDeploymentPipeline on closed DB succeeded")
	}
	if _, err := s.ListDeploymentStages(pl.ID); err == nil {
		t.Error("ListDeploymentStages on closed DB succeeded")
	}
	if _, err := s.GetDeploymentStage(pl.ID, stageID); err == nil {
		t.Error("GetDeploymentStage on closed DB succeeded")
	}
	if err := s.UpdateDeploymentStage(&DeploymentStage{PipelineID: pl.ID, ID: stageID}); err == nil {
		t.Error("UpdateDeploymentStage on closed DB succeeded")
	}
	if err := s.SetStageWorkspace(pl.ID, stageID, "w"); err == nil {
		t.Error("SetStageWorkspace on closed DB succeeded")
	}
	if _, err := s.DeploymentPipelineRole(pl.ID, "p"); err == nil {
		t.Error("DeploymentPipelineRole on closed DB succeeded")
	}
	if err := s.AssignStageWorkspace(pl.ID, stageID, "w"); err == nil {
		t.Error("AssignStageWorkspace on closed DB succeeded")
	}
	if err := s.UnassignStageWorkspace(pl.ID, stageID); err == nil {
		t.Error("UnassignStageWorkspace on closed DB succeeded")
	}
	if _, err := s.ListItemPairs(pl.ID, stageID, stageID); err == nil {
		t.Error("ListItemPairs on closed DB succeeded")
	}
	if err := s.AddDeploymentPipelineRole(pl.ID, Principal{ID: "x"}, RoleAdmin); err == nil {
		t.Error("AddDeploymentPipelineRole on closed DB succeeded")
	}
	if err := s.DeleteDeploymentPipelineRole(pl.ID, "x"); err == nil {
		t.Error("DeleteDeploymentPipelineRole on closed DB succeeded")
	}
	if _, err := s.ListDeploymentPipelineRoles(pl.ID); err == nil {
		t.Error("ListDeploymentPipelineRoles on closed DB succeeded")
	}
}

// TestAssignStageWorkspaceRepairsAfterWorkspaceDelete: deleting a paired
// stage's workspace must leave the pipeline usable — the stage unassigns, the
// pairs go with the deleted items, and re-assigning still works.
func TestAssignStageWorkspaceRepairsAfterWorkspaceDelete(t *testing.T) {
	s := newDeploymentStore(t)
	pl := mkPipeline(t, s, "release", 2)
	sts, _ := s.ListDeploymentStages(pl.ID)

	devWS, _ := mkWorkspaceWith(t, s, "dev", "orders")
	testWS, _ := mkWorkspaceWith(t, s, "test", "orders")
	if err := s.AssignStageWorkspace(pl.ID, sts[0].ID, devWS.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.AssignStageWorkspace(pl.ID, sts[1].ID, testWS.ID); err != nil {
		t.Fatal(err)
	}
	// Deleting the neighbour's workspace unassigns that stage (FK) and takes
	// its items — so the pair goes with them, and re-assigning the survivor
	// finds nothing to pair against rather than erroring.
	if err := s.DeleteWorkspace(testWS.ID); err != nil {
		t.Fatal(err)
	}
	if prs, _ := s.ListItemPairs(pl.ID, sts[0].ID, sts[1].ID); len(prs) != 0 {
		t.Fatalf("pairs survived the target workspace's deletion: %+v", prs)
	}
	if err := s.AssignStageWorkspace(pl.ID, sts[0].ID, devWS.ID); err != nil {
		t.Fatalf("re-assign after neighbour deletion: %v", err)
	}
}
