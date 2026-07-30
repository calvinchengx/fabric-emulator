package store

import (
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/clock"
)

func namesStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestWorkspaceNameTaken(t *testing.T) {
	s := namesStore(t)
	p := Principal{ID: "p1", Type: "User"}

	taken, err := s.WorkspaceNameTaken("analytics", "")
	if err != nil || taken {
		t.Fatalf("empty store: %v %v", err, taken)
	}

	ws := &Workspace{DisplayName: "analytics"}
	if err := s.CreateWorkspace(ws, p); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"analytics", "Analytics", "ANALYTICS"} {
		taken, err := s.WorkspaceNameTaken(name, "")
		if err != nil {
			t.Fatal(err)
		}
		if !taken {
			t.Fatalf("%q should be taken (case-insensitive)", name)
		}
	}

	// The holder renaming to its own name is not a conflict.
	if taken, err := s.WorkspaceNameTaken("analytics", ws.ID); err != nil || taken {
		t.Fatalf("self-rename flagged: %v %v", err, taken)
	}
	// A different workspace renaming into it is.
	if taken, err := s.WorkspaceNameTaken("analytics", "other-id"); err != nil || !taken {
		t.Fatalf("other-rename not flagged: %v %v", err, taken)
	}
	if taken, err := s.WorkspaceNameTaken("unused", ""); err != nil || taken {
		t.Fatalf("unused name flagged: %v %v", err, taken)
	}
}

func TestItemNameTaken(t *testing.T) {
	s := namesStore(t)
	p := Principal{ID: "p1", Type: "User"}
	ws := &Workspace{DisplayName: "ws"}
	if err := s.CreateWorkspace(ws, p); err != nil {
		t.Fatal(err)
	}
	other := &Workspace{DisplayName: "other"}
	if err := s.CreateWorkspace(other, p); err != nil {
		t.Fatal(err)
	}

	it := &Item{WorkspaceID: ws.ID, Type: "Notebook", DisplayName: "hello"}
	if err := s.CreateItem(it, nil); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name               string
		wid, disp, typ, ex string
		want               bool
	}{
		{"same name+type", ws.ID, "hello", "Notebook", "", true},
		{"case-insensitive name", ws.ID, "HELLO", "Notebook", "", true},
		{"case-insensitive type", ws.ID, "hello", "notebook", "", true},
		{"reusable across types", ws.ID, "hello", "Lakehouse", "", false},
		{"scoped to workspace", other.ID, "hello", "Notebook", "", false},
		{"self-rename allowed", ws.ID, "hello", "Notebook", it.ID, false},
		{"other item renaming in", ws.ID, "hello", "Notebook", "another", true},
		{"unused name", ws.ID, "nope", "Notebook", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := s.ItemNameTaken(c.wid, c.disp, c.typ, c.ex)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Fatalf("got %v want %v", got, c.want)
			}
		})
	}
}

func TestNameTakenClosedDB(t *testing.T) {
	s := namesStore(t)
	s.Close()
	if _, err := s.WorkspaceNameTaken("x", ""); err == nil {
		t.Fatal("WorkspaceNameTaken on closed DB should error")
	}
	if _, err := s.ItemNameTaken("w", "x", "Notebook", ""); err == nil {
		t.Fatal("ItemNameTaken on closed DB should error")
	}
}
