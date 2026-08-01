package warehouse

import (
	"bytes"
	"context"
	"github.com/calvinchengx/fabric-emulator/internal/testsupport"
	"math/big"
	"testing"

	"github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/format"
)

// decRow exercises both physical encodings delta-rs picks for a decimal by
// precision: INT32 up to 9, INT64 up to 18. Tag is decimal(scale:precision).
type decRow struct {
	ID    int64 `parquet:"id"`
	Small int32 `parquet:"small,decimal(2:9)"`
	Price int64 `parquet:"price,decimal(2:10)"`
}

func writeDecParquet(t *testing.T, rows []decRow) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := parquet.NewGenericWriter[decRow](&buf)
	if _, err := w.Write(rows); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestDecimalString(t *testing.T) {
	cases := []struct {
		unscaled int64
		scale    int
		want     string
	}{
		{150, 2, "1.50"},
		{225, 2, "2.25"},
		{300, 2, "3.00"},
		{5, 2, "0.05"},     // needs left-padding, the classic off-by-a-digit
		{0, 2, "0.00"},     // and padding a zero
		{-150, 2, "-1.50"}, // sign must survive the padding
		{-5, 2, "-0.05"},
		{1234, 0, "1234"}, // scale 0 is a plain integer
	}
	for _, c := range cases {
		got := Decimal{Unscaled: big.NewInt(c.unscaled), Scale: c.scale, Precision: 10}.String()
		if got != c.want {
			t.Errorf("Decimal{%d, scale %d}.String() = %q, want %q", c.unscaled, c.scale, got, c.want)
		}
	}
	// A zero-value Decimal must not panic on the nil big.Int.
	if got := (Decimal{}).String(); got != "0" {
		t.Errorf("zero Decimal = %q, want %q", got, "0")
	}
}

// The unscaled integer in a byte-array decimal is big-endian two's complement,
// so a negative value is only correct if the sign extension is handled.
func TestDecimalValueByteArray(t *testing.T) {
	dec := &format.DecimalType{Scale: 2, Precision: 20}
	pos := decimalValue(parquet.FixedLenByteArrayValue([]byte{0x00, 0x96}), dec) // 150
	if pos.String() != "1.50" {
		t.Errorf("positive byte-array decimal = %q, want %q", pos.String(), "1.50")
	}
	neg := decimalValue(parquet.FixedLenByteArrayValue([]byte{0xFF, 0x6A}), dec) // -150
	if neg.String() != "-1.50" {
		t.Errorf("negative byte-array decimal = %q, want %q", neg.String(), "-1.50")
	}
}

// Regression: a decimal used to be decoded on its physical kind alone, so
// decimal(10,2) 1.50 (stored as the int64 150) came back as 150 and every
// aggregate over it was wrong by 10^scale.
func TestReadDecimalAppliesScale(t *testing.T) {
	data := writeDecParquet(t, []decRow{
		{ID: 1, Small: 150, Price: 150},
		{ID: 2, Small: -5, Price: 225},
	})
	tbl, err := ReadParquetBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := tbl.Rows[0][0].(int64); !ok || got != 1 {
		t.Errorf("plain int64 column changed: %#v", tbl.Rows[0][0])
	}
	for _, c := range []struct {
		col  int
		row  int
		want string
	}{
		{1, 0, "1.50"}, {1, 1, "-0.05"}, // INT32-backed
		{2, 0, "1.50"}, {2, 1, "2.25"}, // INT64-backed
	} {
		d, ok := tbl.Rows[c.row][c.col].(Decimal)
		if !ok {
			t.Fatalf("row %d col %d is %#v, want Decimal", c.row, c.col, tbl.Rows[c.row][c.col])
		}
		if d.String() != c.want {
			t.Errorf("row %d col %d = %q, want %q", c.row, c.col, d.String(), c.want)
		}
		if d.Scale != 2 {
			t.Errorf("row %d col %d scale = %d, want 2", c.row, c.col, d.Scale)
		}
	}
}

// The declared precision/scale must reach the SQL column, and the inserted
// literal must keep its scale — a BIGINT column here is the original bug.
func TestReflectDecimalColumn(t *testing.T) {
	if got := sqlType(&Table{Rows: [][]any{{Decimal{Unscaled: big.NewInt(150), Precision: 10, Scale: 2}}}}, 0); got != "DECIMAL(10,2)" {
		t.Errorf("sqlType = %q, want DECIMAL(10,2)", got)
	}
	// Out-of-range precision is clamped rather than emitting invalid DDL.
	if got := sqlType(&Table{Rows: [][]any{{Decimal{Unscaled: big.NewInt(1), Precision: 99, Scale: 2}}}}, 0); got != "DECIMAL(38,2)" {
		t.Errorf("clamped sqlType = %q, want DECIMAL(38,2)", got)
	}
	if got := literal(Decimal{Unscaled: big.NewInt(150), Precision: 10, Scale: 2}, "N"); got != "1.50" {
		t.Errorf("literal = %q, want 1.50", got)
	}

	db := testsupport.OpenMSSQL(t)
	tbl := &Table{
		Columns: []string{"price"},
		Rows: [][]any{
			{Decimal{Unscaled: big.NewInt(150), Precision: 10, Scale: 2}},
			{Decimal{Unscaled: big.NewInt(225), Precision: 10, Scale: 2}},
			{Decimal{Unscaled: big.NewInt(300), Precision: 10, Scale: 2}},
		},
	}
	if err := reflectTable(context.Background(), db, "orders", tbl, "N"); err != nil {
		t.Fatal(err)
	}
	var sum float64
	if err := db.QueryRow("SELECT SUM(price) FROM [orders]").Scan(&sum); err != nil {
		t.Fatal(err)
	}
	if sum != 6.75 { // 675 was the bug
		t.Errorf("SUM(price) = %v, want 6.75", sum)
	}
}
