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
