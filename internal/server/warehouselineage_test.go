package server

import (
	"sort"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/clock"
	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/calvinchengx/fabric-emulator/internal/tsql"
)

// lineageFixture: a workspace with a lakehouse (silver, reflected) and a
// warehouse (gold) — the two items a medallion's last hop spans.
type lineageFixture struct {
	st   *store.Store
	wl   *warehouseLineage
	lake *store.Item
	wh   *store.Item
}

func newLineageFixture(t *testing.T) *lineageFixture {
	t.Helper()
	st, err := store.Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ws := &store.Workspace{DisplayName: "w"}
	if err := st.CreateWorkspace(ws, store.Principal{ID: "u", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	lake := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lake"}
	wh := &store.Item{WorkspaceID: ws.ID, Type: "Warehouse", DisplayName: "dw"}
	for _, it := range []*store.Item{lake, wh} {
		if err := st.CreateItem(it, nil); err != nil {
			t.Fatal(err)
		}
	}
	return &lineageFixture{st: st, wl: newWarehouseLineage(st), lake: lake, wh: wh}
}

// edges returns the recorded movements as "sourceItem:path -> targetItem:path",
// with item ids reduced to their display names so the assertions read.
func (f *lineageFixture) edges(t *testing.T) []string {
	t.Helper()
	all, err := f.st.ListLineageEdges(f.wh.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	name := func(id string) string {
		switch id {
		case f.lake.ID:
			return "lake"
		case f.wh.ID:
			return "dw"
		}
		return id
	}
	var out []string
	for _, e := range all {
		out = append(out, name(e.SourceItemID)+":"+e.SourcePath+" -> "+name(e.TargetItemID)+":"+e.TargetPath)
	}
	sort.Strings(out)
	return out
}

func (f *lineageFixture) observe(sql string) {
	f.wl.observe(f.wh.ID, tsql.DataFlows(sql))
}

// TestWarehouseLineageDbtBuildAndSwap replays the exact statement sequence
// dbt-fabric ships for one model — temp view, EXEC-wrapped CTAS into a temp
// table, sp_rename, drop the view — and asserts the graph ends up naming the
// real table and the silver tables behind it, with no scaffolding left.
func TestWarehouseLineageDbtBuildAndSwap(t *testing.T) {
	f := newLineageFixture(t)

	// 1. The model body lands in a temp view that reads the reflected lakehouse
	//    by three-part name. This is the cross-item hop: silver → gold.
	f.observe(`create view dbo.fct_order_lines__dbt_tmp_vw as
	    select o.*, c.customer_key
	    from [` + f.lake.ID + `].[dbo].[silver_orders] o
	    join [` + f.lake.ID + `].[dbo].[silver_customer_conformed] c
	      on o.customer_id = c.customer_id`)
	// A view holds no bytes, so it is not itself an edge.
	if got := f.edges(t); got != nil {
		t.Fatalf("a view alone recorded edges: %v", got)
	}

	// 2. The CTAS, wrapped in dynamic SQL exactly as dbt sends it.
	f.observe(`EXEC('CREATE TABLE [` + f.wh.ID + `].[dbo].[fct_order_lines__dbt_temp] ` +
		`AS SELECT * FROM [` + f.wh.ID + `].[dbo].[fct_order_lines__dbt_tmp_vw] ` +
		`OPTION (LABEL = ''dbt-fabric-dw'');')`)

	// The scaffold view resolved through: the sources are the silver tables,
	// not the view.
	want := []string{
		"lake:Tables/silver_customer_conformed -> dw:Tables/fct_order_lines__dbt_temp",
		"lake:Tables/silver_orders -> dw:Tables/fct_order_lines__dbt_temp",
	}
	if got := f.edges(t); !equal(got, want) {
		t.Fatalf("after CTAS:\n got %v\nwant %v", got, want)
	}

	// 3. The swap. The graph must now name the table anyone queries.
	f.observe(`EXEC sp_rename 'dbo.fct_order_lines__dbt_temp', 'fct_order_lines'`)
	want = []string{
		"lake:Tables/silver_customer_conformed -> dw:Tables/fct_order_lines",
		"lake:Tables/silver_orders -> dw:Tables/fct_order_lines",
	}
	if got := f.edges(t); !equal(got, want) {
		t.Fatalf("after rename:\n got %v\nwant %v", got, want)
	}

	// 4. Dropping the scaffold view must not disturb the real edges.
	f.observe(`drop view if exists dbo.fct_order_lines__dbt_tmp_vw`)
	if got := f.edges(t); !equal(got, want) {
		t.Fatalf("after dropping the scaffold view:\n got %v\nwant %v", got, want)
	}
}

// TestWarehouseLineageWithinWarehouse: a gold model built from another gold
// model (dim_date derives from fct_order_lines) is a same-item edge, and a
// two-part name resolves to the connection's own item.
func TestWarehouseLineageWithinWarehouse(t *testing.T) {
	f := newLineageFixture(t)
	f.observe(`SELECT distinct order_date INTO dbo.dim_date FROM dbo.fct_order_lines`)
	want := []string{"dw:Tables/fct_order_lines -> dw:Tables/dim_date"}
	if got := f.edges(t); !equal(got, want) {
		t.Fatalf("intra-warehouse edge:\n got %v\nwant %v", got, want)
	}
	// An INSERT … SELECT appends: still a movement.
	f.observe(`INSERT INTO dbo.dim_date SELECT * FROM dbo.late_arrivals`)
	if got := f.edges(t); len(got) != 2 {
		t.Fatalf("insert edge missing: %v", got)
	}
}

func TestWarehouseLineageRefusesToGuess(t *testing.T) {
	f := newLineageFixture(t)
	// A database part that is not an item this emulator knows.
	f.observe(`SELECT * INTO dbo.gold FROM [not-an-item].[dbo].[t]`)
	if got := f.edges(t); got != nil {
		t.Fatalf("resolved an unknown database: %v", got)
	}
	// A rebuild reading itself is not a movement worth drawing.
	f.observe(`INSERT INTO dbo.t SELECT * FROM dbo.t`)
	if got := f.edges(t); got != nil {
		t.Fatalf("self-edge recorded: %v", got)
	}
	// A dropped table's incoming edges are retired: a graph that keeps drawing
	// a table nobody can query has started lying.
	f.observe(`SELECT * INTO dbo.gold FROM dbo.src`)
	if got := f.edges(t); len(got) != 1 {
		t.Fatalf("setup edge missing: %v", got)
	}
	f.observe(`DROP TABLE dbo.gold`)
	if got := f.edges(t); got != nil {
		t.Fatalf("edges survived the drop: %v", got)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestWarehouseLineageRebuildOverAnExistingTable: dbt's swap when the table
// ALREADY exists moves the old one aside first —
//
//	rename x -> x__dbt_backup ; rename x__dbt_temp -> x ; drop x__dbt_backup
//
// A downstream table built from x must still read x afterwards. Rewriting
// source paths on rename repointed those inputs at the backup, which the drop
// then removed: the medallion showed `fct_orders__dbt_backup -> fct_daily_revenue`,
// an edge from a table that no longer existed.
func TestWarehouseLineageRebuildOverAnExistingTable(t *testing.T) {
	f := newLineageFixture(t)

	// First build, and an aggregate downstream of it.
	f.observe(`SELECT * INTO dbo.fct_orders FROM [` + f.lake.ID + `].[dbo].[silver_orders]`)
	f.observe(`SELECT * INTO dbo.fct_daily_revenue FROM dbo.fct_orders`)
	want := []string{
		"dw:Tables/fct_orders -> dw:Tables/fct_daily_revenue",
		"lake:Tables/silver_orders -> dw:Tables/fct_orders",
	}
	if got := f.edges(t); !equal(got, want) {
		t.Fatalf("first build:\n got %v\nwant %v", got, want)
	}

	// Rebuild fct_orders: aside, swap, drop.
	f.observe(`SELECT * INTO dbo.fct_orders__dbt_temp FROM [` + f.lake.ID + `].[dbo].[silver_orders]`)
	f.observe(`EXEC sp_rename 'dbo.fct_orders', 'fct_orders__dbt_backup'`)
	f.observe(`EXEC sp_rename 'dbo.fct_orders__dbt_temp', 'fct_orders'`)
	f.observe(`DROP TABLE dbo.fct_orders__dbt_backup`)

	// Unchanged: the aggregate still reads fct_orders, and fct_orders is still
	// built from silver. No backup name survives anywhere.
	if got := f.edges(t); !equal(got, want) {
		t.Fatalf("after rebuild:\n got %v\nwant %v", got, want)
	}
	for _, e := range got(t, f) {
		if strings.Contains(e, "__dbt_") {
			t.Fatalf("scaffolding leaked into the graph: %q", e)
		}
	}
}

// got is a tiny helper so the assertion above reads.
func got(t *testing.T, f *lineageFixture) []string { return f.edges(t) }
