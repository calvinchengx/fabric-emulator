package warehouse

import (
	"slices"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/clock"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

func writeStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestWriteDeltaTableAppendAndOverwrite: the writer must survive more than one
// commit — writeDeltaSnapshot only ever wrote commit zero, so a second write
// collided. Append accumulates; overwrite replaces what a reader sees.
func TestWriteDeltaTableAppendAndOverwrite(t *testing.T) {
	st := writeStore(t)
	ws := &store.Workspace{ID: "ws-1", DisplayName: "w", Type: "Workspace"}
	if err := st.CreateWorkspace(ws, store.Principal{ID: "p", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	lh := &store.Item{ID: "it-1", WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lake"}
	if err := st.CreateItem(lh, nil); err != nil {
		t.Fatal(err)
	}

	first := &Table{Columns: []string{"id", "name"}, Rows: [][]any{{int64(1), "ada"}}}
	if err := WriteDeltaTable(st, ws.ID, lh.ID, "orders", WriteOverwrite, first); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := ReadDeltaTable(st, lh.ID, "orders")
	if err != nil || len(got.Rows) != 1 {
		t.Fatalf("after create: %v rows=%v", err, got)
	}

	second := &Table{Columns: []string{"id", "name"}, Rows: [][]any{{int64(2), "grace"}}}
	if err := WriteDeltaTable(st, ws.ID, lh.ID, "orders", WriteAppend, second); err != nil {
		t.Fatalf("append: %v", err)
	}
	got, err = ReadDeltaTable(st, lh.ID, "orders")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Rows) != 2 {
		t.Fatalf("append should accumulate: got %d rows %v", len(got.Rows), got.Rows)
	}

	third := &Table{Columns: []string{"id", "name"}, Rows: [][]any{{int64(9), "only"}}}
	if err := WriteDeltaTable(st, ws.ID, lh.ID, "orders", WriteOverwrite, third); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	got, err = ReadDeltaTable(st, lh.ID, "orders")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Rows) != 1 || got.Rows[0][0] != int64(9) {
		t.Fatalf("overwrite should replace: got %v", got.Rows)
	}
}

func TestWriteDeltaTableRejectsBadInput(t *testing.T) {
	st := writeStore(t)
	if err := WriteDeltaTable(st, "w", "i", "t", WriteAppend, nil); err == nil {
		t.Fatal("nil table should error")
	}
	if err := WriteDeltaTable(st, "w", "i", "t", "merge", &Table{Columns: []string{"a"}}); err == nil {
		t.Fatal("unknown mode should error")
	}
}

// TestNextCommitVersionIgnoresNonCommits: checkpoints, CRCs and stray files in
// _delta_log must not be mistaken for commits, or the next write would collide.
func TestNextCommitVersionIgnoresNonCommits(t *testing.T) {
	st := writeStore(t)
	ws := &store.Workspace{ID: "w", DisplayName: "w", Type: "Workspace"}
	if err := st.CreateWorkspace(ws, store.Principal{ID: "p", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	lh := &store.Item{ID: "i", WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "l"}
	if err := st.CreateItem(lh, nil); err != nil {
		t.Fatal(err)
	}
	tbl := &Table{Columns: []string{"a"}, Rows: [][]any{{"x"}}}
	if err := WriteDeltaTable(st, ws.ID, lh.ID, "t", WriteOverwrite, tbl); err != nil {
		t.Fatal(err)
	}
	for _, noise := range []string{"_last_checkpoint", "00000000000000000000.checkpoint.parquet", "00000000000000000000.crc"} {
		if err := st.CreateOneLakePath(&store.OneLakePath{
			WorkspaceID: ws.ID, ItemID: lh.ID,
			RelPath: "Tables/t/_delta_log/" + noise, Content: []byte("{}"),
		}, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := WriteDeltaTable(st, ws.ID, lh.ID, "t", WriteAppend, tbl); err != nil {
		t.Fatalf("append after noise: %v", err)
	}
	got, err := ReadDeltaTable(st, lh.ID, "t")
	if err != nil || len(got.Rows) != 2 {
		t.Fatalf("append should land at v1: %v rows=%v", err, got)
	}
}

// TestWriteDeltaTableParquetRoundTrip: typed columns survive the commit, so a
// reader sees int64/float64/bool rather than everything stringified.
func TestWriteDeltaTableTypedColumns(t *testing.T) {
	st := writeStore(t)
	ws := &store.Workspace{ID: "w", DisplayName: "w", Type: "Workspace"}
	if err := st.CreateWorkspace(ws, store.Principal{ID: "p", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	lh := &store.Item{ID: "i", WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "l"}
	if err := st.CreateItem(lh, nil); err != nil {
		t.Fatal(err)
	}
	tbl := &Table{Columns: []string{"n", "f", "b", "s"}, Rows: [][]any{{int64(7), 1.5, true, "x"}}}
	if err := WriteDeltaTable(st, ws.ID, lh.ID, "typed", WriteOverwrite, tbl); err != nil {
		t.Fatal(err)
	}
	got, err := ReadDeltaTable(st, lh.ID, "typed")
	if err != nil {
		t.Fatal(err)
	}
	// The declared column order survives the round trip. encodeParquet builds
	// the PHYSICAL schema from a map (so the Parquet fields are alphabetical),
	// but Delta resolves data files to metaData.schemaString by name — so every
	// reader, ours included, must present the logical order.
	if want := []string{"n", "f", "b", "s"}; !slices.Equal(got.Columns, want) {
		t.Fatalf("column order = %v, want the declared %v", got.Columns, want)
	}
	row := got.Rows[0]
	if row[0] != int64(7) || row[1] != 1.5 || row[2] != true || row[3] != "x" {
		t.Fatalf("types not preserved: %#v", row)
	}
}

// TestReadDeltaTableUsesSchemaOrderNotParquetOrder: the physical Parquet field
// order is an implementation detail. A reader that trusts it disagrees with
// Spark/delta-rs about `SELECT *`, and with the SQL analytics endpoint that
// reflects through this reader.
func TestReadDeltaTableUsesSchemaOrderNotParquetOrder(t *testing.T) {
	st := writeStore(t)
	ws := &store.Workspace{ID: "w", DisplayName: "w", Type: "Workspace"}
	if err := st.CreateWorkspace(ws, store.Principal{ID: "p", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	lh := &store.Item{ID: "i", WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "l"}
	if err := st.CreateItem(lh, nil); err != nil {
		t.Fatal(err)
	}
	// Declared order is deliberately anti-alphabetical.
	tbl := &Table{Columns: []string{"zebra", "apple", "mango"}, Rows: [][]any{{"z", "a", "m"}}}
	if err := WriteDeltaTable(st, ws.ID, lh.ID, "ord", WriteOverwrite, tbl); err != nil {
		t.Fatal(err)
	}
	got, err := ReadDeltaTable(st, lh.ID, "ord")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Columns, []string{"zebra", "apple", "mango"}) {
		t.Fatalf("column order = %v, want declared order", got.Columns)
	}
	if got.Rows[0][0] != "z" || got.Rows[0][1] != "a" || got.Rows[0][2] != "m" {
		t.Fatalf("values must follow their names: %v", got.Rows[0])
	}
}

// TestWriteDeltaTableAllNullColumn: a column with no non-null value falls back
// to string rather than failing.
func TestWriteDeltaTableAllNullColumn(t *testing.T) {
	st := writeStore(t)
	ws := &store.Workspace{ID: "w", DisplayName: "w", Type: "Workspace"}
	if err := st.CreateWorkspace(ws, store.Principal{ID: "p", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	lh := &store.Item{ID: "i", WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "l"}
	if err := st.CreateItem(lh, nil); err != nil {
		t.Fatal(err)
	}
	tbl := &Table{Columns: []string{"a", "b"}, Rows: [][]any{{nil, "x"}}}
	if err := WriteDeltaTable(st, ws.ID, lh.ID, "nulls", WriteOverwrite, tbl); err != nil {
		t.Fatalf("all-null column: %v", err)
	}
	if _, err := ReadDeltaTable(st, lh.ID, "nulls"); err != nil {
		t.Fatalf("read back: %v", err)
	}
}
