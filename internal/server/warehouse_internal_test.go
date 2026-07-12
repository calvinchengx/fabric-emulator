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

	// "u" created the workspace, so it is Admin. Lakehouse by id → read-only, the
	// resolved backend database is the item id, and reflection populated the engine.
	tdb, ro, err := route(ctx, "", lake.ID, "u")
	if err != nil || !ro || tdb != lake.ID {
		t.Fatalf("lakehouse: db=%q readOnly=%v err=%v", tdb, ro, err)
	}
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM [m]").Scan(&n); err != nil || n != 2 {
		t.Fatalf("reflected rows = %d (err %v), want 2", n, err)
	}
	// Warehouse as Admin → read-write.
	if tdb, ro, err := route(ctx, "", wh.ID, "u"); err != nil || ro || tdb != wh.ID {
		t.Fatalf("warehouse admin: db=%q readOnly=%v err=%v", tdb, ro, err)
	}
	// Unknown database → error.
	if _, _, err := route(ctx, "", "does-not-exist", "u"); err == nil {
		t.Error("unknown database accepted")
	}
	// A non-SQL item (Notebook) → error.
	if _, _, err := route(ctx, "", nb.ID, "u"); err == nil {
		t.Error("notebook accepted as a SQL endpoint")
	}
	// EnsureDatabase failure surfaces.
	if _, _, err := warehouseRouter(st, &fakeWH{db: db, ensureErr: fmt.Errorf("boom")}, idOf)(ctx, "", wh.ID, "u"); err == nil {
		t.Error("EnsureDatabase error not surfaced")
	}

	// --- Connect by display name (real Fabric addressing): workspace from the
	// server name, item by name. Resolves to the same backend database (item id).
	srvByName := ws.DisplayName + ".datawarehouse.fabric.microsoft.com"
	if tdb, ro, err := route(ctx, srvByName, "wh", "u"); err != nil || ro || tdb != wh.ID {
		t.Fatalf("warehouse by name: db=%q readOnly=%v err=%v (want %s, read-write)", tdb, ro, err, wh.ID)
	}
	if tdb, ro, err := route(ctx, ws.ID+".datawarehouse.fabric.microsoft.com", "lake", "u"); err != nil || !ro || tdb != lake.ID {
		t.Fatalf("lakehouse by name (workspace by id): db=%q readOnly=%v err=%v", tdb, ro, err)
	}
	// A name with no workspace in the server name → error (can't scope it).
	if _, _, err := route(ctx, "", "wh", "u"); err == nil {
		t.Error("addressed a warehouse by name with no workspace in the server name")
	}
	// A name in an unknown workspace → error.
	if _, _, err := route(ctx, "no-such-ws.datawarehouse.fabric.microsoft.com", "wh", "u"); err == nil {
		t.Error("resolved a name against an unknown workspace")
	}
	// A name that matches no item in the (valid) workspace → error.
	if _, _, err := route(ctx, srvByName, "ghost", "u"); err == nil {
		t.Error("resolved a name that matches no item")
	}

	// --- RBAC ---
	// A principal with no role on the workspace is denied.
	if _, _, err := route(ctx, "", wh.ID, "stranger"); err == nil {
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
	if _, ro, err := route(ctx, "", wh.ID, "viewer"); err != nil || !ro {
		t.Fatalf("warehouse viewer: readOnly=%v err=%v (want read-only)", ro, err)
	}
	// A Contributor gets read-write on a Warehouse.
	grant("contrib", store.RoleContributor)
	if _, ro, err := route(ctx, "", wh.ID, "contrib"); err != nil || ro {
		t.Fatalf("warehouse contributor: readOnly=%v err=%v (want read-write)", ro, err)
	}
}

// TestWorkspaceRef covers extracting the workspace from a Fabric server name:
// the dotted Fabric host, a bare label, an IPv4 host (no workspace), and empties.
func TestWorkspaceRef(t *testing.T) {
	cases := []struct{ server, want string }{
		{"my-ws.datawarehouse.fabric.microsoft.com", "my-ws"},
		{"  ws2.datawarehouse.fabric.microsoft.com  ", "ws2"},
		{"bareLabel", "bareLabel"},
		{"127.0.0.1", ""},   // IPv4 first label is numeric — not a workspace
		{"10.0.0.5", ""},    // ditto
		{"", ""},            // empty
		{"   ", ""},         // whitespace only
		{".leadingdot", ""}, // empty first label
	}
	for _, c := range cases {
		if got := workspaceRef(c.server); got != c.want {
			t.Errorf("workspaceRef(%q) = %q, want %q", c.server, got, c.want)
		}
	}
}
