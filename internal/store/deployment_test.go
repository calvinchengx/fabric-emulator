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
}
