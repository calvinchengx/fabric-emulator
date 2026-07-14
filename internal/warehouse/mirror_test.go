package warehouse

import (
	"testing"
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
