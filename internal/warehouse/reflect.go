package warehouse

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// Reflect (re)materialises a lakehouse's Delta tables into the SQL engine so the
// warehouse endpoint can query them: for each Tables/<name>, read the Delta
// table and DROP/CREATE/INSERT it into db. Idempotent — safe to call on every
// connect. Returns the names reflected.
func Reflect(ctx context.Context, db *sql.DB, st *store.Store, itemID string) ([]string, error) {
	// Production target is SQL Server: Unicode string literals take the N prefix.
	return reflect(ctx, db, st, itemID, "N")
}

// reflect is Reflect with the string-literal Unicode prefix parameterised, so
// tests can target SQLite (no N prefix, no MAX) over the same code path.
func reflect(ctx context.Context, db *sql.DB, st *store.Store, itemID, nprefix string) ([]string, error) {
	dirs, err := st.ListOneLakePaths(itemID, "Tables", false)
	if err != nil {
		return nil, err
	}
	var done []string
	for _, d := range dirs {
		if !d.IsDir {
			continue
		}
		name := strings.TrimPrefix(d.RelPath, "Tables/")
		tbl, err := ReadDeltaTable(st, itemID, name)
		if err != nil {
			// A folder under Tables/ that isn't a Delta table is skipped, not fatal.
			continue
		}
		if err := reflectTable(ctx, db, name, tbl, nprefix); err != nil {
			return done, fmt.Errorf("reflect %q: %w", name, err)
		}
		done = append(done, name)
	}
	return done, nil
}

// reflectTable drops and recreates one table, then bulk-inserts its rows using
// literal values (no bound parameters — the placeholder dialect differs between
// SQL Server @p and SQLite ?, and reflected data is our own).
func reflectTable(ctx context.Context, db *sql.DB, name string, tbl *Table, nprefix string) error {
	q := quoteIdent(name)
	if _, err := db.ExecContext(ctx, "DROP TABLE IF EXISTS "+q); err != nil {
		return err
	}
	defs := make([]string, len(tbl.Columns))
	for i, c := range tbl.Columns {
		defs[i] = quoteIdent(c) + " " + sqlType(tbl, i)
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE "+q+" ("+strings.Join(defs, ", ")+")"); err != nil {
		return err
	}

	const batch = 500
	for start := 0; start < len(tbl.Rows); start += batch {
		end := start + batch
		if end > len(tbl.Rows) {
			end = len(tbl.Rows)
		}
		values := make([]string, 0, end-start)
		for _, row := range tbl.Rows[start:end] {
			cells := make([]string, len(row))
			for i, v := range row {
				cells[i] = literal(v, nprefix)
			}
			values = append(values, "("+strings.Join(cells, ",")+")")
		}
		if _, err := db.ExecContext(ctx, "INSERT INTO "+q+" VALUES "+strings.Join(values, ",")); err != nil {
			return err
		}
	}
	return nil
}

// sqlType infers a column's SQL type from its first non-null value (default
// NVARCHAR). The type names are valid in both SQL Server and SQLite.
func sqlType(tbl *Table, col int) string {
	for _, row := range tbl.Rows {
		switch row[col].(type) {
		case bool:
			return "BIT"
		case int64:
			return "BIGINT"
		case float64:
			return "FLOAT"
		case []byte:
			return "VARBINARY(4000)"
		case string:
			return "NVARCHAR(4000)"
		}
	}
	return "NVARCHAR(4000)"
}

// literal renders a Go value as a SQL literal. nprefix is "N" for SQL Server
// (Unicode string literals) or "" for SQLite.
func literal(v any, nprefix string) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case bool:
		if x {
			return "1"
		}
		return "0"
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case []byte:
		return "0x" + hex.EncodeToString(x)
	case string:
		return nprefix + "'" + strings.ReplaceAll(x, "'", "''") + "'"
	default:
		return nprefix + "'" + strings.ReplaceAll(fmt.Sprint(x), "'", "''") + "'"
	}
}

// quoteIdent wraps an identifier in brackets (T-SQL; SQLite accepts them too).
func quoteIdent(s string) string {
	return "[" + strings.ReplaceAll(s, "]", "]]") + "]"
}
