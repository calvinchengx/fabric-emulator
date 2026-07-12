package warehouse

import (
	"bytes"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/clock"
	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/parquet-go/parquet-go"
)

type saleRow struct {
	Region string `parquet:"region"`
	Amount int64  `parquet:"amount"`
}

func writeParquet(t *testing.T, rows []saleRow) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := parquet.NewGenericWriter[saleRow](&buf)
	if _, err := w.Write(rows); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// numRow is an all-numeric table, so reflecting it emits no N'…' string
// literals — lets a SQLite test exercise the SQL Server ("N" prefix) code path.
type numRow struct {
	ID     int64   `parquet:"id"`
	Amount float64 `parquet:"amount"`
}

func writeNumParquet(t *testing.T, rows []numRow) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := parquet.NewGenericWriter[numRow](&buf)
	if _, err := w.Write(rows); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// seedLakehouse opens a store with a workspace + lakehouse item.
func seedLakehouse(t *testing.T) (*store.Store, string, string) {
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
	it := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lake"}
	if err := st.CreateItem(it, nil); err != nil {
		t.Fatal(err)
	}
	return st, ws.ID, it.ID
}

func put(t *testing.T, st *store.Store, wsID, itemID, rel string, content []byte) {
	t.Helper()
	if err := st.CreateOneLakePath(&store.OneLakePath{WorkspaceID: wsID, ItemID: itemID, RelPath: rel, Content: content}, false); err != nil {
		t.Fatal(err)
	}
}

func TestReadDeltaTable(t *testing.T) {
	st, wsID, itemID := seedLakehouse(t)
	put(t, st, wsID, itemID, "Tables/sales/part-0.parquet",
		writeParquet(t, []saleRow{{"us", 80}, {"eu", 60}}))
	put(t, st, wsID, itemID, "Tables/sales/_delta_log/00000000000000000000.json",
		[]byte(`{"commitInfo":{"operation":"WRITE"}}
{"add":{"path":"part-0.parquet","size":123}}`))

	tbl, err := ReadDeltaTable(st, itemID, "sales")
	if err != nil {
		t.Fatal(err)
	}
	if len(tbl.Columns) != 2 || tbl.Columns[0] != "region" || tbl.Columns[1] != "amount" {
		t.Fatalf("columns = %v", tbl.Columns)
	}
	if len(tbl.Rows) != 2 {
		t.Fatalf("rows = %d", len(tbl.Rows))
	}
	if tbl.Rows[0][0] != "us" || tbl.Rows[0][1] != int64(80) {
		t.Errorf("row0 = %v", tbl.Rows[0])
	}
	if tbl.Rows[1][0] != "eu" || tbl.Rows[1][1] != int64(60) {
		t.Errorf("row1 = %v", tbl.Rows[1])
	}
}

// TestDeltaLogSupersession: a later commit removes the first file and adds a
// second — only the active file's rows are read (Delta's add/remove semantics).
func TestDeltaLogSupersession(t *testing.T) {
	st, wsID, itemID := seedLakehouse(t)
	put(t, st, wsID, itemID, "Tables/t/part-0.parquet", writeParquet(t, []saleRow{{"old", 1}}))
	put(t, st, wsID, itemID, "Tables/t/part-1.parquet", writeParquet(t, []saleRow{{"new", 2}}))
	put(t, st, wsID, itemID, "Tables/t/_delta_log/00000000000000000000.json",
		[]byte(`{"add":{"path":"part-0.parquet"}}`))
	put(t, st, wsID, itemID, "Tables/t/_delta_log/00000000000000000001.json",
		[]byte(`{"remove":{"path":"part-0.parquet"}}
{"add":{"path":"part-1.parquet"}}`))

	tbl, err := ReadDeltaTable(st, itemID, "t")
	if err != nil {
		t.Fatal(err)
	}
	if len(tbl.Rows) != 1 || tbl.Rows[0][0] != "new" {
		t.Fatalf("expected only the active file's row, got %v", tbl.Rows)
	}
}

type mixedRow struct {
	B   bool    `parquet:"b"`
	I32 int32   `parquet:"i32"`
	F   float32 `parquet:"f"`
	D   float64 `parquet:"d"`
	S   *string `parquet:"s,optional"`
}

// TestReadDeltaMixedTypes covers the value conversions: bool, int32→int64,
// float32→float64, double, and an optional column carrying a NULL.
func TestReadDeltaMixedTypes(t *testing.T) {
	st, wsID, itemID := seedLakehouse(t)
	s := "hi"
	var buf bytes.Buffer
	w := parquet.NewGenericWriter[mixedRow](&buf)
	if _, err := w.Write([]mixedRow{
		{B: true, I32: 7, F: 1.5, D: 2.5, S: &s},
		{B: false, I32: -3, F: 0, D: 9.0, S: nil},
	}); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	put(t, st, wsID, itemID, "Tables/m/part-0.parquet", buf.Bytes())
	put(t, st, wsID, itemID, "Tables/m/_delta_log/00000000000000000000.json",
		[]byte(`{"add":{"path":"part-0.parquet"}}`))

	tbl, err := ReadDeltaTable(st, itemID, "m")
	if err != nil {
		t.Fatal(err)
	}
	r0 := tbl.Rows[0]
	if r0[0] != true || r0[1] != int64(7) || r0[2] != float64(1.5) || r0[3] != float64(2.5) || r0[4] != "hi" {
		t.Fatalf("row0 = %v (types %T %T %T %T %T)", r0, r0[0], r0[1], r0[2], r0[3], r0[4])
	}
	if tbl.Rows[1][4] != nil {
		t.Errorf("row1 optional col should be NULL, got %v", tbl.Rows[1][4])
	}
}

func TestReadDeltaTableErrors(t *testing.T) {
	st, wsID, itemID := seedLakehouse(t)
	// No _delta_log at all.
	if _, err := ReadDeltaTable(st, itemID, "missing"); err == nil {
		t.Error("expected error for a table with no commits")
	}
	// Corrupt Parquet content.
	put(t, st, wsID, itemID, "Tables/bad/part-0.parquet", []byte("not parquet"))
	put(t, st, wsID, itemID, "Tables/bad/_delta_log/00000000000000000000.json",
		[]byte(`{"add":{"path":"part-0.parquet"}}`))
	if _, err := ReadDeltaTable(st, itemID, "bad"); err == nil {
		t.Error("expected a parquet parse error")
	}
	// Malformed _delta_log line.
	put(t, st, wsID, itemID, "Tables/x/_delta_log/00000000000000000000.json", []byte(`{not json`))
	if _, err := ReadDeltaTable(st, itemID, "x"); err == nil {
		t.Error("expected a malformed-log error")
	}
	// An add referencing a data file that isn't there.
	put(t, st, wsID, itemID, "Tables/y/_delta_log/00000000000000000000.json",
		[]byte(`{"add":{"path":"gone.parquet"}}`))
	if _, err := ReadDeltaTable(st, itemID, "y"); err == nil {
		t.Error("expected a missing-data-file error")
	}
	// A commit with only a removed file → no active data.
	put(t, st, wsID, itemID, "Tables/z/_delta_log/00000000000000000000.json",
		[]byte(`{"remove":{"path":"old.parquet"}}`))
	if _, err := ReadDeltaTable(st, itemID, "z"); err == nil {
		t.Error("expected a no-active-files error")
	}
}
