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

// TestRefreshMirroredDatabase covers the external-source mirror hook's
// control-plane validation: RBAC, the item-type guard, a missing/unknown
// connection, a connection missing server/database or non-Basic credentials —
// and that a validated request reaches warehouse.Mirror (a connect failure to
// an unreachable host surfaces as 502, proving the wiring, without needing a
// real SQL Server for this non-gated test).
func TestRefreshMirroredDatabase(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	md := &store.Item{WorkspaceID: ws.ID, Type: "MirroredDatabase", DisplayName: "mirror"}
	if err := st.CreateItem(md, nil); err != nil {
		t.Fatal(err)
	}
	nb := &store.Item{WorkspaceID: ws.ID, Type: "Notebook", DisplayName: "nb"}
	if err := st.CreateItem(nb, nil); err != nil {
		t.Fatal(err)
	}
	at := func(iid string) map[string]string { return map[string]string{"wid": ws.ID, "iid": iid} }

	mkConn := func(details, credType string) string {
		c := &store.Connection{DisplayName: "src", Details: []byte(details), CredentialsJSON: `{"credentialType":"` + credType + `","username":"sa","password":"x"}`}
		if credType == "" {
			c.CredentialsJSON = ""
		}
		if err := st.CreateConnection(c); err != nil {
			t.Fatal(err)
		}
		return c.ID
	}

	// Missing connectionId → 400.
	if w := do(a.refreshMirroredDatabase, admin, "POST", "{}", at(md.ID)); w.Code != http.StatusBadRequest {
		t.Fatalf("missing connectionId: code %d, want 400", w.Code)
	}

	// Item not found / wrong type → 404.
	if w := do(a.refreshMirroredDatabase, admin, "POST", `{"connectionId":"x"}`, at(nb.ID)); w.Code != http.StatusNotFound {
		t.Fatalf("notebook item: code %d, want 404", w.Code)
	}

	// Unknown connection → 400.
	if w := do(a.refreshMirroredDatabase, admin, "POST", `{"connectionId":"does-not-exist"}`, at(md.ID)); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown connection: code %d, want 400", w.Code)
	}

	// Connection missing server/database → 400.
	badDetails := mkConn(`{"server":""}`, "Basic")
	if w := do(a.refreshMirroredDatabase, admin, "POST", `{"connectionId":"`+badDetails+`"}`, at(md.ID)); w.Code != http.StatusBadRequest {
		t.Fatalf("missing server/database: code %d, want 400", w.Code)
	}

	// Non-Basic credentials → 400.
	nonBasic := mkConn(`{"server":"localhost:1","database":"d"}`, "Key")
	if w := do(a.refreshMirroredDatabase, admin, "POST", `{"connectionId":"`+nonBasic+`"}`, at(md.ID)); w.Code != http.StatusBadRequest {
		t.Fatalf("non-Basic credentials: code %d, want 400", w.Code)
	}

	// No credentials at all → 400.
	noCreds := mkConn(`{"server":"localhost:1","database":"d"}`, "")
	if w := do(a.refreshMirroredDatabase, admin, "POST", `{"connectionId":"`+noCreds+`"}`, at(md.ID)); w.Code != http.StatusBadRequest {
		t.Fatalf("no credentials: code %d, want 400", w.Code)
	}

	// A validated request reaches the mirror attempt: an unreachable host fails
	// to connect/query, surfacing as 502 — proving the whole path is wired, not
	// just accepted.
	good := mkConn(`{"server":"127.0.0.1:1","database":"d"}`, "Basic")
	if w := do(a.refreshMirroredDatabase, admin, "POST", `{"connectionId":"`+good+`"}`, at(md.ID)); w.Code != http.StatusBadGateway {
		t.Fatalf("unreachable source: code %d, want 502, body %s", w.Code, w.Body)
	}

	// A Viewer (below Contributor) is refused.
	if w := do(a.refreshMirroredDatabase, viewer, "POST", `{"connectionId":"`+good+`"}`, at(md.ID)); w.Code != http.StatusForbidden {
		t.Fatalf("viewer: code %d, want 403", w.Code)
	}
}
