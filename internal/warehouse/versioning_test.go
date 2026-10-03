package warehouse

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/clock"
	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/calvinchengx/fabric-emulator/internal/testsupport"
)

const day = int64(24 * 3600)

// versionedWarehouse is a Warehouse item on a frozen emulator clock, far from
// real "now", so a stamp from the wall clock could not pass by coincidence.
func versionedWarehouse(t *testing.T) (*store.Store, *clock.Clock, string, string) {
	t.Helper()
	clk := clock.New()
	clk.Freeze()
	clk.Advance(day * 400)
	st, err := store.Open("", clk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ws := &store.Workspace{ID: "ws-ver", DisplayName: "w", Type: "Workspace"}
	if err := st.CreateWorkspace(ws, store.Principal{ID: "p", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	wh := &store.Item{ID: "wh-ver", WorkspaceID: ws.ID, Type: "Warehouse", DisplayName: "wh"}
	if err := st.CreateItem(wh, nil); err != nil {
		t.Fatal(err)
	}
	return st, clk, ws.ID, wh.ID
}

func idTable(vals ...int64) (*Table, []colType) {
	tbl := &Table{Columns: []string{"id"}}
	for _, v := range vals {
		tbl.Rows = append(tbl.Rows, []any{v})
	}
	return tbl, []colType{{kind: kindLong}}
}

func commit(t *testing.T, st *store.Store, ws, item, name string, vals ...int64) {
	t.Helper()
	tbl, kinds := idTable(vals...)
	if err := commitVersion(st, ws, item, name, tbl, kinds); err != nil {
		t.Fatalf("commitVersion: %v", err)
	}
}

// Each commit supersedes the last as the table's contents and keeps it as a
// version: the whole point of Phase 4, with no SQL Server involved.
func TestCommitVersionBuildsAHistory(t *testing.T) {
	st, clk, ws, wh := versionedWarehouse(t)
	commit(t, st, ws, wh, "orders", 1, 2)
	v0 := clk.Now()
	clk.Advance(3600)
	commit(t, st, ws, wh, "orders", 1, 2, 3)
	v1 := clk.Now()
	clk.Advance(3600)
	commit(t, st, ws, wh, "orders") // emptied: TRUNCATE is a version too
	v2 := clk.Now()

	for _, c := range []struct {
		at   int64
		want []int64
	}{{v0, []int64{1, 2}}, {v1, []int64{1, 2, 3}}, {v1 + 1800, []int64{1, 2, 3}}, {v2, []int64{}}} {
		if got := asOfIDs(t, st, wh, "orders", time.Unix(c.at, 0)); !sameIDs(got, c.want) {
			t.Errorf("as of %d = %v, want %v", c.at, got, c.want)
		}
	}
	// Every version restates the schema: an ALTER TABLE changes columns with no
	// file to remove, and a replay stopped there must see them.
	for _, v := range []string{"00000000000000000000", "00000000000000000001", "00000000000000000002"} {
		if a := commitActions(t, st, wh, "orders", v); a["metaData"] == nil {
			t.Errorf("commit %s does not restate the schema", v)
		}
	}
}

func TestCommitVersionFollowsASchemaChange(t *testing.T) {
	st, clk, ws, wh := versionedWarehouse(t)
	commit(t, st, ws, wh, "t", 1)
	v0 := clk.Now()
	clk.Advance(60)
	tbl := &Table{Columns: []string{"id", "extra"}, Rows: [][]any{{int64(1), "x"}}}
	if err := commitVersion(st, ws, wh, "t", tbl, []colType{{kind: kindLong}, {kind: kindString}}); err != nil {
		t.Fatal(err)
	}
	old, err := ReadDeltaTableAsOf(st, wh, "t", time.Unix(v0, 0))
	if err != nil || len(old.Columns) != 1 {
		t.Fatalf("as of v0 = %+v, %v; want the one-column schema", old, err)
	}
	cur, err := ReadDeltaTable(st, wh, "t")
	if err != nil || len(cur.Columns) != 2 {
		t.Fatalf("now = %+v, %v; want two columns", cur, err)
	}
}

// DROP ends the history; RENAME carries it. This is Fabric's behaviour and
// what makes a dbt rebuild (build a temp, swap it in) start the new table's
// history at the swap.
func TestDropEndsAndRenameCarriesHistory(t *testing.T) {
	st, clk, ws, wh := versionedWarehouse(t)
	commit(t, st, ws, wh, "stage__dbt_temp", 1)
	v0 := clk.Now()
	clk.Advance(60)
	commit(t, st, ws, wh, "stage__dbt_temp", 1, 2)

	if err := RenameTableHistory(st, wh, "STAGE__DBT_TEMP", "stage"); err != nil { // CI match
		t.Fatal(err)
	}
	if got := asOfIDs(t, st, wh, "stage", time.Unix(v0, 0)); !sameIDs(got, []int64{1}) {
		t.Errorf("renamed table as of v0 = %v, want its past under the new name", got)
	}
	if _, err := ReadDeltaTable(st, wh, "stage__dbt_temp"); err == nil {
		t.Error("the old name still has a table")
	}
	if err := DropTableHistory(st, wh, "stage"); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDeltaTable(st, wh, "stage"); err == nil {
		t.Error("a dropped table kept its history")
	}
	// Both are no-ops for a table with no history (made before versioning).
	if err := DropTableHistory(st, wh, "never"); err != nil {
		t.Errorf("drop of an unversioned table: %v", err)
	}
	if err := RenameTableHistory(st, wh, "never", "other"); err != nil {
		t.Errorf("rename of an unversioned table: %v", err)
	}
}

func dataFiles(t *testing.T, st *store.Store, wh, name string) []string {
	t.Helper()
	entries, err := st.ListOneLakePaths(wh, "Tables/"+name, false)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.RelPath, ".parquet") {
			out = append(out, e.RelPath[strings.LastIndex(e.RelPath, "/")+1:])
		}
	}
	return out
}

// Retention removes only the bytes no version inside the window can reach, and
// keeps the log whole so delta-rs and Spark can still replay it.
func TestExpireVersionsKeepsTheWindowAndTheLog(t *testing.T) {
	st, clk, ws, wh := versionedWarehouse(t)
	commit(t, st, ws, wh, "t", 1) // v0, day 0
	clk.Advance(10 * day)
	commit(t, st, ws, wh, "t", 1, 2) // v1, day 10
	clk.Advance(10 * day)
	commit(t, st, ws, wh, "t", 1, 2, 3) // v2, day 20
	clk.Advance(20 * day)               // now day 40

	// 30-day window at day 40: cutoff day 10. The base version is v1 (made AT
	// the cutoff), so v0's file is the only thing nothing in the window reaches.
	n, err := ExpireVersions(st, wh, "t", 30)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expired %d files, want 1 (v0's)", n)
	}
	if got := dataFiles(t, st, wh, "t"); len(got) != 2 {
		t.Fatalf("data files after expiry = %v, want v1's and v2's", got)
	}
	// The window still answers, at its edge and inside it.
	cutoff := RetentionCutoff(st, 30)
	if got := asOfIDs(t, st, wh, "t", cutoff); !sameIDs(got, []int64{1, 2}) {
		t.Errorf("as of the cutoff = %v, want v1's rows", got)
	}
	if got := asOfIDs(t, st, wh, "t", time.Unix(clk.Now(), 0)); !sameIDs(got, []int64{1, 2, 3}) {
		t.Errorf("now = %v", got)
	}
	// The log is whole: version 0 can still be replayed to by anything that
	// needs a contiguous log.
	if _, err := st.GetOneLakePath(wh, "Tables/t/_delta_log/"+commitFileName(0)); err != nil {
		t.Errorf("commit 0 was removed: %v", err)
	}
	// A second pass has nothing left to do.
	if n, err := ExpireVersions(st, wh, "t", 30); err != nil || n != 0 {
		t.Errorf("second expiry = %d, %v; want 0", n, err)
	}
}

func TestExpireVersionsLeavesAYoungTableAlone(t *testing.T) {
	st, clk, ws, wh := versionedWarehouse(t)
	commit(t, st, ws, wh, "t", 1)
	clk.Advance(day)
	commit(t, st, ws, wh, "t", 2)
	if n, err := ExpireVersions(st, wh, "t", 30); err != nil || n != 0 {
		t.Fatalf("expired %d, %v; want nothing inside the window", n, err)
	}
	if n, err := ExpireVersions(st, wh, "absent", 30); err != nil || n != 0 {
		t.Fatalf("expiry of a table with no log = %d, %v", n, err)
	}
	// A file added by an overwrite that is still live must never be expired,
	// whatever its age: the current table is not history.
	clk.Advance(100 * day)
	if n, err := ExpireVersions(st, wh, "t", 1); err != nil || n != 1 {
		t.Fatalf("expired %d, %v; want only the superseded file", n, err)
	}
	if got, err := ReadDeltaTable(st, wh, "t"); err != nil || len(got.Rows) != 1 {
		t.Fatalf("current table after expiry = %+v, %v", got, err)
	}
}

// An instant outside the window is refused by name, not with a missing-file
// error from a version whose bytes were expired.
func TestWarehouseResolverRefusesOutsideTheWindow(t *testing.T) {
	st, clk, ws, wh := versionedWarehouse(t)
	commit(t, st, ws, wh, "t", 1)
	first := clk.Now()
	clk.Advance(40 * day)
	commit(t, st, ws, wh, "t", 1, 2)

	resolve := WarehouseTimeTravelResolver(st, wh, 30)
	if _, _, err := resolve("t", time.Unix(first, 0)); err == nil ||
		!strings.Contains(err.Error(), "30-day data retention window") {
		t.Fatalf("outside the window: err = %v, want the retention refusal", err)
	}
	snap, ok, err := resolve("t", time.Unix(clk.Now(), 0))
	if err != nil || !ok || len(snap.RowLiterals) != 2 {
		t.Fatalf("inside the window = %+v, %v, %v", snap, ok, err)
	}
	// A wider window reaches the same instant: the bound is configuration.
	if _, ok, err := WarehouseTimeTravelResolver(st, wh, 120)("t", time.Unix(first, 0)); err != nil || !ok {
		t.Fatalf("120-day window = %v, %v", ok, err)
	}
}

// SnapshotTable against a real SQL Server (skipped without WAREHOUSE_MSSQL_DSN).
func TestSnapshotTableVersionsARealTable(t *testing.T) {
	db := testsupport.OpenMSSQL(t)
	st, clk, _, wh := versionedWarehouse(t)
	ctx := context.Background()
	exec := func(q string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	snap := func(schema, name string) (string, string) {
		t.Helper()
		actual, skipped, err := SnapshotTable(ctx, db, st, wh, schema, name)
		if err != nil {
			t.Fatalf("SnapshotTable(%s.%s): %v", schema, name, err)
		}
		return actual, skipped
	}

	exec(`CREATE TABLE dbo.Customer (id bigint, name nvarchar(20), balance decimal(10,2))`)
	if actual, skipped := snap("dbo", "Customer"); actual != "Customer" || skipped != "" {
		t.Fatalf("empty table = %q, %q", actual, skipped)
	}
	v0 := clk.Now()
	clk.Advance(60)
	exec(`INSERT INTO dbo.Customer VALUES (1, N'ann', 10.50), (2, NULL, 0.25)`)
	// The engine's own spelling is the folder, whatever the statement wrote.
	if actual, _ := snap("", "customer"); actual != "Customer" {
		t.Fatalf("actual name = %q, want the engine's spelling", actual)
	}
	v1 := clk.Now()
	clk.Advance(60)
	exec(`UPDATE dbo.Customer SET name = N'zed' WHERE id = 1`)
	snap("dbo", "Customer")

	old, err := ReadDeltaTableAsOf(st, wh, "Customer", time.Unix(v1, 0))
	if err != nil || len(old.Rows) != 2 {
		t.Fatalf("as of v1 = %+v, %v", old, err)
	}
	empty, err := ReadDeltaTableAsOf(st, wh, "Customer", time.Unix(v0, 0))
	if err != nil || len(empty.Rows) != 0 || len(empty.Columns) != 3 {
		t.Fatalf("as of v0 = %+v, %v; want an empty table with its columns", empty, err)
	}
	cur, err := ReadDeltaTable(st, wh, "Customer")
	if err != nil {
		t.Fatal(err)
	}
	var names []any
	for _, r := range cur.Rows {
		names = append(names, r[1])
	}
	if len(cur.Rows) != 2 || (names[0] != "zed" && names[1] != "zed") {
		t.Fatalf("current rows = %v", cur.Rows)
	}
	// The decimal keeps its scale (a kind alone would drop it).
	for _, r := range cur.Rows {
		if d, ok := r[2].(Decimal); !ok || !strings.Contains(d.String(), ".") {
			t.Errorf("balance = %#v, want a scaled Decimal", r[2])
		}
	}

	// Skips are reported, never silent and never errors.
	exec(`CREATE SCHEMA other`)
	exec(`CREATE TABLE other.t (a int)`)
	if _, skipped := snap("other", "t"); !strings.Contains(skipped, "only dbo") {
		t.Errorf("non-dbo schema skipped = %q", skipped)
	}
	exec(`CREATE VIEW dbo.v AS SELECT id FROM dbo.Customer`)
	if _, skipped := snap("dbo", "v"); skipped != "not a base table" {
		t.Errorf("a view skipped = %q", skipped)
	}
	if _, skipped := snap("dbo", "nothing_here"); skipped != "not a base table" {
		t.Errorf("a missing table skipped = %q", skipped)
	}
	// A bracket in the name is escaped, not an injection.
	exec(`CREATE TABLE dbo.[we]]ird] (a int)`)
	if actual, skipped := snap("dbo", "we]ird"); actual != "we]ird" || skipped != "" {
		t.Errorf("bracket in a name = %q, %q", actual, skipped)
	}
}

// A foreign writer may remove a file and add the same path back later (an
// undo). Such a file is live inside the window, so expiry must keep it even
// though an old commit added it and a newer one removed it.
func TestExpireVersionsKeepsAReAddedFile(t *testing.T) {
	st, clk, ws, wh := versionedWarehouse(t)
	now := clk.Now() * 1000
	log := func(v int, ageDays int64, actions ...string) {
		body := `{"commitInfo":{"timestamp":` + strconv.FormatInt(now-ageDays*day*1000, 10) + `}}` + "\n" + strings.Join(actions, "\n") + "\n"
		put(t, st, ws, wh, "Tables/t/_delta_log/"+commitFileName(v), []byte(body))
	}
	add := `{"add":{"path":"a.parquet","modificationTime":1}}`
	rm := `{"remove":{"path":"a.parquet"}}`
	log(0, 90, add)
	log(1, 60, rm) // the base: last commit at or before the 30-day cutoff
	log(2, 5, add) // re-added, inside the window
	put(t, st, ws, wh, "Tables/t/a.parquet", []byte("x"))
	if n, err := ExpireVersions(st, wh, "t", 30); err != nil || n != 0 {
		t.Fatalf("expired %d, %v; want the re-added file kept", n, err)
	}
	if _, err := st.GetOneLakePath(wh, "Tables/t/a.parquet"); err != nil {
		t.Fatalf("the live file was deleted: %v", err)
	}
}
