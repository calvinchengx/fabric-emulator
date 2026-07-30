package store

import (
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/clock"
)

func TestListAllWorkspaces(t *testing.T) {
	s, err := Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	all, err := s.ListAllWorkspaces()
	if err != nil || len(all) != 0 {
		t.Fatalf("empty store: %v %d", err, len(all))
	}

	for _, name := range []string{"one", "two"} {
		w := &Workspace{ID: "ws-" + name, DisplayName: name, Type: "Workspace"}
		if err := s.CreateWorkspace(w, Principal{ID: "p1", Type: "User"}); err != nil {
			t.Fatal(err)
		}
	}
	all, err = s.ListAllWorkspaces()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("want 2, got %d", len(all))
	}
	for _, w := range all {
		if w.Type != "Workspace" || w.DisplayName == "" {
			t.Fatalf("bad row: %+v", w)
		}
	}
}

func TestListJobInstances(t *testing.T) {
	ck := clock.New()
	s, err := Open("", ck)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	jobs, err := s.ListJobInstances(0)
	if err != nil || len(jobs) != 0 {
		t.Fatalf("empty store: %v %d", err, len(jobs))
	}

	if err := s.CreateWorkspace(&Workspace{ID: "ws-1", DisplayName: "jobs-ws", Type: "Workspace"}, Principal{ID: "p1", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateItem(&Item{ID: "it-1", WorkspaceID: "ws-1", Type: "Notebook", DisplayName: "nb"}, nil); err != nil {
		t.Fatal(err)
	}

	ck.Freeze()
	for _, id := range []string{"job-a", "job-b", "job-c"} {
		j := &JobInstance{ID: id, ItemID: "it-1", JobType: "RunNotebook"}
		if err := s.CreateJobInstance(j); err != nil {
			t.Fatal(err)
		}
		ck.Advance(1)
	}

	jobs, err = s.ListJobInstances(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Fatalf("limit not applied: got %d", len(jobs))
	}
	// Newest first.
	if jobs[0].CreatedAt < jobs[1].CreatedAt {
		t.Fatalf("not newest-first: %d then %d", jobs[0].CreatedAt, jobs[1].CreatedAt)
	}
	if jobs[0].ItemID != "it-1" || jobs[0].JobType != "RunNotebook" || jobs[0].InvokeType != "Manual" {
		t.Fatalf("bad row: %+v", jobs[0])
	}

	jobs, err = s.ListJobInstances(0) // 0 → default limit
	if err != nil || len(jobs) != 3 {
		t.Fatalf("default limit: %v %d", err, len(jobs))
	}
}

func TestListOperations(t *testing.T) {
	s, err := Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ops, err := s.ListOperations(0)
	if err != nil || len(ops) != 0 {
		t.Fatalf("empty store: %v %d", err, len(ops))
	}

	now := s.Now()
	for i, id := range []string{"op-a", "op-b", "op-c"} {
		op := &Operation{ID: id, Kind: "CreateItem", CreatedAt: now + int64(i), CompleteAt: now + int64(i)}
		if err := s.CreateOperation(op); err != nil {
			t.Fatal(err)
		}
	}

	ops, err = s.ListOperations(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 2 {
		t.Fatalf("limit not applied: got %d", len(ops))
	}
	// Newest first.
	if ops[0].CreatedAt < ops[1].CreatedAt {
		t.Fatalf("not newest-first: %d then %d", ops[0].CreatedAt, ops[1].CreatedAt)
	}

	ops, err = s.ListOperations(0) // 0 → default limit
	if err != nil || len(ops) != 3 {
		t.Fatalf("default limit: %v %d", err, len(ops))
	}
}
