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
)

// readSQLTable reads all rows of a table into a Table plus the Parquet kind
// inferred per column (from the first non-null value across the column; a
// column with only NULLs is mirrored as string).
func readSQLTable(ctx context.Context, db *sql.DB, name string) (*Table, []colKind, error) {
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
			if bs, ok := v.([]byte); ok {
				vals[i] = string(bs)
			}
		}
		tbl.Rows = append(tbl.Rows, vals)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	kinds := make([]colKind, len(cols))
	for i := range cols {
		kinds[i] = kindString
		for _, row := range tbl.Rows {
			if row[i] != nil {
				kinds[i] = kindOf(row[i])
				break
			}
		}
	}
	return tbl, kinds, nil
}

func kindOf(v any) colKind {
	switch v.(type) {
	case int64, int32, int:
		return kindLong
	case float64, float32:
		return kindDouble
	case bool:
		return kindBool
	default:
		return kindString
	}
}

// writeDeltaSnapshot writes a table as a single-commit Delta table under
// Tables/<name>/: one Parquet data file plus a _delta_log commit (protocol +
// metaData + add) that delta-rs / Spark / DuckDB — and this package's own
// reader — accept.
func writeDeltaSnapshot(st *store.Store, wsID, itemID, name string, tbl *Table, kinds []colKind) error {
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
func encodeParquet(tbl *Table, kinds []colKind) ([]byte, error) {
	group := parquet.Group{}
	kindByName := make(map[string]colKind, len(tbl.Columns))
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

func leafFor(k colKind) parquet.Node {
	switch k {
	case kindLong:
		return parquet.Leaf(parquet.Int64Type)
	case kindDouble:
		return parquet.Leaf(parquet.DoubleType)
	case kindBool:
		return parquet.Leaf(parquet.BooleanType)
	default:
		return parquet.String()
	}
}

// coerce normalizes a scanned value to the Go type the column's Parquet leaf
// expects (so a mixed driver representation still writes cleanly).
func coerce(v any, k colKind) any {
	if v == nil {
		return nil
	}
	switch k {
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
	case kindBool:
		if b, ok := v.(bool); ok {
			return b
		}
	default:
		return fmt.Sprint(v)
	}
	return v
}

// deltaTypeName maps a column kind to the Delta schema type string.
func deltaTypeName(k colKind) string {
	switch k {
	case kindLong:
		return "long"
	case kindDouble:
		return "double"
	case kindBool:
		return "boolean"
	default:
		return "string"
	}
}

// deltaCommit builds the _delta_log/0.json NDJSON commit (protocol, metaData,
// add) for a fresh single-file Delta table.
func deltaCommit(cols []string, kinds []colKind, size int, nowMillis int64) []byte {
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
