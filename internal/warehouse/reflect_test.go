package warehouse

import (
	"context"
	"database/sql"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/testsupport"
)

// TestReflectTable materialises a Delta table into a real SQL Server and reads
// it back: inferred types, literal encoding (strings with quotes, NULLs,
// bool/int/float), and row count.
func TestReflectTable(t *testing.T) {
	db := testsupport.OpenMSSQL(t)
	ctx := context.Background()

	tbl := &Table{
		Columns: []string{"region", "amount", "ratio", "active"},
		Rows: [][]any{
			{"us", int64(80), 1.5, true},
			{"o'brien", int64(60), 2.5, false}, // quote must be escaped
			{nil, int64(0), 0.0, nil},          // NULLs
		},
	}
	if err := reflectTable(ctx, db, "sales", tbl); err != nil {
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

// TestSQLType: the DDL side of reflection. The literal-encoding half of this
// test went with literal() — it rendered values into INSERT text, which nothing
// does any more; bulk copy hands the encoder Go values directly. What survives
// is type inference, which still decides every reflected column's DDL.
func TestSQLType(t *testing.T) {
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

	db := testsupport.OpenMSSQL(t)
	ctx := context.Background()

	done, err := Reflect(ctx, db, st, itemID)
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

// TestReflect exercises the exported Reflect (which forces the SQL Server "N"
// Unicode string prefix). A numeric-only table keeps the generated literals
// valid on the SQLite test engine, covering the production entry point.
func TestReflect(t *testing.T) {
	st, wsID, itemID := seedLakehouse(t)
	put(t, st, wsID, itemID, "Tables/metrics/part-0.parquet",
		writeNumParquet(t, []numRow{{1, 10.5}, {2, 20.5}}))
	put(t, st, wsID, itemID, "Tables/metrics/_delta_log/00000000000000000000.json",
		[]byte(`{"add":{"path":"part-0.parquet"}}`))
	// A loose file directly under Tables/ (not a directory) is skipped.
	put(t, st, wsID, itemID, "Tables/loose.txt", []byte("x"))

	db := testsupport.OpenMSSQL(t)
	ctx := context.Background()

	done, err := Reflect(ctx, db, st, itemID)
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 1 || done[0] != "metrics" {
		t.Fatalf("reflected = %v, want [metrics]", done)
	}
	var sum float64
	if err := db.QueryRowContext(ctx, "SELECT SUM(amount) FROM [metrics]").Scan(&sum); err != nil {
		t.Fatal(err)
	}
	if sum < 30.9 || sum > 31.1 {
		t.Fatalf("reflected SUM(amount) = %v, want 31", sum)
	}
}

// TestReflectListError: a store error listing Tables/ surfaces, not panics.
func TestReflectListError(t *testing.T) {
	st, _, itemID := seedLakehouse(t)
	st.Close() // a closed store makes ListOneLakePaths fail
	db := testsupport.OpenMSSQL(t)
	if _, err := Reflect(context.Background(), db, st, itemID); err == nil {
		t.Fatal("expected an error from the closed store")
	}
}
