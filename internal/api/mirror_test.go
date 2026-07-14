package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// TestRefreshMirror covers the mirror hook's control-plane behavior: RBAC, the
// item-type guard, the 501 when no backend is wired, the happy path, and the
// error surface — all without a SQL backend (MirrorItem is faked).
func TestRefreshMirror(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	db := &store.Item{WorkspaceID: ws.ID, Type: "SQLDatabase", DisplayName: "opsdb"}
	if err := st.CreateItem(db, nil); err != nil {
		t.Fatal(err)
	}
	nb := &store.Item{WorkspaceID: ws.ID, Type: "Notebook", DisplayName: "nb"}
	if err := st.CreateItem(nb, nil); err != nil {
		t.Fatal(err)
	}
	at := func(iid string) map[string]string { return map[string]string{"wid": ws.ID, "iid": iid} }

	// No backend wired → 501.
	if w := do(a.refreshMirror, admin, "POST", "", at(db.ID)); w.Code != http.StatusNotImplemented {
		t.Fatalf("nil MirrorItem: code %d, want 501", w.Code)
	}

	// Happy path → 200 and the hook is called with the item id.
	var called string
	a.MirrorItem = func(_ context.Context, id string) error { called = id; return nil }
	if w := do(a.refreshMirror, admin, "POST", "", at(db.ID)); w.Code != http.StatusOK {
		t.Fatalf("happy: code %d, want 200", w.Code)
	}
	if called != db.ID {
		t.Fatalf("MirrorItem called with %q, want %q", called, db.ID)
	}

	// A non-SQLDatabase item → 404.
	if w := do(a.refreshMirror, admin, "POST", "", at(nb.ID)); w.Code != http.StatusNotFound {
		t.Fatalf("notebook: code %d, want 404", w.Code)
	}

	// A mirror failure surfaces as 502.
	a.MirrorItem = func(_ context.Context, _ string) error { return fmt.Errorf("boom") }
	if w := do(a.refreshMirror, admin, "POST", "", at(db.ID)); w.Code != http.StatusBadGateway {
		t.Fatalf("mirror error: code %d, want 502", w.Code)
	}

	// A Viewer (below Contributor) is refused.
	if w := do(a.refreshMirror, viewer, "POST", "", at(db.ID)); w.Code != http.StatusForbidden {
		t.Fatalf("viewer: code %d, want 403", w.Code)
	}
}
