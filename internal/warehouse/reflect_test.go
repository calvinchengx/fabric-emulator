package warehouse

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

// TestReflectTable materialises a Delta table into a real database/sql engine
// (SQLite — same code path SQL Server uses) and reads it back: inferred types,
// literal encoding (strings with quotes, NULLs, bool/int/float), and row count.
func TestReflectTable(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	tbl := &Table{
		Columns: []string{"region", "amount", "ratio", "active"},
		Rows: [][]any{
			{"us", int64(80), 1.5, true},
			{"o'brien", int64(60), 2.5, false}, // quote must be escaped
			{nil, int64(0), 0.0, nil},          // NULLs
		},
	}
	if err := reflectTable(ctx, db, "sales", tbl, ""); err != nil {
		t.Fatal(err)
	}

	// Aggregate over the reflected table to prove the values landed.
	var total int64
	if err := db.QueryRowContext(ctx, "SELECT SUM(amount) FROM [sales]").Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 140 {
		t.Fatalf("SUM(amount) = %d, want 140", total)
	}
	var region sql.NullString
	if err := db.QueryRowContext(ctx, "SELECT region FROM [sales] WHERE amount = 60").Scan(&region); err != nil {
		t.Fatal(err)
	}
	if region.String != "o'brien" {
		t.Fatalf("escaped string = %q", region.String)
	}
	var n int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM [sales] WHERE region IS NULL").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("NULL region count = %d, want 1", n)
	}
}

func TestLiteralAndSQLType(t *testing.T) {
	cases := []struct {
		v       any
		nprefix string
		want    string
	}{
		{nil, "N", "NULL"},
		{true, "N", "1"},
		{false, "", "0"},
		{int64(42), "", "42"},
		{3.5, "", "3.5"},
		{[]byte{0x01, 0xAB}, "N", "0x01ab"},
		{"a'b", "N", "N'a''b'"},
		{"x", "", "'x'"},
		{int32(7), "N", "N'7'"}, // unhandled type → default text
	}
	for _, c := range cases {
		if got := literal(c.v, c.nprefix); got != c.want {
			t.Errorf("literal(%v,%q) = %q, want %q", c.v, c.nprefix, got, c.want)
		}
	}

	// sqlType picks a type from the first non-null value.
	types := map[string]*Table{
		"BIT":             {Rows: [][]any{{nil}, {true}}},
		"BIGINT":          {Rows: [][]any{{int64(1)}}},
		"FLOAT":           {Rows: [][]any{{1.5}}},
		"VARBINARY(4000)": {Rows: [][]any{{[]byte{1}}}},
		"NVARCHAR(4000)":  {Rows: [][]any{{nil}}}, // all-null → default
	}
	for want, tbl := range types {
		if got := sqlType(tbl, 0); got != want {
			t.Errorf("sqlType = %q, want %q", got, want)
		}
	}
}

// TestReflectFromOneLake wires the two halves: a real Delta table in OneLake is
// read and reflected into the engine, and a SELECT returns its data.
func TestReflectFromOneLake(t *testing.T) {
	st, wsID, itemID := seedLakehouse(t)
	put(t, st, wsID, itemID, "Tables/sales/part-0.parquet",
		writeParquet(t, []saleRow{{"us", 80}, {"eu", 60}}))
	put(t, st, wsID, itemID, "Tables/sales/_delta_log/00000000000000000000.json",
		[]byte(`{"add":{"path":"part-0.parquet"}}`))
	// A non-table folder under Tables/ is skipped, not fatal.
	put(t, st, wsID, itemID, "Tables/notatable/readme.txt", []byte("hi"))

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	done, err := reflect(ctx, db, st, itemID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 1 || done[0] != "sales" {
		t.Fatalf("reflected = %v, want [sales]", done)
	}
	var got int64
	if err := db.QueryRowContext(ctx,
		"SELECT SUM(amount) FROM [sales] WHERE region IN ('us','eu')").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != 140 {
		t.Fatalf("reflected SUM = %d, want 140", got)
	}
}
