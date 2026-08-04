package warehouse

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/parquet-go/parquet-go"
	"math/big"
	"strconv"
	"strings"
)

// Mirror snapshots every base table in a Fabric SQL Database's SQL Server
// database to OneLake as a Delta table under Tables/<name>/ — the "mirroring"
// that makes an operational (OLTP) SQL Database queryable as Delta by
// Spark / DuckDB / delta-rs, exactly as real Fabric mirrors it. It is the
// reverse of Reflect (Delta → engine): here engine → Delta. Each call writes a
// fresh single-commit snapshot per table (a full re-sync, not incremental).
func Mirror(ctx context.Context, db *sql.DB, st *store.Store, itemID string) error {
	it, err := st.GetItemByID(itemID)
	if err != nil {
		return fmt.Errorf("mirror: item %q not found: %w", itemID, err)
	}
	tables, err := listBaseTables(ctx, db)
	if err != nil {
		return fmt.Errorf("mirror: listing tables: %w", err)
	}
	for _, name := range tables {
		tbl, kinds, err := readSQLTable(ctx, db, name)
		if err != nil {
			return fmt.Errorf("mirror: reading %q: %w", name, err)
		}
		if err := writeDeltaSnapshot(st, it.WorkspaceID, itemID, name, tbl, kinds); err != nil {
			return fmt.Errorf("mirror: writing Delta for %q: %w", name, err)
		}
	}
	return nil
}

// listBaseTables returns the user base tables in the connected database.
func listBaseTables(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT TABLE_NAME FROM INFORMATION_SCHEMA.TABLES WHERE TABLE_TYPE='BASE TABLE' ORDER BY TABLE_NAME")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// colKind is the Delta/Parquet type a mirrored column is written as.
type colKind int

const (
	kindString colKind = iota
	kindLong
	kindDouble
	kindBool
	kindDate
	kindTimestamp
	kindBinary
	kindInt
	kindDecimal
)

// colType is a column's Delta type. A decimal cannot be described by a kind
// alone — its precision and scale are part of the type, and dropping them is
// the same loss sqlType guards against in the other direction: every aggregate
// over the column then comes back wrong by 10^scale.
type colType struct {
	kind      colKind
	precision int // kindDecimal only
	scale     int // kindDecimal only
}

// readSQLTable reads all rows of a table into a Table plus the Parquet kind
// inferred per column (from the first non-null value across the column; a
// column with only NULLs is mirrored as string).
func readSQLTable(ctx context.Context, db *sql.DB, name string) (*Table, []colType, error) {
	// name comes from INFORMATION_SCHEMA (server metadata), not user input; it is
	// bracket-quoted for identifiers with spaces.
	rows, err := db.QueryContext(ctx, "SELECT * FROM ["+name+"]")
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, nil, err
	}
	tbl := &Table{Columns: cols}
	// Resolved from the driver's column metadata BEFORE any row is read, since
	// the scan loop's own []byte handling depends on knowing which columns are
	// binary.
	kinds := make([]colType, len(cols))
	ctypes, cterr := rows.ColumnTypes()
	for i := range kinds {
		if cterr == nil && i < len(ctypes) {
			kinds[i] = typeFromSQL(ctypes[i])
		}
	}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, nil, err
		}
		for i, v := range vals {
			if bs, ok := v.([]byte); ok && kinds[i].kind != kindBinary {
				// Bytes are text only when the COLUMN is text. A varbinary
				// stringified here would be mirrored as a Delta string, which
				// is how the reverse direction lost binary.
				vals[i] = string(bs)
			}
		}
		tbl.Rows = append(tbl.Rows, vals)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return tbl, kinds, nil
}

// kindFromSQL maps a driver-reported column type to the Delta kind.
//
// The server's own metadata, not the first non-null value: a DATE and a
// DATETIME2 both scan as time.Time and an INT and a BIGINT both as int64, so
// value inference cannot tell either pair apart and silently collapses them.
// That is the same mistake, in the opposite direction, as reading a Parquet
// DATE by its physical kind.
// typeFromSQL resolves a column's full Delta type from the driver's metadata.
//
// DecimalSize() is the authority for precision and scale — the value arrives as
// the decimal STRING ("1234.567"), which carries the scale it happens to have
// printed rather than the one the column is declared with, so 1.50 in a
// DECIMAL(10,2) and 1.5 in a DECIMAL(10,1) are indistinguishable by value.
func typeFromSQL(ct *sql.ColumnType) colType {
	switch strings.ToUpper(ct.DatabaseTypeName()) {
	case "DECIMAL", "NUMERIC":
		p, sc, ok := ct.DecimalSize()
		if !ok {
			// Nothing declared: the widest exact type, rather than silently
			// rounding to an int.
			p, sc = 38, 0
		}
		return colType{kind: kindDecimal, precision: int(p), scale: int(sc)}
	case "MONEY":
		// SQL Server's MONEY is a fixed decimal(19,4) and reports no
		// DecimalSize, so it has to be named rather than inferred.
		return colType{kind: kindDecimal, precision: 19, scale: 4}
	case "SMALLMONEY":
		return colType{kind: kindDecimal, precision: 10, scale: 4}
	}
	return colType{kind: kindFromSQL(ct.DatabaseTypeName())}
}

func kindFromSQL(name string) colKind {
	switch strings.ToUpper(name) {
	case "DATE":
		return kindDate
	case "DATETIME", "DATETIME2", "SMALLDATETIME", "DATETIMEOFFSET":
		return kindTimestamp
	case "BINARY", "VARBINARY", "IMAGE":
		return kindBinary
	case "INT", "SMALLINT", "TINYINT":
		return kindInt
	case "BIGINT":
		return kindLong
	case "FLOAT", "REAL":
		return kindDouble
	case "BIT":
		return kindBool
	}
	return kindString
}

// kindOf is the fallback for a table built without driver metadata (the
// in-memory tests). It cannot distinguish DATE from DATETIME2 — nothing in the
// Go value does — so it answers timestamp and leaves the precise call to
// kindFromSQL, which has the server's own answer.
// colTypeOf is the value-based fallback, for a table built without driver
// metadata. A Decimal value carries its own precision and scale, so this one
// case loses nothing.
func colTypeOf(v any) colType {
	if d, ok := v.(Decimal); ok {
		return colType{kind: kindDecimal, precision: d.Precision, scale: d.Scale}
	}
	return colType{kind: kindOf(v)}
}

func kindOf(v any) colKind {
	switch v.(type) {
	case int32, int:
		return kindInt
	case int64:
		return kindLong
	case float64, float32:
		return kindDouble
	case bool:
		return kindBool
	case Decimal:
		return kindDecimal
	case time.Time, Timestamp:
		return kindTimestamp
	case Date:
		return kindDate
	case []byte:
		return kindBinary
	default:
		return kindString
	}
}

// writeDeltaSnapshot writes a table as a single-commit Delta table under
// Tables/<name>/: one Parquet data file plus a _delta_log commit (protocol +
// metaData + add) that delta-rs / Spark / DuckDB — and this package's own
// reader — accept.
func writeDeltaSnapshot(st *store.Store, wsID, itemID, name string, tbl *Table, kinds []colType) error {
	pq, err := encodeParquet(tbl, kinds)
	if err != nil {
		return err
	}
	root := path.Join("Tables", name)
	now := time.Now().UnixMilli()
	if err := st.CreateOneLakePath(&store.OneLakePath{
		WorkspaceID: wsID, ItemID: itemID, RelPath: path.Join(root, "part-0.parquet"), Content: pq,
	}, false); err != nil {
		return err
	}
	return st.CreateOneLakePath(&store.OneLakePath{
		WorkspaceID: wsID, ItemID: itemID,
		RelPath: path.Join(root, "_delta_log", "00000000000000000000.json"),
		Content: deltaCommit(tbl.Columns, kinds, len(pq), now),
	}, false)
}

// encodeParquet writes the table's rows as a Parquet file whose (nullable)
// columns carry their inferred type.
func encodeParquet(tbl *Table, kinds []colType) ([]byte, error) {
	group := parquet.Group{}
	kindByName := make(map[string]colType, len(tbl.Columns))
	for i, c := range tbl.Columns {
		group[c] = parquet.Optional(leafFor(kinds[i]))
		kindByName[c] = kinds[i]
	}
	schema := parquet.NewSchema("mirror", group)

	// Group is a map, so the schema orders columns by name; map each input column
	// to its leaf column index.
	colIndex := make(map[string]int, len(tbl.Columns))
	for i, f := range schema.Fields() {
		colIndex[f.Name()] = i
	}

	// Build rows with explicit definition levels so a present zero value (a
	// `false` bool, a `0`) is written non-null, not confused with NULL — the
	// ambiguity a map/struct writer cannot express.
	prows := make([]parquet.Row, len(tbl.Rows))
	for r, row := range tbl.Rows {
		pr := make(parquet.Row, len(tbl.Columns))
		for i, c := range tbl.Columns {
			ci := colIndex[c]
			if v := coerce(row[i], kindByName[c]); v == nil {
				pr[ci] = parquet.NullValue().Level(0, 0, ci) // definition level 0 = NULL
			} else {
				pr[ci] = parquet.ValueOf(v).Level(0, 1, ci) // definition level 1 = present
			}
		}
		prows[r] = pr
	}

	var buf bytes.Buffer
	w := parquet.NewWriter(&buf, schema)
	if len(prows) > 0 {
		if _, err := w.WriteRows(prows); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func leafFor(t colType) parquet.Node {
	if t.kind == kindDecimal {
		// The physical encoding delta-rs picks by precision, so a reader that
		// resolves by annotation (including this package's own) finds what it
		// expects at every width.
		switch {
		case t.precision <= 9:
			return parquet.Decimal(t.scale, t.precision, parquet.Int32Type)
		case t.precision <= 18:
			return parquet.Decimal(t.scale, t.precision, parquet.Int64Type)
		default:
			return parquet.Decimal(t.scale, t.precision, parquet.ByteArrayType)
		}
	}
	switch t.kind {
	case kindLong:
		return parquet.Leaf(parquet.Int64Type)
	case kindInt:
		return parquet.Int(32)
	case kindDouble:
		return parquet.Leaf(parquet.DoubleType)
	case kindBool:
		return parquet.Leaf(parquet.BooleanType)
	case kindDate:
		// The ANNOTATED node, not a bare INT32. Writing the day count without
		// the annotation is precisely the state the reader could not recover
		// from, and it would round-trip back as a long.
		return parquet.Date()
	case kindTimestamp:
		return parquet.Timestamp(parquet.Microsecond)
	case kindBinary:
		return parquet.Leaf(parquet.ByteArrayType)
	default:
		return parquet.String()
	}
}

// coerce normalizes a scanned value to the Go type the column's Parquet leaf
// expects (so a mixed driver representation still writes cleanly).
func coerce(v any, t colType) any {
	if v == nil {
		return nil
	}
	if t.kind == kindDecimal {
		return decimalUnscaled(v, t)
	}
	switch t.kind {
	case kindLong:
		switch n := v.(type) {
		case int64:
			return n
		case int32:
			return int64(n)
		case int:
			return int64(n)
		}
	case kindDouble:
		switch f := v.(type) {
		case float64:
			return f
		case float32:
			return float64(f)
		}
	case kindInt:
		switch n := v.(type) {
		case int32:
			return n
		case int64:
			return int32(n)
		case int:
			return int32(n)
		}
	case kindBool:
		if b, ok := v.(bool); ok {
			return b
		}
	case kindDate:
		if t, ok := dayTime(v); ok {
			return int32(t.Sub(epochDay) / (24 * time.Hour))
		}
	case kindTimestamp:
		if t, ok := dayTime(v); ok {
			return t.UnixMicro()
		}
	case kindBinary:
		switch b := v.(type) {
		case []byte:
			return b
		case string:
			return []byte(b)
		}
	default:
		return fmt.Sprint(v)
	}
	return v
}

// decimalUnscaled renders a decimal value as the unscaled integer its Parquet
// leaf expects, at the COLUMN's scale rather than the value's.
//
// The driver hands back a string, so "1.5" in a DECIMAL(10,2) has to become
// 150, not 15 — reading the scale off the printed value is exactly how the
// scale gets lost.
func decimalUnscaled(v any, t colType) any {
	var d Decimal
	switch x := v.(type) {
	case Decimal:
		d = x
	case string:
		parsed, ok := parseDecimal(x, t.scale)
		if !ok {
			return nil
		}
		d = parsed
	case []byte:
		parsed, ok := parseDecimal(string(x), t.scale)
		if !ok {
			return nil
		}
		d = parsed
	case float64:
		parsed, ok := parseDecimal(strconv.FormatFloat(x, 'f', t.scale, 64), t.scale)
		if !ok {
			return nil
		}
		d = parsed
	case int64:
		d = Decimal{Unscaled: new(big.Int).Mul(big.NewInt(x), pow10(t.scale)), Scale: t.scale}
	case int32:
		d = Decimal{Unscaled: new(big.Int).Mul(big.NewInt(int64(x)), pow10(t.scale)), Scale: t.scale}
	default:
		return nil
	}
	if d.Unscaled == nil {
		return nil
	}
	u := rescale(d, t.scale)
	switch {
	case t.precision <= 9:
		return int32(u.Int64())
	case t.precision <= 18:
		return u.Int64()
	default:
		return twosComplement(u)
	}
}

// rescale moves an unscaled integer from its own scale to the column's.
func rescale(d Decimal, scale int) *big.Int {
	u := new(big.Int).Set(d.Unscaled)
	switch {
	case d.Scale < scale:
		return u.Mul(u, pow10(scale-d.Scale))
	case d.Scale > scale:
		return u.Quo(u, pow10(d.Scale-scale))
	}
	return u
}

func pow10(n int) *big.Int {
	if n <= 0 {
		return big.NewInt(1)
	}
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

// parseDecimal reads "-1234.567" into an unscaled integer at `scale`.
func parseDecimal(s string, scale int) (Decimal, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Decimal{}, false
	}
	neg := false
	switch s[0] {
	case '-':
		neg, s = true, s[1:]
	case '+':
		s = s[1:]
	}
	intPart, fracPart := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, fracPart = s[:i], s[i+1:]
	}
	// Pad or truncate the fraction to the column's scale.
	for len(fracPart) < scale {
		fracPart += "0"
	}
	if len(fracPart) > scale {
		fracPart = fracPart[:scale]
	}
	digits := intPart + fracPart
	if digits == "" {
		return Decimal{}, false
	}
	u, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		return Decimal{}, false
	}
	if neg {
		u.Neg(u)
	}
	return Decimal{Unscaled: u, Scale: scale}, true
}

// twosComplement is the big-endian encoding a byte-array DECIMAL uses, and the
// one decimalValue reads back.
func twosComplement(u *big.Int) []byte {
	if u.Sign() >= 0 {
		b := u.Bytes()
		if len(b) == 0 {
			return []byte{0}
		}
		if b[0]&0x80 != 0 {
			return append([]byte{0}, b...)
		}
		return b
	}
	n := new(big.Int).Set(u)
	size := (n.BitLen()/8 + 2)
	mod := new(big.Int).Lsh(big.NewInt(1), uint(size*8))
	b := new(big.Int).Add(n, mod).Bytes()
	for len(b) < size {
		b = append([]byte{0xff}, b...)
	}
	return b
}

// dayTime extracts a time from the shapes a date/timestamp column arrives in.
func dayTime(v any) (time.Time, bool) {
	switch t := v.(type) {
	case time.Time:
		return t.UTC(), true
	case Date:
		return t.T.UTC(), true
	case Timestamp:
		return t.T.UTC(), true
	}
	return time.Time{}, false
}

// deltaTypeName maps a column kind to the Delta schema type string.
func deltaTypeName(t colType) string {
	if t.kind == kindDecimal {
		return fmt.Sprintf("decimal(%d,%d)", t.precision, t.scale)
	}
	switch t.kind {
	case kindLong:
		return "long"
	case kindInt:
		return "integer"
	case kindDouble:
		return "double"
	case kindBool:
		return "boolean"
	case kindDate:
		return "date"
	case kindTimestamp:
		return "timestamp"
	case kindBinary:
		return "binary"
	default:
		return "string"
	}
}

// deltaCommit builds the _delta_log/0.json NDJSON commit (protocol, metaData,
// add) for a fresh single-file Delta table.
func deltaCommit(cols []string, kinds []colType, size int, nowMillis int64) []byte {
	fields := make([]map[string]any, len(cols))
	for i, c := range cols {
		fields[i] = map[string]any{"name": c, "type": deltaTypeName(kinds[i]), "nullable": true, "metadata": map[string]any{}}
	}
	schemaJSON, _ := json.Marshal(map[string]any{"type": "struct", "fields": fields})

	protocol := map[string]any{"protocol": map[string]any{"minReaderVersion": 1, "minWriterVersion": 2}}
	metaData := map[string]any{"metaData": map[string]any{
		"id":               store.NewID(),
		"format":           map[string]any{"provider": "parquet", "options": map[string]any{}},
		"schemaString":     string(schemaJSON),
		"partitionColumns": []string{},
		"configuration":    map[string]any{},
		"createdTime":      nowMillis,
	}}
	add := map[string]any{"add": map[string]any{
		"path":             "part-0.parquet",
		"partitionValues":  map[string]any{},
		"size":             size,
		"modificationTime": nowMillis,
		"dataChange":       true,
	}}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	_ = enc.Encode(protocol)
	_ = enc.Encode(metaData)
	_ = enc.Encode(add)
	return buf.Bytes()
}
