package warehouse

// The Delta ↔ SQL type mapping, in both directions.
//
// Reported from contoso-data-platform: a Spark DateType column surfaced through
// the SQL analytics endpoint as BIGINT — the raw days-since-epoch — where real
// Fabric says `date`. Measuring it found three more of the same shape, because
// they share one cause: the reader kept only the DECIMAL logical annotation and
// discarded the rest, so date, timestamp and int all arrived as int64 and all
// three reflected as BIGINT, while binary arrived as a Go string.
//
// It fails two ways, and the quiet one is worse. A dbt model joining the column
// against a real date dies with `Operand type clash: date is incompatible with
// bigint`, naming neither the column nor the cause — loud, findable. But
// `SELECT rate_date` just returns 20627, a perfectly plausible integer that
// nothing marks as a date, and a report or a semantic model carries it straight
// through.
//
// None of this needs a SQL Server: readParquet and sqlType are pure, so these
// run in the default suite rather than behind WAREHOUSE_MSSQL_DSN.

import (
	"bytes"
	"context"
	"fmt"
	"github.com/calvinchengx/fabric-emulator/internal/testsupport"
	"strings"
	"testing"
	"time"

	"github.com/parquet-go/parquet-go"
)

// everyType is one column per type worth asserting, in the physical encodings
// Spark and delta-rs actually write.
type everyType struct {
	Dt  int32   `parquet:"dt,date"`
	Ts  int64   `parquet:"ts,timestamp(microsecond)"`
	Bin []byte  `parquet:"bin"`
	I   int32   `parquet:"i"`
	L   int64   `parquet:"l"`
	S   string  `parquet:"s"`
	F   float64 `parquet:"f"`
	B   bool    `parquet:"b"`
}

func writeEveryType(t *testing.T, row everyType) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := parquet.NewGenericWriter[everyType](&buf)
	if _, err := w.Write([]everyType{row}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func columnIndex(tbl *Table, name string) int {
	for i, c := range tbl.Columns {
		if c == name {
			return i
		}
	}
	return -1
}

// TestReflectedSQLTypesMatchFabric is the regression test for the report. Each
// expectation is the type real Fabric surfaces for that Delta type.
func TestReflectedSQLTypesMatchFabric(t *testing.T) {
	tbl, err := readParquet(writeEveryType(t, everyType{
		Dt: 20627, Ts: 1700000000000000, Bin: []byte{0x01, 0x02},
		I: 7, L: 9, S: "x", F: 1.5, B: true,
	}))
	if err != nil {
		t.Fatal(err)
	}

	for col, want := range map[string]string{
		"dt":  "DATE",
		"ts":  "DATETIME2",
		"bin": "VARBINARY(4000)",
		"i":   "INT",
		"l":   "BIGINT",
		"s":   "VARCHAR(8000)", // varchar, not nvarchar — Parquet has no unicode type
		"f":   "FLOAT",
		"b":   "BIT",
	} {
		i := columnIndex(tbl, col)
		if i < 0 {
			t.Fatalf("column %q missing from %v", col, tbl.Columns)
		}
		if got := sqlType(tbl, i); got != want {
			t.Errorf("%s: reflected as %s, want %s (Fabric's mapping) — value arrived as %T",
				col, got, want, tbl.Rows[0][i])
		}
	}
}

// TestDateDecodesToTheCalendarDayNotTheDayCount: the type name alone is not
// enough. If DATE were emitted while the value stayed 20627, the insert would
// fail or store a nonsense day — the number has to become the date it denotes.
func TestDateDecodesToTheCalendarDayNotTheDayCount(t *testing.T) {
	tbl, err := readParquet(writeEveryType(t, everyType{Dt: 20627}))
	if err != nil {
		t.Fatal(err)
	}
	v := tbl.Rows[0][columnIndex(tbl, "dt")]
	d, ok := v.(Date)
	if !ok {
		t.Fatalf("date column decoded as %T (%v); want warehouse.Date", v, v)
	}
	// 20627 days after 1970-01-01, checked against the stdlib rather than by
	// hand — the first version of this line said 2026-06-11, which is day 20615.
	if got := d.String(); got != "2026-06-23" {
		t.Errorf("Date = %s, want 2026-06-23", got)
	}
	// And what reaches the bulk-copy encoder is a time, not the day count.
	if _, ok := bulkValue(d).(time.Time); !ok {
		t.Errorf("bulkValue(Date) = %T; the destination column is DATE, so an "+
			"integer here would put the bug back one layer down", bulkValue(d))
	}
}

// TestTimestampHonoursItsUnit: the unit lives only in the annotation, so
// reading microseconds as milliseconds is silent — it lands in 1970 rather
// than failing.
func TestTimestampHonoursItsUnit(t *testing.T) {
	tbl, err := readParquet(writeEveryType(t, everyType{Ts: 1700000000000000}))
	if err != nil {
		t.Fatal(err)
	}
	v := tbl.Rows[0][columnIndex(tbl, "ts")]
	ts, ok := v.(Timestamp)
	if !ok {
		t.Fatalf("timestamp column decoded as %T; want warehouse.Timestamp", v)
	}
	if got := ts.T.UTC().Format("2006-01-02 15:04:05"); got != "2023-11-14 22:13:20" {
		t.Errorf("Timestamp = %s, want 2023-11-14 22:13:20 (micros, not millis)", got)
	}
}

// TestBinaryStaysBytes: unannotated BYTE_ARRAY is binary. Stringifying it both
// loses the distinction and can produce invalid UTF-8 in a string column —
// which is why sqlType's VARBINARY arm was unreachable for anything read from
// Parquet.
func TestBinaryStaysBytes(t *testing.T) {
	tbl, err := readParquet(writeEveryType(t, everyType{Bin: []byte{0xff, 0x00, 0xfe}}))
	if err != nil {
		t.Fatal(err)
	}
	v := tbl.Rows[0][columnIndex(tbl, "bin")]
	b, ok := v.([]byte)
	if !ok {
		t.Fatalf("binary column decoded as %T; want []byte", v)
	}
	if !bytes.Equal(b, []byte{0xff, 0x00, 0xfe}) {
		t.Errorf("binary round-trip = % x, want ff 00 fe", b)
	}
	// A UTF-8 annotated column is still text.
	if got := tbl.Rows[0][columnIndex(tbl, "s")]; got != nil {
		if _, isStr := got.(string); !isStr {
			t.Errorf("an annotated string column decoded as %T; want string", got)
		}
	}
}

// TestMirrorRoundTripPreservesLogicalTypes drives the WRITE direction: a table
// mirrored out to Delta and read back must come home as the same logical types.
// Before, colKind had no date/timestamp/binary/int at all and deltaTypeName
// collapsed everything that was not long/double/boolean to "string", so this
// was unrepresentable in both directions — the report's half that could not be
// seen from the reporting repo.
func TestMirrorRoundTripPreservesLogicalTypes(t *testing.T) {
	day := time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC)
	instant := time.Date(2023, 11, 14, 22, 13, 20, 0, time.UTC)
	tbl := &Table{
		Columns: []string{"dt", "ts", "bin", "i", "l", "s", "f", "b"},
		Rows: [][]any{{
			Date{T: day}, Timestamp{T: instant}, []byte{0x01, 0x02},
			int32(7), int64(9), "x", 1.5, true,
		}},
	}
	kinds := make([]colType, len(tbl.Columns))
	for i := range tbl.Columns {
		kinds[i] = colTypeOf(tbl.Rows[0][i])
	}

	// The Delta schema names the logical type, which is what any other reader
	// (Spark, delta-rs, DuckDB) resolves the file by.
	for i, want := range map[int]string{
		0: "date", 1: "timestamp", 2: "binary", 3: "integer",
		4: "long", 5: "string", 6: "double", 7: "boolean",
	} {
		if got := deltaTypeName(kinds[i]); got != want {
			t.Errorf("%s: Delta schema type %q, want %q", tbl.Columns[i], got, want)
		}
	}

	pq, err := encodeParquet(tbl, kinds)
	if err != nil {
		t.Fatal(err)
	}
	back, err := readParquet(pq)
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]any{}
	for i, c := range back.Columns {
		got[c] = back.Rows[0][i]
	}
	if d, ok := got["dt"].(Date); !ok || !d.T.Equal(day) {
		t.Errorf("date round-tripped as %T %v, want %v", got["dt"], got["dt"], day)
	}
	if ts, ok := got["ts"].(Timestamp); !ok || !ts.T.Equal(instant) {
		t.Errorf("timestamp round-tripped as %T %v, want %v", got["ts"], got["ts"], instant)
	}
	if b, ok := got["bin"].([]byte); !ok || !bytes.Equal(b, []byte{0x01, 0x02}) {
		t.Errorf("binary round-tripped as %T %v", got["bin"], got["bin"])
	}
	if got["i"] != int32(7) {
		t.Errorf("int round-tripped as %T %v, want int32(7)", got["i"], got["i"])
	}
	if got["l"] != int64(9) {
		t.Errorf("long round-tripped as %T %v, want int64(9)", got["l"], got["l"])
	}
}

// TestReflectedDateIsUsableAsADateInSQLServer is the report's own scenario, end
// to end against a real engine: what INFORMATION_SCHEMA says, and whether a
// join against a real date works.
//
// The unit tests above assert the type NAME sqlType chooses. This asserts the
// two things the reporter actually hit — `INFORMATION_SCHEMA.COLUMNS` reporting
// `bigint`, and `Operand type clash: date is incompatible with bigint` on a
// join — neither of which a pure-Go test can reach.
func TestReflectedDateIsUsableAsADateInSQLServer(t *testing.T) {
	db := testsupport.OpenMSSQL(t)
	ctx := context.Background()

	// The table is READ FROM PARQUET, not hand-built with Date values. That is
	// the whole chain the report describes — a Spark-written Delta file reaching
	// the analytics endpoint — and it is the only way this test can fail when
	// the reader stops decoding the DATE annotation. Built by hand, it passes
	// with the reader's date arm deleted, which is how the first version of this
	// test proved nothing.
	day := time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC)
	pq := writeEveryType(t, everyType{
		Dt: 20627, Ts: day.UnixMicro(), S: "GBP", F: 1.27, B: true,
	})
	read, err := readParquet(pq)
	if err != nil {
		t.Fatal(err)
	}
	tbl := &Table{
		Columns: []string{"rate_date", "quoted_on", "currency", "rate_to_usd", "carried"},
		Rows: [][]any{{
			read.Rows[0][columnIndex(read, "dt")],
			read.Rows[0][columnIndex(read, "ts")],
			read.Rows[0][columnIndex(read, "s")],
			read.Rows[0][columnIndex(read, "f")],
			read.Rows[0][columnIndex(read, "b")],
		}},
	}
	if err := reflectTable(ctx, db, "silver_fx_daily", tbl); err != nil {
		t.Fatalf("reflect: %v", err)
	}

	got := map[string]string{}
	rows, err := db.QueryContext(ctx,
		`SELECT COLUMN_NAME, DATA_TYPE FROM INFORMATION_SCHEMA.COLUMNS
		 WHERE TABLE_NAME = 'silver_fx_daily'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var c, dt string
		if err := rows.Scan(&c, &dt); err != nil {
			t.Fatal(err)
		}
		got[c] = dt
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	// The exact table from the report, plus the two that were already right.
	for col, want := range map[string]string{
		"rate_date": "date", "quoted_on": "datetime2",
		// varchar, per Fabric's documented Delta STRING -> varchar mapping.
		"currency": "varchar", "rate_to_usd": "float", "carried": "bit",
	} {
		if got[col] != want {
			t.Errorf("INFORMATION_SCHEMA reports %s as %q, want %q", col, got[col], want)
		}
	}

	// The loud failure: a join against a real date. This is the query that died
	// with "Operand type clash: date is incompatible with bigint".
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM silver_fx_daily f
		 JOIN (SELECT CAST('2026-06-23' AS date) AS order_date) o
		   ON f.rate_date = o.order_date`).Scan(&n); err != nil {
		t.Fatalf("joining the reflected date against a real date: %v", err)
	}
	if n != 1 {
		t.Errorf("join matched %d rows, want 1", n)
	}

	// The quiet failure: selecting it returns a date, not 20627.
	var day2 time.Time
	if err := db.QueryRowContext(ctx, "SELECT rate_date FROM silver_fx_daily").Scan(&day2); err != nil {
		t.Fatalf("selecting the reflected date: %v", err)
	}
	if !day2.UTC().Equal(day) {
		t.Errorf("rate_date = %v, want %v", day2.UTC(), day)
	}
}

// TestMirrorPreservesDecimalScale is the write-direction half of the decimal
// story. `sqlType` has guarded the READ direction since the comment there was
// written ("reflecting a decimal as BIGINT drops the scale and every aggregate
// over it is then wrong by 10^scale") — but mirroring a SQL decimal OUT to
// Delta collapsed it to a string, so the same value could not make the round
// trip at all.
//
// Each width is asserted because the physical encoding changes with precision:
// delta-rs picks INT32 to 9 digits, INT64 to 18, byte array beyond, and a
// reader resolving by annotation has to find what it expects at every one.
func TestMirrorPreservesDecimalScale(t *testing.T) {
	for _, tc := range []struct {
		name             string
		precision, scale int
		in               string
		want             string
	}{
		{"int32-backed", 9, 2, "1234.56", "1234.56"},
		{"int64-backed", 18, 4, "12345678.9012", "12345678.9012"},
		{"byte-array-backed", 30, 6, "123456789012345678.123456", "123456789012345678.123456"},
		{"negative", 12, 3, "-42.500", "-42.500"},
		{"scale padded from a shorter literal", 10, 2, "1.5", "1.50"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ct := colType{kind: kindDecimal, precision: tc.precision, scale: tc.scale}
			tbl := &Table{Columns: []string{"amt"}, Rows: [][]any{{tc.in}}}

			// The Delta schema must name the precision and scale, since that is
			// what any other reader resolves the column by.
			wantType := fmt.Sprintf("decimal(%d,%d)", tc.precision, tc.scale)
			if got := deltaTypeName(ct); got != wantType {
				t.Fatalf("Delta type = %q, want %q", got, wantType)
			}

			pq, err := encodeParquet(tbl, []colType{ct})
			if err != nil {
				t.Fatal(err)
			}
			back, err := readParquet(pq)
			if err != nil {
				t.Fatal(err)
			}
			d, ok := back.Rows[0][0].(Decimal)
			if !ok {
				t.Fatalf("round-tripped as %T (%v); want Decimal — a decimal that "+
					"comes back a string or an int has lost its scale",
					back.Rows[0][0], back.Rows[0][0])
			}
			if got := d.String(); got != tc.want {
				t.Errorf("round-trip = %s, want %s", got, tc.want)
			}
			// And it reflects back to SQL as the same declared type.
			if got := sqlType(back, 0); got != fmt.Sprintf("DECIMAL(%d,%d)", tc.precision, tc.scale) {
				t.Errorf("reflected as %s, want DECIMAL(%d,%d)", got, tc.precision, tc.scale)
			}
		})
	}
}

// TestMirrorDecimalUsesTheColumnScaleNotTheValues: the driver hands back a
// printed string, so "1.5" in a DECIMAL(10,2) must become 150 and not 15.
// Reading the scale off the value is how it gets lost — and the two are
// indistinguishable by value alone.
func TestMirrorDecimalUsesTheColumnScaleNotTheValues(t *testing.T) {
	ct := colType{kind: kindDecimal, precision: 10, scale: 2}
	v := coerce("1.5", ct)
	if v != int64(150) {
		t.Fatalf("coerce(\"1.5\", decimal(10,2)) = %v (%T); want the unscaled 150", v, v)
	}
	// The same digits at a different declared scale are a different number.
	if v := coerce("1.5", colType{kind: kindDecimal, precision: 10, scale: 3}); v != int64(1500) {
		t.Errorf("coerce at scale 3 = %v; want 1500", v)
	}
}

// TestMirroredDecimalSurvivesARealSQLServer drives the whole mirror path
// against the engine: a SQL DECIMAL column is snapshotted to Delta and read
// back, with the scale intact. The unit tests above construct the colType by
// hand; this one gets it from the driver's DecimalSize, which is the part that
// can silently return "not ok" and quietly fall back.
func TestMirroredDecimalSurvivesARealSQLServer(t *testing.T) {
	db := testsupport.OpenMSSQL(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx,
		"CREATE TABLE [prices] (sku NVARCHAR(10), amt DECIMAL(12,3), fee MONEY)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO prices VALUES ('a', 1234.567, 9.99), ('b', -0.500, 0.01)"); err != nil {
		t.Fatal(err)
	}

	tbl, kinds, err := readSQLTable(ctx, db, "prices")
	if err != nil {
		t.Fatal(err)
	}
	col := func(n string) int {
		for i, c := range tbl.Columns {
			if c == n {
				return i
			}
		}
		t.Fatalf("no column %q in %v", n, tbl.Columns)
		return -1
	}
	// The declared type, taken from the driver rather than from the values.
	if got := deltaTypeName(kinds[col("amt")]); got != "decimal(12,3)" {
		t.Errorf("amt mirrors as %q, want decimal(12,3)", got)
	}
	// MONEY reports no DecimalSize, so it has to be named explicitly or it
	// falls through to a string.
	if got := deltaTypeName(kinds[col("fee")]); got != "decimal(19,4)" {
		t.Errorf("fee (MONEY) mirrors as %q, want decimal(19,4)", got)
	}

	pq, err := encodeParquet(tbl, kinds)
	if err != nil {
		t.Fatal(err)
	}
	back, err := readParquet(pq)
	if err != nil {
		t.Fatal(err)
	}
	// The read-back table's own column order, not the source table's:
	// encodeParquet builds a parquet.Group, which is a map, so the written
	// schema is ordered by NAME. Indexing the result with the source positions
	// reads a different column and the mistake surfaces as a type panic.
	amt, sku := columnIndex(back, "amt"), columnIndex(back, "sku")
	got := map[string]string{}
	for _, r := range back.Rows {
		d, ok := r[amt].(Decimal)
		if !ok {
			t.Fatalf("amt round-tripped as %T (%v); want Decimal", r[amt], r[amt])
		}
		key, ok := r[sku].(string)
		if !ok {
			t.Fatalf("sku round-tripped as %T; want string", r[sku])
		}
		got[key] = d.String()
	}
	if got["a"] != "1234.567" || got["b"] != "-0.500" {
		t.Errorf("decimal round-trip = %v, want a=1234.567 b=-0.500", got)
	}
}

// nestedRow mixes nested columns with flat ones, and puts a flat column AFTER
// the nested ones — which is where the old reader did its worst damage.
type nestedLine struct {
	LineNo    int32  `parquet:"line_no"`
	ProductID string `parquet:"product_id"`
	Quantity  int32  `parquet:"quantity"`
}
type nestedAddr struct {
	Country string `parquet:"country"`
	No      string `parquet:"no"`
}
type nestedRow struct {
	Flat  string            `parquet:"flat"`
	Lines []nestedLine      `parquet:"lines"`
	Addr  nestedAddr        `parquet:"addr"`
	Tags  map[string]string `parquet:"tags"`
	After int64             `parquet:"after_flat"`
}

// TestNestedColumnsAreOmittedNotFabricated.
//
// Reported from contoso-data-platform after the date fix: a lakehouse holding a
// nested column reflected garbage, silently. Measured before this fix, on a
// table of flat/lines/addr/tags/after_flat:
//
//	flat       -> "control"   (correct, it is leaf 0)
//	lines      -> 8           (lines.line_no of the SECOND element)
//	addr       -> "P-200"     (lines.product_id)
//	tags       -> 4           (lines.quantity)
//	after_flat -> "SG"        (addr.country) — and its real 999 was DROPPED
//
// The reader assigned parquet LEAF values by position into a slice sized by
// TOP-LEVEL field count, so one nested column shifted every column after it.
// Nothing raised; a SELECT returned plausible values. Unlike the date bug there
// was no loud half at all.
//
// Fabric does not represent these types: "Types that aren't listed in the table
// aren't represented as the table columns in the SQL analytics endpoint", and
// "Some columns that exist in the Spark Delta tables might not be available".
// So the column is dropped and everything else stays correct.
func TestNestedColumnsAreOmittedNotFabricated(t *testing.T) {
	var buf bytes.Buffer
	w := parquet.NewGenericWriter[nestedRow](&buf)
	if _, err := w.Write([]nestedRow{{
		Flat: "control",
		Lines: []nestedLine{
			{LineNo: 7, ProductID: "P-100", Quantity: 3},
			{LineNo: 8, ProductID: "P-200", Quantity: 4},
		},
		Addr:  nestedAddr{Country: "SG", No: "1"},
		Tags:  map[string]string{"k": "v"},
		After: 999,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	tbl, err := readParquet(buf.Bytes())
	if err != nil {
		t.Fatalf("a nested schema must read, not error: %v", err)
	}

	// The nested columns are gone, and NAMED so a reader can find out why.
	if got := strings.Join(tbl.Columns, ","); got != "flat,after_flat" {
		t.Fatalf("columns = %q; want only the flat ones", got)
	}
	if got := strings.Join(tbl.Skipped, ","); got != "lines,addr,tags" {
		t.Errorf("skipped = %q; want lines,addr,tags recorded by name", got)
	}

	// The decisive assertion: the flat column AFTER the nested ones carries its
	// OWN value. This is what the old reader destroyed — it read "SG".
	after := columnIndex(tbl, "after_flat")
	if got := tbl.Rows[0][after]; got != int64(999) {
		t.Errorf("after_flat = %v (%T); want int64(999) — a nested column must "+
			"not displace the columns following it", got, got)
	}
	if got := sqlType(tbl, after); got != "BIGINT" {
		t.Errorf("after_flat reflects as %s; want BIGINT", got)
	}
	if got := tbl.Rows[0][columnIndex(tbl, "flat")]; got != "control" {
		t.Errorf("flat = %v; want \"control\"", got)
	}
}

// TestDeltaStringReflectsAsVarchar: Fabric maps Delta STRING to varchar(8000),
// and says of nvarchar "there's no similar unicode data type in Parquet". We
// emitted NVARCHAR(4000), which was a plain divergence — and one this repo
// briefly documented as correct.
func TestDeltaStringReflectsAsVarchar(t *testing.T) {
	tbl, err := readParquet(writeEveryType(t, everyType{S: "x"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := sqlType(tbl, columnIndex(tbl, "s")); got != "VARCHAR(8000)" {
		t.Errorf("Delta string reflected as %s; want VARCHAR(8000)", got)
	}
	// An all-null column falls back to the same type, not to nvarchar.
	empty := &Table{Columns: []string{"c"}, Rows: [][]any{{nil}}}
	if got := sqlType(empty, 0); got != "VARCHAR(8000)" {
		t.Errorf("the all-null default is %s; want VARCHAR(8000)", got)
	}
}
