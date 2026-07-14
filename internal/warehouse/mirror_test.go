package warehouse

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

// TestMirrorDeltaRoundTrip: the mirror writer produces a Delta table (Parquet +
// _delta_log commit) that reads back — through this package's own Delta reader —
// with the same columns, types, and rows (NULLs included). Parquet is
// name-addressed, so the comparison is by column name, not position.
func TestMirrorDeltaRoundTrip(t *testing.T) {
	st, wsID, itemID := seedLakehouse(t)

	tbl := &Table{
		Columns: []string{"id", "amount", "active", "note"},
		Rows: [][]any{
			{int64(1), 10.5, true, "a"},
			{int64(2), 20.5, false, nil}, // NULL note
			{nil, nil, nil, "c"},         // NULL numerics/bool
		},
	}
	kinds := []colKind{kindLong, kindDouble, kindBool, kindString}
	if err := writeDeltaSnapshot(st, wsID, itemID, "sales", tbl, kinds); err != nil {
		t.Fatalf("writeDeltaSnapshot: %v", err)
	}

	got, err := ReadDeltaTable(st, itemID, "sales")
	if err != nil {
		t.Fatalf("ReadDeltaTable: %v", err)
	}
	if len(got.Rows) != len(tbl.Rows) {
		t.Fatalf("row count = %d, want %d", len(got.Rows), len(tbl.Rows))
	}
	// Build column-name → index for both, and compare each row by name.
	gi := map[string]int{}
	for i, c := range got.Columns {
		gi[c] = i
	}
	for _, c := range tbl.Columns {
		if _, ok := gi[c]; !ok {
			t.Fatalf("mirrored table missing column %q (got %v)", c, got.Columns)
		}
	}
	for r := range tbl.Rows {
		for wc, want := range tbl.Rows[r] {
			name := tbl.Columns[wc]
			have := got.Rows[r][gi[name]]
			if !sameCell(want, have) {
				t.Errorf("row %d col %q = %#v (%T), want %#v (%T)", r, name, have, have, want, want)
			}
		}
	}
}

func TestMirrorEmptyTable(t *testing.T) {
	st, wsID, itemID := seedLakehouse(t)
	tbl := &Table{Columns: []string{"id"}, Rows: nil}
	if err := writeDeltaSnapshot(st, wsID, itemID, "empty", tbl, []colKind{kindLong}); err != nil {
		t.Fatalf("writeDeltaSnapshot(empty): %v", err)
	}
	got, err := ReadDeltaTable(st, itemID, "empty")
	if err != nil {
		t.Fatalf("ReadDeltaTable(empty): %v", err)
	}
	if len(got.Rows) != 0 {
		t.Fatalf("empty table read %d rows", len(got.Rows))
	}
}

func TestKindInference(t *testing.T) {
	cases := []struct {
		v    any
		want colKind
	}{
		{int64(1), kindLong}, {int32(1), kindLong}, {int(1), kindLong},
		{1.5, kindDouble}, {float32(1.5), kindDouble},
		{true, kindBool},
		{"x", kindString}, {[]byte("x"), kindString},
	}
	for _, c := range cases {
		if got := kindOf(c.v); got != c.want {
			t.Errorf("kindOf(%T) = %d, want %d", c.v, got, c.want)
		}
	}
}

// sameCell compares two mirrored cell values, tolerating the int/float widening
// the round-trip is expected to preserve exactly for our written types.
func sameCell(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	switch av := a.(type) {
	case int64:
		bv, ok := b.(int64)
		return ok && av == bv
	case float64:
		bv, ok := b.(float64)
		return ok && av == bv
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	default:
		return a == b
	}
}

// TestReadSQLTable covers readSQLTable (and kindOf/coerce's real-driver path)
// against an in-memory SQLite database — the same generic `SELECT *` + Scan
// pattern a real SQL Server driver uses, without needing one for this bit.
// (listBaseTables/Mirror's INFORMATION_SCHEMA query is SQL-Server-specific and
// is exercised end-to-end only against a real engine, by the gated e2es in
// internal/server and internal/api.)
func TestReadSQLTable(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "CREATE TABLE [people] (id INTEGER, ratio REAL, active INTEGER, name TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO people VALUES (1, 1.5, 1, 'ada'), (2, NULL, 0, NULL), (NULL, NULL, NULL, 'c')"); err != nil {
		t.Fatal(err)
	}

	tbl, kinds, err := readSQLTable(ctx, db, "people")
	if err != nil {
		t.Fatalf("readSQLTable: %v", err)
	}
	if len(tbl.Rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(tbl.Rows))
	}
	want := map[string]colKind{"id": kindLong, "ratio": kindDouble, "active": kindLong, "name": kindString}
	for i, c := range tbl.Columns {
		if kinds[i] != want[c] {
			t.Errorf("column %q kind = %d, want %d", c, kinds[i], want[c])
		}
	}
}

// TestReadSQLTableQueryError: a query error (e.g. a missing table) surfaces,
// not a panic.
func TestReadSQLTableQueryError(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, _, err := readSQLTable(context.Background(), db, "no_such_table"); err == nil {
		t.Error("expected an error reading a missing table")
	}
}

// TestCoerce covers coerce's type-mismatch fallback branches (a value that
// doesn't match its inferred kind is passed through unchanged).
func TestCoerce(t *testing.T) {
	if coerce(nil, kindLong) != nil {
		t.Error("coerce(nil) should stay nil regardless of kind")
	}
	if v := coerce("not-a-number", kindLong); v != "not-a-number" {
		t.Errorf("coerce mismatched kindLong = %v, want passthrough", v)
	}
	if v := coerce("not-a-number", kindDouble); v != "not-a-number" {
		t.Errorf("coerce mismatched kindDouble = %v, want passthrough", v)
	}
	if v := coerce(123, kindBool); v != 123 {
		t.Errorf("coerce mismatched kindBool = %v, want passthrough", v)
	}
	if v := coerce(int32(5), kindLong); v != int64(5) {
		t.Errorf("coerce int32->kindLong = %v (%T), want int64(5)", v, v)
	}
	if v := coerce(float32(1.5), kindDouble); v != float64(1.5) {
		t.Errorf("coerce float32->kindDouble = %v (%T), want float64(1.5)", v, v)
	}
}
