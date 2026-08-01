package warehouse

import (
	"context"
	"database/sql"
	"math/big"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/testsupport"
)

// bulkInsert is how every reflected table is loaded in production, and until
// the SQLite double was removed it had no test at all — SQLite has no bulk
// protocol, so the branch was never taken. These tests exercise it directly
// rather than through reflectTable, so each behaviour can be forced on its own.

// TestBulkInsertShortRow: a row with fewer values than the table has columns.
//
// Not a contrived case — a Delta table whose schema gained a column has exactly
// this shape, with older data files short by one. The missing tail must land as
// NULL rather than shifting the remaining values left or failing the load.
func TestBulkInsertShortRow(t *testing.T) {
	db := testsupport.OpenMSSQL(t)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx,
		"CREATE TABLE [short] (a NVARCHAR(20), b BIGINT, c NVARCHAR(20))"); err != nil {
		t.Fatal(err)
	}
	tbl := &Table{
		Columns: []string{"a", "b", "c"},
		Rows: [][]any{
			{"full", int64(1), "tail"},
			{"short"},             // two columns missing
			{"partial", int64(2)}, // one column missing
		},
	}
	if err := bulkInsert(ctx, db, "[short]", tbl); err != nil {
		t.Fatalf("bulkInsert: %v", err)
	}

	rows, err := db.QueryContext(ctx, "SELECT a, b, c FROM [short] ORDER BY a")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type got struct {
		a string
		b sql.NullInt64
		c sql.NullString
	}
	var out []got
	for rows.Next() {
		var g got
		if err := rows.Scan(&g.a, &g.b, &g.c); err != nil {
			t.Fatal(err)
		}
		out = append(out, g)
	}
	if len(out) != 3 {
		t.Fatalf("rows = %d, want 3", len(out))
	}
	// Ordered by a: full, partial, short.
	if out[0].a != "full" || !out[0].b.Valid || out[0].b.Int64 != 1 || out[0].c.String != "tail" {
		t.Errorf("full row = %+v", out[0])
	}
	if out[1].a != "partial" || !out[1].b.Valid || out[1].b.Int64 != 2 || out[1].c.Valid {
		t.Errorf("partial row should have a NULL tail, got %+v", out[1])
	}
	if out[2].a != "short" || out[2].b.Valid || out[2].c.Valid {
		t.Errorf("short row should have two NULLs, got %+v", out[2])
	}
}

// TestBulkInsertKeepsNulls: a NULL in the Delta must land as NULL, not as the
// column's DEFAULT. That is what BulkOptions.KeepNulls buys, and without a test
// the option could be dropped and only a consumer would notice.
func TestBulkInsertKeepsNulls(t *testing.T) {
	db := testsupport.OpenMSSQL(t)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx,
		"CREATE TABLE [nulls] (id BIGINT, note NVARCHAR(20) DEFAULT 'defaulted')"); err != nil {
		t.Fatal(err)
	}
	tbl := &Table{Columns: []string{"id", "note"}, Rows: [][]any{{int64(1), nil}}}
	if err := bulkInsert(ctx, db, "[nulls]", tbl); err != nil {
		t.Fatalf("bulkInsert: %v", err)
	}
	var note sql.NullString
	if err := db.QueryRowContext(ctx, "SELECT note FROM [nulls]").Scan(&note); err != nil {
		t.Fatal(err)
	}
	if note.Valid {
		t.Errorf("NULL was replaced by the column default (%q); KeepNulls is not in effect", note.String)
	}
}

// TestBulkInsertDecimalKeepsScale: a Decimal goes over the wire as its exact
// decimal string, so the destination column's scale survives. Sending a float64
// instead is the specific mistake sqlType's comment warns about — every
// aggregate would then be wrong by a power of ten.
func TestBulkInsertDecimalKeepsScale(t *testing.T) {
	db := testsupport.OpenMSSQL(t)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, "CREATE TABLE [money] (amount DECIMAL(12,4))"); err != nil {
		t.Fatal(err)
	}
	// 1234.5678 as an unscaled integer with scale 4.
	dec := Decimal{Unscaled: big.NewInt(12345678), Precision: 12, Scale: 4}
	tbl := &Table{Columns: []string{"amount"}, Rows: [][]any{{dec}}}
	if err := bulkInsert(ctx, db, "[money]", tbl); err != nil {
		t.Fatalf("bulkInsert: %v", err)
	}
	var got string
	if err := db.QueryRowContext(ctx,
		"SELECT CONVERT(NVARCHAR(50), amount) FROM [money]").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "1234.5678" {
		t.Errorf("amount = %s, want 1234.5678 (scale lost in the bulk encoder)", got)
	}
}

// TestBulkInsertWideTable: the shape that motivated bulk copy in the first
// place. A hundred columns is where the old literal-INSERT path degraded
// sharply, so the load path is asserted at that width rather than only at the
// three or four columns the other tests use.
func TestBulkInsertWideTable(t *testing.T) {
	db := testsupport.OpenMSSQL(t)
	ctx := context.Background()

	const cols, rowCount = 100, 500
	names := make([]string, cols)
	defs := make([]string, cols)
	for i := range names {
		names[i] = "c" + itoa(i)
		defs[i] = "[" + names[i] + "] NVARCHAR(20)"
	}
	if _, err := db.ExecContext(ctx,
		"CREATE TABLE [wide] ("+strings.Join(defs, ",")+")"); err != nil {
		t.Fatal(err)
	}
	tbl := &Table{Columns: names, Rows: make([][]any, rowCount)}
	for r := range tbl.Rows {
		row := make([]any, cols)
		for c := range row {
			row[c] = "v" + itoa(c)
		}
		tbl.Rows[r] = row
	}
	if err := bulkInsert(ctx, db, "[wide]", tbl); err != nil {
		t.Fatalf("bulkInsert: %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM [wide]").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != rowCount {
		t.Fatalf("rows = %d, want %d", n, rowCount)
	}
	var last string
	if err := db.QueryRowContext(ctx, "SELECT TOP 1 [c99] FROM [wide]").Scan(&last); err != nil {
		t.Fatal(err)
	}
	if last != "v99" {
		t.Errorf("last column = %q, want v99 — a column shifted", last)
	}
}

// TestBulkInsertErrors: the failure paths return the error rather than
// panicking or committing a partial load.
func TestBulkInsertErrors(t *testing.T) {
	ctx := context.Background()
	tbl := &Table{Columns: []string{"a"}, Rows: [][]any{{"x"}}}

	t.Run("closed pool", func(t *testing.T) {
		db := testsupport.OpenMSSQL(t)
		db.Close() // BeginTx cannot start
		if err := bulkInsert(ctx, db, "[whatever]", tbl); err == nil {
			t.Error("bulkInsert on a closed pool succeeded")
		}
	})

	t.Run("table does not exist", func(t *testing.T) {
		db := testsupport.OpenMSSQL(t)
		// CopyIn probes the destination's metadata before streaming, so Prepare
		// is where a missing table surfaces.
		if err := bulkInsert(ctx, db, "[nope]", tbl); err == nil {
			t.Error("bulkInsert into a missing table succeeded")
		}
	})

	t.Run("value the column cannot take", func(t *testing.T) {
		db := testsupport.OpenMSSQL(t)
		if _, err := db.ExecContext(ctx, "CREATE TABLE [typed] (n BIGINT)"); err != nil {
			t.Fatal(err)
		}
		bad := &Table{Columns: []string{"n"}, Rows: [][]any{{"not a number"}}}
		if err := bulkInsert(ctx, db, "[typed]", bad); err == nil {
			t.Error("bulkInsert accepted a string for a BIGINT column")
		}
		// And nothing was committed.
		var n int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM [typed]").Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("a failed bulk load left %d row(s) behind; the transaction did not roll back", n)
		}
	})
}

// itoa avoids pulling strconv in for two call sites.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [4]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	return string(b[n:])
}
