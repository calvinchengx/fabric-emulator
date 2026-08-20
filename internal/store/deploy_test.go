package store

// Deploy Stage Content, store side (docs/23 D2). The three rules under test
// are the ones the design doc calls out as easiest to get wrong: metadata
// only, not a mirror, and pairs decide rather than names.

import (
	"errors"
	"testing"
)

// deployFixture builds a 3-stage pipeline with dev and test assigned.
type deployFixture struct {
	s        *Store
	pl       *DeploymentPipeline
	stages   []*DeploymentStage
	dev, tst *Workspace
}

func newDeployFixture(t *testing.T, devItems, testItems []string) *deployFixture {
	t.Helper()
	s := newDeploymentStore(t)
	pl := mkPipeline(t, s, "release", 3)
	sts, _ := s.ListDeploymentStages(pl.ID)
	dev, _ := mkWorkspaceWith(t, s, "dev", devItems...)
	tst, _ := mkWorkspaceWith(t, s, "test", testItems...)
	if err := s.AssignStageWorkspace(pl.ID, sts[0].ID, dev.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.AssignStageWorkspace(pl.ID, sts[1].ID, tst.ID); err != nil {
		t.Fatal(err)
	}
	return &deployFixture{s: s, pl: pl, stages: sts, dev: dev, tst: tst}
}

func (f *deployFixture) deploy(t *testing.T, selected ...ItemSelector) *DeploymentOperation {
	t.Helper()
	op, err := f.s.DeployStageContent(f.pl.ID, f.stages[0].ID, f.stages[1].ID,
		NewID(), "note", "alice", selected)
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	return op
}

func (f *deployFixture) itemNames(t *testing.T, ws *Workspace) []string {
	t.Helper()
	items, err := f.s.ListItems(ws.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.DisplayName)
	}
	return out
}

// TestDeployCleanCreatesAndPairs: an unpaired source item is created in the
// target and the copy is paired, so the NEXT deploy updates instead of
// duplicating.
func TestDeployCleanCreatesAndPairs(t *testing.T) {
	f := newDeployFixture(t, []string{"orders"}, nil)

	op := f.deploy(t)
	if len(op.Items) != 1 || op.Items[0].Outcome != DeployCreated {
		t.Fatalf("first deploy = %+v", op.Items)
	}
	if op.Items[0].TargetItemID == "" || op.Items[0].TargetItemID == op.Items[0].SourceItemID {
		t.Fatalf("target must be a NEW item with its own id: %+v", op.Items[0])
	}

	// Deploying again updates the pair rather than creating a second copy.
	op2 := f.deploy(t)
	if len(op2.Items) != 1 || op2.Items[0].Outcome != DeployUpdated {
		t.Fatalf("second deploy = %+v", op2.Items)
	}
	if op2.Items[0].TargetItemID != op.Items[0].TargetItemID {
		t.Fatalf("second deploy retargeted: %+v vs %+v", op2.Items[0], op.Items[0])
	}
	if got := f.itemNames(t, f.tst); len(got) != 1 {
		t.Fatalf("target items after two deploys = %v, want exactly one", got)
	}
}

// TestDeployIsNotAMirror: items in the target but not the source survive.
// updateFromGit deletes stale items; deploy must not.
func TestDeployIsNotAMirror(t *testing.T) {
	f := newDeployFixture(t, []string{"orders"}, []string{"target-only"})
	f.deploy(t)
	got := f.itemNames(t, f.tst)
	if len(got) != 2 {
		t.Fatalf("target items = %v, want the deployed item AND target-only", got)
	}
	var found bool
	for _, n := range got {
		if n == "target-only" {
			found = true
		}
	}
	if !found {
		t.Fatalf("deploy deleted a target-only item: %v", got)
	}
}

// TestDeployCopiesDefinitionNotData: the definition rides along; OneLake
// bytes do not. A deployed lakehouse arrives empty.
func TestDeployCopiesDefinitionNotData(t *testing.T) {
	s := newDeploymentStore(t)
	pl := mkPipeline(t, s, "release", 2)
	sts, _ := s.ListDeploymentStages(pl.ID)
	dev, _ := mkWorkspaceWith(t, s, "dev")
	tst, _ := mkWorkspaceWith(t, s, "test")

	src := &Item{WorkspaceID: dev.ID, DisplayName: "lake", Type: "Lakehouse", Description: "d"}
	parts := []DefinitionPart{{Path: "a.py", Payload: "cGFzcw==", PayloadType: "InlineBase64"}}
	if err := s.CreateItem(src, parts); err != nil {
		t.Fatal(err)
	}
	// Real bytes in the source item's OneLake area.
	if err := s.CreateOneLakePath(&OneLakePath{
		WorkspaceID: dev.ID, ItemID: src.ID,
		RelPath: "Tables/t/part.parquet", Content: []byte("DATA"),
	}, false); err != nil {
		t.Fatal(err)
	}
	if err := s.AssignStageWorkspace(pl.ID, sts[0].ID, dev.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.AssignStageWorkspace(pl.ID, sts[1].ID, tst.ID); err != nil {
		t.Fatal(err)
	}

	op, err := s.DeployStageContent(pl.ID, sts[0].ID, sts[1].ID, NewID(), "", "alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetDefinition(op.Items[0].TargetItemID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != "a.py" || got[0].Payload != "cGFzcw==" {
		t.Fatalf("definition did not survive deploy: %+v", got)
	}
	// The deployed item's OneLake area is EMPTY — metadata only. A deployed
	// lakehouse arrives with no data, which is the documented behaviour and
	// the thing an emulator is most tempted to "helpfully" get wrong.
	paths, err := s.ListOneLakePaths(op.Items[0].TargetItemID, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 0 {
		t.Fatalf("deploy copied OneLake data (%d paths); it must copy metadata only", len(paths))
	}
	// …and the source's data is still there, untouched.
	srcPaths, err := s.ListOneLakePaths(src.ID, "", true)
	if err != nil || len(srcPaths) != 1 {
		t.Fatalf("source OneLake data disturbed: %d paths, %v", len(srcPaths), err)
	}
}

// TestDeployOverwritesPairedItemDespiteRename is the D2 half of the pairing
// regression: the pair decides, not the name. A renamed target is updated in
// place and KEEPS its own name (docs/23 Q2).
func TestDeployOverwritesPairedItemDespiteRename(t *testing.T) {
	f := newDeployFixture(t, []string{"orders"}, nil)
	op := f.deploy(t)
	targetID := op.Items[0].TargetItemID

	tgt, err := f.s.GetItemByID(targetID)
	if err != nil {
		t.Fatal(err)
	}
	tgt.DisplayName = "orders-in-test"
	if err := f.s.UpdateItem(tgt); err != nil {
		t.Fatal(err)
	}

	op2 := f.deploy(t)
	if op2.Items[0].Outcome != DeployUpdated || op2.Items[0].TargetItemID != targetID {
		t.Fatalf("rename broke the pair: %+v", op2.Items[0])
	}
	after, _ := f.s.GetItemByID(targetID)
	if after.DisplayName != "orders-in-test" {
		t.Fatalf("deploy overwrote the target's display name: %q", after.DisplayName)
	}
	if got := f.itemNames(t, f.tst); len(got) != 1 {
		t.Fatalf("rename caused a duplicate: %v", got)
	}
}

// TestDeploySelective: naming items deploys only those.
func TestDeploySelective(t *testing.T) {
	f := newDeployFixture(t, []string{"orders", "sales"}, nil)
	items, _ := f.s.ListItems(f.dev.ID, "")
	var ordersID string
	for _, it := range items {
		if it.DisplayName == "orders" {
			ordersID = it.ID
		}
	}
	op := f.deploy(t, ItemSelector{SourceItemID: ordersID, ItemType: "Notebook"})
	if len(op.Items) != 1 || op.Items[0].DisplayName != "orders" {
		t.Fatalf("selective deploy = %+v", op.Items)
	}
	if got := f.itemNames(t, f.tst); len(got) != 1 || got[0] != "orders" {
		t.Fatalf("target items = %v, want only orders", got)
	}
}

// TestDeployUnpairedNameCollisionFailsLoudly: an unpaired same-named item in
// the target makes the clean deploy collide with the uniqueness index. The
// deploy must fail, not silently skip or rename (docs/23 Q1).
func TestDeployUnpairedNameCollisionFailsLoudly(t *testing.T) {
	s := newDeploymentStore(t)
	pl := mkPipeline(t, s, "release", 2)
	sts, _ := s.ListDeploymentStages(pl.ID)
	dev, _ := mkWorkspaceWith(t, s, "dev", "orders")
	tst, _ := mkWorkspaceWith(t, s, "test")

	// Assign FIRST (nothing to pair), then add a colliding name to the target
	// — items added after assignment are not paired.
	if err := s.AssignStageWorkspace(pl.ID, sts[0].ID, dev.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.AssignStageWorkspace(pl.ID, sts[1].ID, tst.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateItem(&Item{WorkspaceID: tst.ID, DisplayName: "orders", Type: "Notebook"}, nil); err != nil {
		t.Fatal(err)
	}

	_, err := s.DeployStageContent(pl.ID, sts[0].ID, sts[1].ID, NewID(), "", "alice", nil)
	if !errors.Is(err, ErrNameConflict) {
		t.Fatalf("collision = %v, want ErrNameConflict", err)
	}
}

// TestDeployRejectsNonAdjacentAndUnassigned.
func TestDeployRejectsNonAdjacentAndUnassigned(t *testing.T) {
	f := newDeployFixture(t, []string{"orders"}, nil)

	// stage 0 -> stage 2 is not adjacent.
	_, err := f.s.DeployStageContent(f.pl.ID, f.stages[0].ID, f.stages[2].ID, NewID(), "", "a", nil)
	if !errors.Is(err, ErrStagesNotAdjacent) {
		t.Errorf("non-adjacent = %v, want ErrStagesNotAdjacent", err)
	}
	// stage 1 -> stage 2: stage 2 has no workspace.
	_, err = f.s.DeployStageContent(f.pl.ID, f.stages[1].ID, f.stages[2].ID, NewID(), "", "a", nil)
	if !errors.Is(err, ErrStageUnassigned) {
		t.Errorf("unassigned target = %v, want ErrStageUnassigned", err)
	}
	// Unknown stage ids.
	_, err = f.s.DeployStageContent(f.pl.ID, "nope", f.stages[1].ID, NewID(), "", "a", nil)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown source = %v, want ErrNotFound", err)
	}
	_, err = f.s.DeployStageContent(f.pl.ID, f.stages[0].ID, "nope", NewID(), "", "a", nil)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown target = %v, want ErrNotFound", err)
	}
}

// TestDeployBackwards: promotion also runs later -> earlier, reading the
// stored pairs in the other direction.
func TestDeployBackwards(t *testing.T) {
	f := newDeployFixture(t, []string{"orders"}, nil)
	f.deploy(t) // dev -> test, creates and pairs

	// Now test -> dev must UPDATE the existing dev item via the same pair.
	op, err := f.s.DeployStageContent(f.pl.ID, f.stages[1].ID, f.stages[0].ID,
		NewID(), "", "alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(op.Items) != 1 || op.Items[0].Outcome != DeployUpdated {
		t.Fatalf("backward deploy = %+v", op.Items)
	}
	if got := f.itemNames(t, f.dev); len(got) != 1 {
		t.Fatalf("backward deploy duplicated in the source stage: %v", got)
	}
}

// TestDeploymentOperationsRecorded: each deploy is retrievable by id and
// listed newest-first.
func TestDeploymentOperationsRecorded(t *testing.T) {
	f := newDeployFixture(t, []string{"orders"}, nil)
	first := f.deploy(t)
	second := f.deploy(t)

	got, err := f.s.GetDeploymentOperation(f.pl.ID, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Note != "note" || got.PerformedBy != "alice" || len(got.Items) != 1 {
		t.Fatalf("recorded operation = %+v", got)
	}
	if got.Items[0].Outcome != DeployCreated {
		t.Fatalf("detail did not round-trip: %+v", got.Items)
	}
	if _, err := f.s.GetDeploymentOperation(f.pl.ID, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown operation = %v, want ErrNotFound", err)
	}
	byID, err := f.s.GetDeploymentOperationByID(second.ID)
	if err != nil || byID.ID != second.ID {
		t.Fatalf("by-id lookup = %+v, %v", byID, err)
	}
	if _, err := f.s.GetDeploymentOperationByID("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown by-id = %v, want ErrNotFound", err)
	}

	ops, err := f.s.ListDeploymentOperations(f.pl.ID)
	if err != nil || len(ops) != 2 {
		t.Fatalf("list = %+v, %v", ops, err)
	}
	if ops[0].ID != second.ID {
		t.Errorf("list is not newest-first: %v then %v", ops[0].ID, ops[1].ID)
	}
}

// TestDeployAfterTargetItemDeleted: deleting the target item cascades its
// pair away, so the next deploy cleanly re-creates and re-pairs rather than
// erroring or leaving the stage permanently broken.
func TestDeployAfterTargetItemDeleted(t *testing.T) {
	f := newDeployFixture(t, []string{"orders"}, nil)
	first := f.deploy(t)
	targetID := first.Items[0].TargetItemID

	if err := f.s.DeleteItem(f.tst.ID, targetID); err != nil {
		t.Fatal(err)
	}
	second := f.deploy(t)
	if second.Items[0].Outcome != DeployCreated {
		t.Fatalf("re-deploy after target deletion = %+v", second.Items[0])
	}
	if second.Items[0].TargetItemID == targetID {
		t.Fatal("re-deploy reused the deleted item's id")
	}
	if got := f.itemNames(t, f.tst); len(got) != 1 || got[0] != "orders" {
		t.Fatalf("target items = %v", got)
	}
	// The new copy is paired, so a third deploy updates rather than duplicating.
	if third := f.deploy(t); third.Items[0].Outcome != DeployUpdated {
		t.Fatalf("third deploy = %+v", third.Items[0])
	}
}

// TestDeployEmptySourceStage: nothing to promote is a recorded no-op, not an
// error.
func TestDeployEmptySourceStage(t *testing.T) {
	f := newDeployFixture(t, nil, []string{"target-only"})
	op := f.deploy(t)
	if len(op.Items) != 0 {
		t.Fatalf("empty source deployed %+v", op.Items)
	}
	if got := f.itemNames(t, f.tst); len(got) != 1 || got[0] != "target-only" {
		t.Fatalf("target disturbed by an empty deploy: %v", got)
	}
	// Still recorded, so the history shows the attempt.
	ops, err := f.s.ListDeploymentOperations(f.pl.ID)
	if err != nil || len(ops) != 1 || len(ops[0].Items) != 0 {
		t.Fatalf("empty deploy not recorded: %+v, %v", ops, err)
	}
}

func TestDeployClosedDBErrors(t *testing.T) {
	f := newDeployFixture(t, []string{"orders"}, nil)
	pl, sts := f.pl, f.stages
	_ = f.s.Close()

	if _, err := f.s.DeployStageContent(pl.ID, sts[0].ID, sts[1].ID, NewID(), "", "a", nil); err == nil {
		t.Error("DeployStageContent on closed DB succeeded")
	}
	if _, err := f.s.GetDeploymentOperation(pl.ID, "x"); err == nil {
		t.Error("GetDeploymentOperation on closed DB succeeded")
	}
	if _, err := f.s.GetDeploymentOperationByID("x"); err == nil {
		t.Error("GetDeploymentOperationByID on closed DB succeeded")
	}
	if _, err := f.s.ListDeploymentOperations(pl.ID); err == nil {
		t.Error("ListDeploymentOperations on closed DB succeeded")
	}
}

// TestDeployRecreatesAnItemDeletedFromTheTarget: deleting a deployed item and
// deploying again puts it back, as a fresh create rather than an error.
//
// NOTE ON WHAT THIS DOES *NOT* COVER, because the first version of this test
// claimed otherwise. `DeployStageContent` has an `ErrNotFound` arm for a pair
// row that outlived its item, and this test does not reach it: deleting the
// item CASCADES the pair away (`deployment_pipeline_pairs.later_item_id
// REFERENCES items(id) ON DELETE CASCADE`), so the row is gone before deploy
// looks. Removing that arm does not fail this test — mutation showed exactly
// that. It is defensive code against a state the schema forbids, in the same
// class as ErrPairingAmbiguous, and forcing a test through it would prove
// nothing about the store.
//
// What it does cover is the outcome a user sees, which is worth pinning on its
// own: the item comes back, with a new id, and the target holds exactly one.
func TestDeployRecreatesAnItemDeletedFromTheTarget(t *testing.T) {
	f := newDeployFixture(t, []string{"nb"}, nil)

	// First deploy pairs dev/nb to a fresh copy in test.
	op := f.deploy(t)
	if len(op.Items) != 1 || op.Items[0].Outcome != DeployCreated {
		t.Fatalf("first deploy = %+v; want one created item", op.Items)
	}
	targetID := op.Items[0].TargetItemID

	// Delete the target, leaving the pair row pointing at nothing.
	if err := f.s.DeleteItem(f.tst.ID, targetID); err != nil {
		t.Fatal(err)
	}

	// The second deploy must CREATE rather than fail.
	op2 := f.deploy(t)
	if len(op2.Items) != 1 {
		t.Fatalf("second deploy = %+v; want one item", op2.Items)
	}
	if op2.Items[0].Outcome != DeployCreated {
		t.Errorf("outcome = %v; want %v — a pair whose item is gone is unpaired",
			op2.Items[0].Outcome, DeployCreated)
	}
	if op2.Items[0].TargetItemID == targetID {
		t.Error("the new item reused the deleted id")
	}
	if got := f.itemNames(t, f.tst); len(got) != 1 || got[0] != "nb" {
		t.Errorf("target workspace = %v; want exactly one nb", got)
	}
}

// TestPairingIsComputedEarlierToLaterWhicheverStageIsAssignedSecond.
//
// Pairs are directional: earlier stage first. AssignStageWorkspace has to
// order the two stages itself, because the caller may assign them in either
// order — and it is the SECOND assignment that computes the pairs.
//
// Assigning dev-then-test exercises one side of that swap; nothing exercised
// the other. If the swap were dropped, assigning in the reverse order would
// build every pair backwards, and a deploy would then treat the later stage as
// the source.
func TestPairingIsComputedEarlierToLaterWhicheverStageIsAssignedSecond(t *testing.T) {
	forward := deployedNamesAssigning(t, false)
	reverse := deployedNamesAssigning(t, true)
	if forward != reverse {
		t.Errorf("assignment order changed the deployment: dev-first produced %q, "+
			"test-first produced %q — pairs must be earlier->later either way",
			forward, reverse)
	}
	if forward != "nb:"+string(DeployUpdated) {
		t.Errorf("deploy outcome = %q; want the paired item to be UPDATED", forward)
	}
}

// deployedNamesAssigning builds the same pipeline, assigning the two stages in
// the given order, then deploys and reports what happened to the one item.
func deployedNamesAssigning(t *testing.T, testStageFirst bool) string {
	t.Helper()
	s := newDeploymentStore(t)
	pl := mkPipeline(t, s, "release", 3)
	sts, _ := s.ListDeploymentStages(pl.ID)
	dev, _ := mkWorkspaceWith(t, s, "dev", "nb")
	tst, _ := mkWorkspaceWith(t, s, "test", "nb")

	assign := [][2]string{{sts[0].ID, dev.ID}, {sts[1].ID, tst.ID}}
	if testStageFirst {
		assign[0], assign[1] = assign[1], assign[0]
	}
	for _, a := range assign {
		if err := s.AssignStageWorkspace(pl.ID, a[0], a[1]); err != nil {
			t.Fatalf("assign %v: %v", a, err)
		}
	}

	op, err := s.DeployStageContent(pl.ID, sts[0].ID, sts[1].ID, NewID(), "n", "alice", nil)
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if len(op.Items) != 1 {
		t.Fatalf("deploy = %+v; want one item", op.Items)
	}
	return op.Items[0].DisplayName + ":" + string(op.Items[0].Outcome)
}
