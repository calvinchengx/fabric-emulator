package store

import (
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/clock"
)

func TestLineageEdgeLifecycleAndIdempotency(t *testing.T) {
	s, err := Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ws := &Workspace{DisplayName: "w"}
	if err := s.CreateWorkspace(ws, Principal{ID: "owner", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	pipeline := &Item{WorkspaceID: ws.ID, Type: "DataPipeline", DisplayName: "p"}
	src := &Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "src"}
	dst := &Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "dst"}
	for _, it := range []*Item{pipeline, src, dst} {
		if err := s.CreateItem(it, nil); err != nil {
			t.Fatal(err)
		}
	}
	job := &JobInstance{ItemID: pipeline.ID, JobType: "Pipeline"}
	if err := s.CreateJobInstance(job); err != nil {
		t.Fatal(err)
	}
	edge := &LineageEdge{WorkspaceID: ws.ID, JobID: job.ID, ActivityName: "copy", SourceWorkspaceID: ws.ID, SourceItemID: src.ID, SourcePath: "Tables/a", TargetWorkspaceID: ws.ID, TargetItemID: dst.ID, TargetPath: "Tables/b"}
	if err := s.CreateLineageEdge(edge); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateLineageEdge(edge); err != nil {
		t.Fatal(err)
	}
	edges, err := s.ListLineageEdges(ws.ID)
	if err != nil || len(edges) != 1 {
		t.Fatalf("edges=%+v err=%v", edges, err)
	}
	if got, err := s.GetLineageEdge(edge.ID); err != nil || got.SourcePath != "Tables/a" {
		t.Fatalf("get=%+v err=%v", got, err)
	}
	if _, err := s.GetLineageEdge("missing"); err != ErrNotFound {
		t.Fatalf("missing err=%v", err)
	}
}
