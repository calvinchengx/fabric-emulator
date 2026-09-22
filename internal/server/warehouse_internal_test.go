package server

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"github.com/calvinchengx/fabric-emulator/internal/testsupport"
	"path/filepath"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/clock"
	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/calvinchengx/fabric-emulator/internal/tds"
	"github.com/parquet-go/parquet-go"
)

// fakeWH is a warehouseBackend that hands back a fixed *sql.DB (SQLite in the
// test) and can force an EnsureDatabase error — enough to drive the router
// without a real SQL Server.
type fakeWH struct {
	db        *sql.DB
	ensureErr error
	asCalls   []dbAsCall
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
	defer func() { _ = st.Close() }()
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

	db := testsupport.OpenMSSQL(t)
	ctx := context.Background()
	// Identity principalOf: the "token" passed in is the principal id.
	idOf := func(tok string) (string, error) { return tok, nil }
	route := warehouseRouter(st, &fakeWH{db: db}, idOf, nil)

	// "u" created the workspace, so it is Admin. Lakehouse by id → read-only, the
	// resolved backend database is the item id, and reflection populated the engine.
	got, err := route(ctx, "", lake.ID, "u")
	if err != nil || !got.ReadOnly || got.TargetDB != lake.ID {
		t.Fatalf("lakehouse: %+v err=%v", got, err)
	}
	// The caller travels with the connection: without it the splice would log
	// in as the relay's own account and the engine would have one identity for
	// everyone (docs/55).
	if got.Principal != "u" {
		t.Fatalf("principal = %q, want the caller", got.Principal)
	}
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM [m]").Scan(&n); err != nil || n != 2 {
		t.Fatalf("reflected rows = %d (err %v), want 2", n, err)
	}
	// Warehouse as Admin → read-write.
	if got, err := route(ctx, "", wh.ID, "u"); err != nil || got.ReadOnly || got.TargetDB != wh.ID {
		t.Fatalf("warehouse admin: %+v err=%v", got, err)
	}
	// Unknown database → error.
	if _, err := route(ctx, "", "does-not-exist", "u"); err == nil {
		t.Error("unknown database accepted")
	}
	// A non-SQL item (Notebook) → error.
	if _, err := route(ctx, "", nb.ID, "u"); err == nil {
		t.Error("notebook accepted as a SQL endpoint")
	}
	// EnsureDatabase failure surfaces.
	if _, err := warehouseRouter(st, &fakeWH{db: db, ensureErr: fmt.Errorf("boom")}, idOf, nil)(ctx, "", wh.ID, "u"); err == nil {
		t.Error("EnsureDatabase error not surfaced")
	}

	// --- Connect by display name (real Fabric addressing): workspace from the
	// server name, item by name. Resolves to the same backend database (item id).
	srvByName := ws.DisplayName + ".datawarehouse.fabric.microsoft.com"
	if byName, err := route(ctx, srvByName, "wh", "u"); err != nil || byName.ReadOnly || byName.TargetDB != wh.ID {
		t.Fatalf("warehouse by name: %+v err=%v (want %s, read-write)", byName, err, wh.ID)
	}
	if byID, err := route(ctx, ws.ID+".datawarehouse.fabric.microsoft.com", "lake", "u"); err != nil ||
		!byID.ReadOnly || byID.TargetDB != lake.ID {
		t.Fatalf("lakehouse by name (workspace by id): %+v err=%v", byID, err)
	}
	// A name with no workspace in the server name → error (can't scope it).
	if _, err := route(ctx, "", "wh", "u"); err == nil {
		t.Error("addressed a warehouse by name with no workspace in the server name")
	}
	// A name in an unknown workspace → error.
	if _, err := route(ctx, "no-such-ws.datawarehouse.fabric.microsoft.com", "wh", "u"); err == nil {
		t.Error("resolved a name against an unknown workspace")
	}
	// A name that matches no item in the (valid) workspace → error.
	if _, err := route(ctx, srvByName, "ghost", "u"); err == nil {
		t.Error("resolved a name that matches no item")
	}

	// --- RBAC ---
	// A principal with no role on the workspace is denied.
	if _, err := route(ctx, "", wh.ID, "stranger"); err == nil {
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
	if v, err := route(ctx, "", wh.ID, "viewer"); err != nil || !v.ReadOnly {
		t.Fatalf("warehouse viewer: %+v err=%v (want read-only)", v, err)
	}
	// A Contributor gets read-write on a Warehouse.
	grant("contrib", store.RoleContributor)
	if c, err := route(ctx, "", wh.ID, "contrib"); err != nil || c.ReadOnly {
		t.Fatalf("warehouse contributor: %+v err=%v (want read-write)", c, err)
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

// TestSQLDBFor covers the pipeline Script/StoredProcedure hook's control-plane
// guard: item-not-found, a non-SQL item type, EnsureDatabase failure, and the
// happy path — all against SQLite (no real SQL Server needed for the guard
// logic itself).
func TestSQLDBFor(t *testing.T) {
	st, err := store.Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ws := &store.Workspace{DisplayName: "w"}
	if err := st.CreateWorkspace(ws, store.Principal{ID: "u", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	wh := &store.Item{WorkspaceID: ws.ID, Type: "Warehouse", DisplayName: "wh"}
	nb := &store.Item{WorkspaceID: ws.ID, Type: "Notebook", DisplayName: "nb"}
	for _, it := range []*store.Item{wh, nb} {
		if err := st.CreateItem(it, nil); err != nil {
			t.Fatal(err)
		}
	}
	db := testsupport.OpenMSSQL(t)
	ctx := context.Background()

	// Unknown item → error.
	if _, err := sqlDBFor(&fakeWH{db: db}, st)(ctx, "does-not-exist"); err == nil {
		t.Error("unknown item accepted")
	}
	// A non-SQL item (Notebook) → error.
	if _, err := sqlDBFor(&fakeWH{db: db}, st)(ctx, nb.ID); err == nil {
		t.Error("notebook accepted as a SQL endpoint")
	}
	// EnsureDatabase failure surfaces.
	if _, err := sqlDBFor(&fakeWH{db: db, ensureErr: fmt.Errorf("boom")}, st)(ctx, wh.ID); err == nil {
		t.Error("EnsureDatabase error not surfaced")
	}
	// Happy path: a Warehouse item returns the backend's db.
	got, err := sqlDBFor(&fakeWH{db: db}, st)(ctx, wh.ID)
	if err != nil || got != db {
		t.Fatalf("sqlDBFor happy path: db=%v err=%v", got, err)
	}
}

// TestMirrorItem covers the SQLDatabase mirror hook's control-plane guard
// (EnsureDatabase failure) and that it reaches warehouse.Mirror (a SQLite
// backend fails there — no INFORMATION_SCHEMA.TABLES — proving the call is
// wired; the full successful mirror is proven by the gated e2e against a real
// SQL Server).
func TestMirrorItem(t *testing.T) {
	st, err := store.Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ws := &store.Workspace{DisplayName: "w"}
	if err := st.CreateWorkspace(ws, store.Principal{ID: "u", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	db := &store.Item{WorkspaceID: ws.ID, Type: "SQLDatabase", DisplayName: "db"}
	if err := st.CreateItem(db, nil); err != nil {
		t.Fatal(err)
	}
	sqldb, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer sqldb.Close()
	ctx := context.Background()

	// EnsureDatabase failure surfaces.
	if err := mirrorItem(&fakeWH{db: sqldb, ensureErr: fmt.Errorf("boom")}, st)(ctx, db.ID); err == nil {
		t.Error("EnsureDatabase error not surfaced")
	}
	// A validated call reaches warehouse.Mirror (SQLite has no
	// INFORMATION_SCHEMA.TABLES, so Mirror errors — proving the wiring).
	if err := mirrorItem(&fakeWH{db: sqldb}, st)(ctx, db.ID); err == nil {
		t.Error("expected Mirror to surface an error against a non-SQL-Server backend")
	}
}

// TestResolveSQLItem covers how a TDS connection's `database=` and server name
// become an ITEM — the decision that settles which workspace's data a session
// reads. It was reachable only through TestWarehouseRouter, which gates on a
// real SQL Server and so skips on any machine without one; these lookups need
// no engine at all.
//
// The precedence matters and is easy to get backwards. A workspace can hold a
// Warehouse and a Lakehouse of the SAME name, and they are not
// interchangeable: the Warehouse is read-write and owns its data, the
// Lakehouse endpoint is a read-only reflection. Resolving to the wrong one
// turns a client's writes into errors, or worse, points a reader at a stale
// mirror while it believes it is on the warehouse.
func TestResolveSQLItem(t *testing.T) {
	st, err := store.Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ws := &store.Workspace{DisplayName: "analytics"}
	if err := st.CreateWorkspace(ws, store.Principal{ID: "u", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	other := &store.Workspace{DisplayName: "other"}
	if err := st.CreateWorkspace(other, store.Principal{ID: "u", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	// "sales" exists as BOTH kinds in the same workspace — the ambiguity the
	// precedence rule exists to settle.
	lake := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "sales"}
	wh := &store.Item{WorkspaceID: ws.ID, Type: "Warehouse", DisplayName: "sales"}
	lakeOnly := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "raw"}
	for _, it := range []*store.Item{lake, wh, lakeOnly} {
		if err := st.CreateItem(it, nil); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("by item id, workspace-agnostic", func(t *testing.T) {
		// No workspace in the server name at all: an id is globally unique, so
		// it must resolve without one. This is how the emulator's own tooling
		// connects.
		got, err := resolveSQLItem(st, "localhost", lakeOnly.ID)
		if err != nil || got.ID != lakeOnly.ID {
			t.Fatalf("by id = %v, %v; want %s", got, err, lakeOnly.ID)
		}
	})

	t.Run("by name, workspace from the server label", func(t *testing.T) {
		got, err := resolveSQLItem(st, "analytics.datawarehouse.fabric.microsoft.com", "raw")
		if err != nil || got.ID != lakeOnly.ID {
			t.Fatalf("by name = %v, %v; want %s", got, err, lakeOnly.ID)
		}
	})

	t.Run("workspace addressable by id as well as name", func(t *testing.T) {
		got, err := resolveSQLItem(st, ws.ID+".datawarehouse.fabric.microsoft.com", "raw")
		if err != nil || got.ID != lakeOnly.ID {
			t.Fatalf("workspace by id = %v, %v", got, err)
		}
	})

	t.Run("a Warehouse wins over a Lakehouse of the same name", func(t *testing.T) {
		got, err := resolveSQLItem(st, "analytics", "sales")
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != wh.ID {
			t.Errorf("resolved %q (%s); want the Warehouse %s — the Lakehouse "+
				"endpoint is a read-only reflection and cannot serve a writer",
				got.DisplayName, got.Type, wh.ID)
		}
	})

	t.Run("no workspace in the server name says so", func(t *testing.T) {
		// An IPv4 host has no workspace label, so a NAME cannot be resolved.
		// The message has to explain that, or the user retries the same string.
		_, err := resolveSQLItem(st, "127.0.0.1", "raw")
		if err == nil {
			t.Fatal("want an error when the name has no workspace to resolve in")
		}
		if !strings.Contains(err.Error(), "workspace in the server name") {
			t.Errorf("error = %q; it must name the missing piece", err)
		}
	})

	t.Run("unknown workspace, and unknown item, are different errors", func(t *testing.T) {
		_, err := resolveSQLItem(st, "nosuchws", "raw")
		if err == nil || !strings.Contains(err.Error(), "not found") {
			t.Errorf("unknown workspace error = %v", err)
		}
		// "raw" exists, but not in THIS workspace — a cross-workspace read
		// must not resolve.
		_, err = resolveSQLItem(st, "other", "raw")
		if err == nil {
			t.Fatal("an item in another workspace must not resolve")
		}
		if !strings.Contains(err.Error(), "no warehouse or lakehouse") {
			t.Errorf("error = %q; want the item-not-in-workspace message", err)
		}
	})

	t.Run("resolveWorkspace takes an id or a display name", func(t *testing.T) {
		byID, err := resolveWorkspace(st, ws.ID)
		if err != nil || byID.ID != ws.ID {
			t.Fatalf("by id = %v, %v", byID, err)
		}
		byName, err := resolveWorkspace(st, "analytics")
		if err != nil || byName.ID != ws.ID {
			t.Fatalf("by name = %v, %v", byName, err)
		}
		if _, err := resolveWorkspace(st, "nope"); err == nil {
			t.Error("want an error for an unknown workspace")
		}
	})
}

// ---- item permissions at the router ----------------------------------------------

func TestDBRungFromRoleAndItemAccess(t *testing.T) {
	for _, tc := range []struct {
		name     string
		role     string
		access   store.Access
		readOnly bool
		want     tds.Role
	}{
		{"a Member owns", store.RoleMember, store.Access{}, true, tds.RoleOwner},
		{"a Contributor on a warehouse writes", store.RoleContributor, store.Access{}, false, tds.RoleWriter},
		{"ReadData reads", "", store.Access{Permissions: []string{"Read"}, Additional: []string{"ReadData"}}, true, tds.RoleReader},
		{"Read alone connects", "", store.Access{Permissions: []string{"Read"}}, true, tds.RoleConnect},
		{"nothing is no access", "", store.Access{}, true, tds.RoleNone},
	} {
		if got := dbRung(tc.role, tc.access, tc.readOnly); got != tc.want {
			t.Errorf("%s: rung = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// Every SQL item in the workspace is listed with the caller's rung on it —
// including RoleNone where it has no access, so a lingering user can lose CONNECT.
func TestWorkspaceGrantsListsNoAccessToo(t *testing.T) {
	st, err := store.Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ws := &store.Workspace{DisplayName: "w"}
	if err := st.CreateWorkspace(ws, store.Principal{ID: "owner", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	shared := &store.Item{WorkspaceID: ws.ID, Type: "Warehouse", DisplayName: "shared"}
	unshared := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "unshared"}
	for _, it := range []*store.Item{shared, unshared} {
		if err := st.CreateItem(it, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.PutItemAccess(store.ItemAccess{ItemID: shared.ID, PrincipalID: "stranger", PrincipalType: "User",
		Permissions: []string{"Read"}, Additional: []string{"ReadData"}}); err != nil {
		t.Fatal(err)
	}
	grants, err := workspaceGrants(st, ws.ID, "stranger", "")
	if err != nil {
		t.Fatal(err)
	}
	rung := map[string]tds.Role{}
	for _, g := range grants {
		rung[g.Database] = g.Role
	}
	if rung[shared.ID] != tds.RoleReader || rung[unshared.ID] != tds.RoleNone || len(rung) != 2 {
		t.Fatalf("grants = %+v, want Reader on the shared item and None on the other", grants)
	}
}

// Fails closed: now that grants also REMOVE access, a store error that skipped an
// item would leave a revoked grant working.
func TestWorkspaceGrantsFailsClosed(t *testing.T) {
	t.Run("listing items", func(t *testing.T) {
		st, err := store.Open("", clock.New())
		if err != nil {
			t.Fatal(err)
		}
		_ = st.Close()
		if _, err := workspaceGrants(st, "ws", "p", ""); err == nil {
			t.Fatal("a failed item listing produced grants")
		}
	})
	t.Run("reading a grant", func(t *testing.T) {
		st, ws, dir := diskStoreWithWarehouse(t)
		corruptGrant(t, st, dir, ws, "p")
		if _, err := workspaceGrants(st, ws.ID, "p", ""); err == nil {
			t.Fatal("an unreadable grant produced grants")
		}
	})
}

// The router refuses on either store failure rather than guessing a rung.
func TestTheRouterFailsClosedOnAccessErrors(t *testing.T) {
	ctx := context.Background()
	idOf := func(tok string) (string, error) { return tok, nil }
	t.Run("the caller's access", func(t *testing.T) {
		st, ws, dir := diskStoreWithWarehouse(t)
		items, _ := st.ListItems(ws.ID, "Warehouse")
		execOn(t, dir, `ALTER TABLE role_assignments RENAME TO ra_elsewhere`)
		if _, err := warehouseRouter(st, &fakeWH{}, idOf, nil)(ctx, "", items[0].ID, "owner"); err == nil ||
			!strings.Contains(err.Error(), "checking access") {
			t.Fatalf("err = %v, want an access-check failure", err)
		}
	})
	t.Run("the workspace grants", func(t *testing.T) {
		st, ws, dir := diskStoreWithWarehouse(t)
		items, _ := st.ListItems(ws.ID, "Warehouse")
		// A second item whose grant for the owner is corrupt: the target's own
		// access resolves, the sweep across the workspace does not.
		other := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "other"}
		if err := st.CreateItem(other, nil); err != nil {
			t.Fatal(err)
		}
		if err := st.PutItemAccess(store.ItemAccess{ItemID: other.ID, PrincipalID: "owner", PrincipalType: "User",
			Permissions: []string{"Read"}}); err != nil {
			t.Fatal(err)
		}
		execOn(t, dir, `UPDATE item_access SET permissions = 'not json' WHERE item_id = '`+other.ID+`'`)
		if _, err := warehouseRouter(st, &fakeWH{}, idOf, nil)(ctx, "", items[0].ID, "owner"); err == nil ||
			!strings.Contains(err.Error(), "checking access") {
			t.Fatalf("err = %v, want the sweep's failure", err)
		}
	})
	t.Run("no Read", func(t *testing.T) {
		st, ws, _ := diskStoreWithWarehouse(t)
		items, _ := st.ListItems(ws.ID, "Warehouse")
		if _, err := warehouseRouter(st, &fakeWH{}, idOf, nil)(ctx, "", items[0].ID, "stranger"); err == nil ||
			!strings.Contains(err.Error(), "access denied") {
			t.Fatalf("err = %v, want access denied", err)
		}
	})
}

func diskStoreWithWarehouse(t *testing.T) (*store.Store, *store.Workspace, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir, clock.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ws := &store.Workspace{DisplayName: "w"}
	if err := st.CreateWorkspace(ws, store.Principal{ID: "owner", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateItem(&store.Item{WorkspaceID: ws.ID, Type: "Warehouse", DisplayName: "wh"}, nil); err != nil {
		t.Fatal(err)
	}
	return st, ws, dir
}

func corruptGrant(t *testing.T, st *store.Store, dir string, ws *store.Workspace, principal string) {
	t.Helper()
	items, err := st.ListItems(ws.ID, "Warehouse")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutItemAccess(store.ItemAccess{ItemID: items[0].ID, PrincipalID: principal, PrincipalType: "User",
		Permissions: []string{"Read"}}); err != nil {
		t.Fatal(err)
	}
	execOn(t, dir, `UPDATE item_access SET permissions = 'not json'`)
}

func execOn(t *testing.T, dir, stmt string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "fabric-emulator.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(stmt); err != nil {
		t.Fatalf("%s: %v", stmt, err)
	}
}

// DBAs records what a server-side read on a caller's behalf asked the backend
// for, and hands back the fake's database.
func (f *fakeWH) DBAs(_ context.Context, database, principal string, grants []tds.Grant) (*sql.DB, error) {
	f.asCalls = append(f.asCalls, dbAsCall{database, principal, grants})
	return f.db, nil
}

type dbAsCall struct {
	database, principal string
	grants              []tds.Grant
}

// sqlDBAsFor is the hook Direct Lake on SQL reads through: the same access
// decision as a relayed connection, the item's database prepared, a lakehouse
// reflected, and the backend asked to log in AS the caller with the rungs the
// workspace gives them.
func TestSQLDBAsFor(t *testing.T) {
	st, err := store.Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
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
	var buf bytes.Buffer
	pw := parquet.NewGenericWriter[metricRow](&buf)
	if _, err := pw.Write([]metricRow{{1, 10.5}}); err != nil {
		t.Fatal(err)
	}
	_ = pw.Close()
	for rel, content := range map[string][]byte{
		"Tables/dlm/part-0.parquet":                       buf.Bytes(),
		"Tables/dlm/_delta_log/00000000000000000000.json": []byte(`{"add":{"path":"part-0.parquet"}}`),
	} {
		if err := st.CreateOneLakePath(&store.OneLakePath{WorkspaceID: ws.ID, ItemID: lake.ID, RelPath: rel, Content: content}, false); err != nil {
			t.Fatal(err)
		}
	}
	db := testsupport.OpenMSSQL(t)
	ctx := context.Background()

	be := &fakeWH{db: db}
	open := sqlDBAsFor(be, st, nil)
	for _, it := range []*store.Item{lake, wh} {
		got, err := open(ctx, it.ID, "u")
		if err != nil || got != db {
			t.Fatalf("%s: %v, %v", it.Type, got, err)
		}
	}
	if len(be.asCalls) != 2 || be.asCalls[0].database != lake.ID || be.asCalls[1].database != wh.ID || be.asCalls[1].principal != "u" {
		t.Fatalf("backend calls = %+v", be.asCalls)
	}
	// The rungs are the workspace's, the same list a relayed connection carries:
	// the Admin owns both items.
	for _, g := range be.asCalls[1].grants {
		if g.Role != tds.RoleOwner {
			t.Errorf("grant %+v, want owner", g)
		}
	}
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM [dlm]").Scan(&n); err != nil || n != 1 {
		t.Fatalf("the lakehouse was not reflected before the read: %d, %v", n, err)
	}
	t.Cleanup(func() { _, _ = db.Exec("DROP TABLE IF EXISTS [dlm]") })

	for name, tc := range map[string]struct {
		open      func(context.Context, string, string) (*sql.DB, error)
		item, who string
	}{
		"an unknown item":                 {open, "does-not-exist", "u"},
		"an item with no SQL endpoint":    {open, nb.ID, "u"},
		"a principal without Read":        {open, wh.ID, "stranger"},
		"a database that cannot be ready": {sqlDBAsFor(&fakeWH{db: db, ensureErr: fmt.Errorf("boom")}, st, nil), wh.ID, "u"},
		"a lakehouse that cannot reflect": {sqlDBAsFor(&fakeWH{db: closedDB(t)}, st, nil), lake.ID, "u"},
	} {
		if _, err := tc.open(ctx, tc.item, tc.who); err == nil {
			t.Errorf("%s: opened", name)
		}
	}
}

func closedDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	return db
}
