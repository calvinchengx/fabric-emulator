package warehouse

import (
	"bytes"
	"context"
	"database/sql"
	"github.com/calvinchengx/fabric-emulator/internal/testsupport"
	"testing"
	"time"
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
	// int32 is kindInt, not kindLong, and []byte is kindBinary, not kindString.
	// Both used to collapse, and both collapses were one-way: a mirrored INT
	// came back a Delta long, and a varbinary came back a string whose bytes
	// had already been through a Go string conversion.
	//
	// kindOf cannot tell DATE from DATETIME2 — nothing in a time.Time does —
	// so it answers timestamp and kindFromSQL makes the precise call from the
	// server's own column metadata.
	cases := []struct {
		v    any
		want colKind
	}{
		{int64(1), kindLong}, {int32(1), kindInt}, {int(1), kindInt},
		{1.5, kindDouble}, {float32(1.5), kindDouble},
		{true, kindBool},
		{"x", kindString}, {[]byte("x"), kindBinary},
		{time.Now(), kindTimestamp},
		{Timestamp{T: time.Now()}, kindTimestamp},
		{Date{T: time.Now()}, kindDate},
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
	db := testsupport.OpenMSSQL(t)
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
	// INTEGER is kindInt, not kindLong: the kind now comes from the driver's
	// column metadata rather than from the scanned value, and SQL Server reports
	// these as INT. Mirroring them as Delta long is what made a round-tripped
	// INT column come back a BIGINT.
	want := map[string]colKind{"id": kindInt, "ratio": kindDouble, "active": kindInt, "name": kindString}
	for i, c := range tbl.Columns {
		if kinds[i] != want[c] {
			t.Errorf("column %q kind = %d, want %d", c, kinds[i], want[c])
		}
	}
}

// TestReadSQLTableQueryError: a query error (e.g. a missing table) surfaces,
// not a panic.
func TestReadSQLTableQueryError(t *testing.T) {
	db := testsupport.OpenMSSQL(t)
	if _, _, err := readSQLTable(context.Background(), db, "no_such_table"); err == nil {
		t.Error("expected an error reading a missing table")
	}
}

// newMirrorDB opens a real SQL Server database for the mirror tests.
//
// It used to build a fake INFORMATION_SCHEMA on SQLite — ATTACH a second
// in-memory database under that name, then CREATE a TABLES table in it — so a
// test could decide what the metadata catalogue said. That is precisely the
// power a real backend does not give you: INFORMATION_SCHEMA.TABLES is a
// read-only system view. The assertions that depended on forging it are gone
// rather than re-faked; see TestMirrorErrors.
func newMirrorDB(t *testing.T) *sql.DB {
	t.Helper()
	return testsupport.OpenMSSQL(t)
}

// TestMirrorSnapshotsBaseTables: Mirror lists the base tables (views excluded),
// snapshots each to OneLake as a Delta table, and the snapshot reads back
// through this package's own Delta reader.
//
// A VARBINARY column stays BYTES. It used to be "normalized to a string", which
// is the write-direction half of the type report: the bytes went through a Go
// string conversion, the Delta schema recorded them as `string`, and nothing
// could tell afterwards that the column had ever been binary.
func TestMirrorSnapshotsBaseTables(t *testing.T) {
	st, _, itemID := seedLakehouse(t)
	db := newMirrorDB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx,
		"CREATE TABLE [orders] (id BIGINT, note NVARCHAR(50), data VARBINARY(50))"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO orders VALUES (1, 'a', 0x414243), (2, NULL, NULL)"); err != nil {
		t.Fatal(err)
	}
	// A real view, so that "views are not mirrored" is answered by SQL Server's
	// own catalogue rather than by a row the test wrote into it.
	if _, err := db.ExecContext(ctx, "CREATE VIEW [v1] AS SELECT id FROM orders"); err != nil {
		t.Fatal(err)
	}

	if err := Mirror(ctx, db, st, itemID); err != nil {
		t.Fatalf("Mirror: %v", err)
	}
	got, err := ReadDeltaTable(st, itemID, "orders")
	if err != nil {
		t.Fatalf("ReadDeltaTable: %v", err)
	}
	if len(got.Rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(got.Rows))
	}
	gi := map[string]int{}
	for i, c := range got.Columns {
		gi[c] = i
	}
	r0 := got.Rows[0]
	data, isBytes := r0[gi["data"]].([]byte)
	if r0[gi["id"]] != int64(1) || r0[gi["note"]] != "a" ||
		!isBytes || !bytes.Equal(data, []byte("ABC")) {
		t.Fatalf("row0 = %v (data is %T)", r0, r0[gi["data"]])
	}
	if got.Rows[1][gi["note"]] != nil || got.Rows[1][gi["data"]] != nil {
		t.Fatalf("row1 NULLs lost: %v", got.Rows[1])
	}
	// The view is not mirrored.
	if _, err := ReadDeltaTable(st, itemID, "v1"); err == nil {
		t.Error("view v1 was mirrored; only BASE TABLEs should be")
	}
}

// TestMirrorErrors: a missing item surfaces as a wrapped error.
//
// This test used to cover three more failures — no INFORMATION_SCHEMA at all,
// metadata naming a table that does not exist, and a NULL TABLE_NAME breaking
// the row scan. All three were reachable only because the old SQLite fixture
// let the test WRITE the metadata catalogue. SQL Server always has an
// INFORMATION_SCHEMA and its TABLES view is read-only, so none of those states
// can be produced against the real backend, and they were removed with the
// fixture rather than re-faked.
//
// The defensive branches they exercised in listBaseTables are consequently
// uncovered. That is a deliberate trade, not an oversight.
func TestMirrorErrors(t *testing.T) {
	st, _, _ := seedLakehouse(t)
	ctx := context.Background()

	db := newMirrorDB(t)
	if err := Mirror(ctx, db, st, "no-such-item"); err == nil {
		t.Error("Mirror with an unknown item succeeded")
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
	if v := coerce(int(7), kindLong); v != int64(7) {
		t.Errorf("coerce int->kindLong = %v (%T), want int64(7)", v, v)
	}
	if v := coerce(float32(1.5), kindDouble); v != float64(1.5) {
		t.Errorf("coerce float32->kindDouble = %v (%T), want float64(1.5)", v, v)
	}
}

// TestKindFromSQLDistinguishesWhatValuesCannot.
//
// This is the half of the mapping that value inference cannot do: a DATE and a
// DATETIME2 both scan as time.Time, an INT and a BIGINT both as int64. Mirroring
// on the value alone collapsed each pair, so a SQL date became a Delta timestamp
// (or worse, a string) and could never round-trip back to a date.
func TestKindFromSQLDistinguishesWhatValuesCannot(t *testing.T) {
	for name, want := range map[string]colKind{
		"DATE": kindDate, "date": kindDate,
		"DATETIME2": kindTimestamp, "DATETIME": kindTimestamp,
		"SMALLDATETIME": kindTimestamp, "DATETIMEOFFSET": kindTimestamp,
		"INT": kindInt, "SMALLINT": kindInt, "TINYINT": kindInt,
		"BIGINT": kindLong,
		"FLOAT":  kindDouble, "REAL": kindDouble,
		"BIT":       kindBool,
		"VARBINARY": kindBinary, "BINARY": kindBinary, "IMAGE": kindBinary,
		"NVARCHAR": kindString, "VARCHAR": kindString,
		"SOMETHING_NEW": kindString, // unknown types stay text rather than guessing
	} {
		if got := kindFromSQL(name); got != want {
			t.Errorf("kindFromSQL(%q) = %d, want %d", name, got, want)
		}
	}
	// The pairs that matter, stated as the property rather than the table.
	if kindFromSQL("DATE") == kindFromSQL("DATETIME2") {
		t.Error("DATE and DATETIME2 collapsed to one kind")
	}
	if kindFromSQL("INT") == kindFromSQL("BIGINT") {
		t.Error("INT and BIGINT collapsed to one kind")
	}
}
