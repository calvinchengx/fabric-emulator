package server

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/clock"
	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/parquet-go/parquet-go"
	_ "modernc.org/sqlite"
)

// fakeWH is a warehouseBackend that hands back a fixed *sql.DB (SQLite in the
// test) and can force an EnsureDatabase error — enough to drive the router
// without a real SQL Server.
type fakeWH struct {
	db        *sql.DB
	ensureErr error
}

func (f *fakeWH) EnsureDatabase(context.Context, string) error { return f.ensureErr }
func (f *fakeWH) DB(string) *sql.DB                            { return f.db }

type metricRow struct {
	ID     int64   `parquet:"id"`
	Amount float64 `parquet:"amount"`
}

// TestWarehouseRouter covers the two-surface routing: Lakehouse (read-only,
// reflect), Warehouse (read-write), unknown/non-SQL items, and the
// EnsureDatabase error path — all against SQLite, no SQL Server needed.
func TestWarehouseRouter(t *testing.T) {
	st, err := store.Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ws := &store.Workspace{DisplayName: "w"}
	if err := st.CreateWorkspace(ws, store.Principal{ID: "u", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	lake := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lake"}
	wh := &store.Item{WorkspaceID: ws.ID, Type: "Warehouse", DisplayName: "wh"}
	nb := &store.Item{WorkspaceID: ws.ID, Type: "Notebook", DisplayName: "nb"}
	for _, it := range []*store.Item{lake, wh, nb} {
		if err := st.CreateItem(it, nil); err != nil {
			t.Fatal(err)
		}
	}
	// A numeric-only Delta table in the lakehouse (no N'…' literals on SQLite).
	var buf bytes.Buffer
	pw := parquet.NewGenericWriter[metricRow](&buf)
	if _, err := pw.Write([]metricRow{{1, 10.5}, {2, 20.5}}); err != nil {
		t.Fatal(err)
	}
	_ = pw.Close()
	seed := func(rel string, content []byte) {
		if err := st.CreateOneLakePath(&store.OneLakePath{WorkspaceID: ws.ID, ItemID: lake.ID, RelPath: rel, Content: content}, false); err != nil {
			t.Fatal(err)
		}
	}
	seed("Tables/m/part-0.parquet", buf.Bytes())
	seed("Tables/m/_delta_log/00000000000000000000.json", []byte(`{"add":{"path":"part-0.parquet"}}`))

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	// Identity principalOf: the "token" passed in is the principal id.
	idOf := func(tok string) (string, error) { return tok, nil }
	route := warehouseRouter(st, &fakeWH{db: db}, idOf)

	// "u" created the workspace, so it is Admin. Lakehouse → read-only, and
	// reflection populated the engine.
	ro, err := route(ctx, lake.ID, "u")
	if err != nil || !ro {
		t.Fatalf("lakehouse: readOnly=%v err=%v", ro, err)
	}
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM [m]").Scan(&n); err != nil || n != 2 {
		t.Fatalf("reflected rows = %d (err %v), want 2", n, err)
	}
	// Warehouse as Admin → read-write.
	if ro, err := route(ctx, wh.ID, "u"); err != nil || ro {
		t.Fatalf("warehouse admin: readOnly=%v err=%v", ro, err)
	}
	// Unknown database → error.
	if _, err := route(ctx, "does-not-exist", "u"); err == nil {
		t.Error("unknown database accepted")
	}
	// A non-SQL item (Notebook) → error.
	if _, err := route(ctx, nb.ID, "u"); err == nil {
		t.Error("notebook accepted as a SQL endpoint")
	}
	// EnsureDatabase failure surfaces.
	if _, err := warehouseRouter(st, &fakeWH{db: db, ensureErr: fmt.Errorf("boom")}, idOf)(ctx, wh.ID, "u"); err == nil {
		t.Error("EnsureDatabase error not surfaced")
	}

	// --- RBAC ---
	// A principal with no role on the workspace is denied.
	if _, err := route(ctx, wh.ID, "stranger"); err == nil {
		t.Error("a principal with no workspace role was granted access")
	}
	grant := func(principal, role string) {
		if err := st.CreateRoleAssignment(&store.RoleAssignment{
			WorkspaceID: ws.ID, Principal: store.Principal{ID: principal, Type: "User"}, Role: role}); err != nil {
			t.Fatal(err)
		}
	}
	// A Viewer gets read-only, even on a Warehouse.
	grant("viewer", store.RoleViewer)
	if ro, err := route(ctx, wh.ID, "viewer"); err != nil || !ro {
		t.Fatalf("warehouse viewer: readOnly=%v err=%v (want read-only)", ro, err)
	}
	// A Contributor gets read-write on a Warehouse.
	grant("contrib", store.RoleContributor)
	if ro, err := route(ctx, wh.ID, "contrib"); err != nil || ro {
		t.Fatalf("warehouse contributor: readOnly=%v err=%v (want read-write)", ro, err)
	}
}
