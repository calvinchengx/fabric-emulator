package warehouse

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	mssql "github.com/microsoft/go-mssqldb"

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

// reflectTable drops and recreates one table, then loads its rows.
//
// SQL Server — the production target — is loaded with the TDS bulk-copy
// protocol, which is how a warehouse is actually loaded: rows stream in binary
// and the server parses no SQL for them at all. Building INSERT ... VALUES text
// instead makes load cost scale with CHARACTER COUNT rather than row count, and
// SQL Server's parse cost grows faster than linearly with statement size, so a
// wide table degrades sharply: reflecting 20,000 rows x 100 columns that way
// measured 123s of statement execution against 0.13s of reading the Delta and
// 0.09s of building the literals. Bulk copy does the same load in ~1s.
//
// The literal path below is kept for the SQLite backend the unit tests inject,
// which has no bulk protocol. That does mean the two backends load by different
// routes; the SQL Server route is the one the e2e suites (e2e/medallion,
// e2e/dbt-fabric, e2e/warehouse-tds) exercise against the real sidecar.
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
	if len(tbl.Rows) == 0 {
		return nil
	}

	// Ask the DRIVER, not the literal dialect: Reflect is called with nprefix
	// "N" by production AND by the unit tests, which hand it a SQLite handle, so
	// the prefix says nothing about who is on the other end of the connection.
	if isSQLServer(db) {
		return bulkInsert(ctx, db, q, tbl)
	}

	// No bulk protocol (SQLite): batch by statement SIZE rather than a fixed row
	// count, so a wide table's statements stay the same size as a narrow one's.
	const (
		maxStmtBytes     = 64 << 10
		maxRowsPerInsert = 500
	)
	prefix := "INSERT INTO " + q + " VALUES "
	var sb strings.Builder
	sb.WriteString(prefix)
	inBatch := 0

	flush := func() error {
		if inBatch == 0 {
			return nil
		}
		if _, err := db.ExecContext(ctx, sb.String()); err != nil {
			return err
		}
		sb.Reset()
		sb.WriteString(prefix)
		inBatch = 0
		return nil
	}

	for _, row := range tbl.Rows {
		cells := make([]string, len(row))
		for i, v := range row {
			cells[i] = literal(v, nprefix)
		}
		frag := "(" + strings.Join(cells, ",") + ")"
		if inBatch > 0 && (sb.Len()+1+len(frag) > maxStmtBytes || inBatch >= maxRowsPerInsert) {
			if err := flush(); err != nil {
				return err
			}
		}
		if inBatch > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(frag)
		inBatch++
	}
	return flush()
}

// isSQLServer reports whether db really is a go-mssqldb connection, and so can
// speak the bulk-copy protocol. Everything else (the tests' SQLite) takes the
// literal-INSERT path.
func isSQLServer(db *sql.DB) bool {
	_, ok := db.Driver().(*mssql.Driver)
	return ok
}

// bulkInsert streams a table's rows into SQL Server over the TDS bulk-copy
// protocol. table must already be quoted: go-mssqldb interpolates the name
// straight into "INSERT BULK %s" and the metadata probe it issues first.
func bulkInsert(ctx context.Context, db *sql.DB, table string, tbl *Table) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // no-op once Commit succeeds

	// KeepNulls: a NULL in the Delta must land as NULL, not as the destination
	// column's default.
	stmt, err := tx.PrepareContext(ctx,
		mssql.CopyIn(table, mssql.BulkOptions{KeepNulls: true}, tbl.Columns...))
	if err != nil {
		return err
	}

	vals := make([]any, len(tbl.Columns))
	for _, row := range tbl.Rows {
		for i := range vals {
			if i < len(row) {
				vals[i] = bulkValue(row[i])
			} else {
				vals[i] = nil // short row: the Delta schema wins
			}
		}
		if _, err := stmt.ExecContext(ctx, vals...); err != nil {
			_ = stmt.Close()
			return err
		}
	}
	// A final parameterless Exec flushes the last partial batch.
	if _, err := stmt.ExecContext(ctx); err != nil {
		_ = stmt.Close()
		return err
	}
	if err := stmt.Close(); err != nil {
		return err
	}
	return tx.Commit()
}

// bulkValue maps a Delta value onto what the bulk-copy encoder accepts. Only
// Decimal needs help: it goes as its exact decimal string, which the encoder
// re-parses at the destination column's scale. Passing a float64 instead would
// reintroduce exactly the scale loss sqlType goes out of its way to avoid.
func bulkValue(v any) any {
	if d, ok := v.(Decimal); ok {
		return d.String()
	}
	return v
}

// sqlType infers a column's SQL type from its first non-null value (default
// NVARCHAR). The type names are valid in both SQL Server and SQLite.
func sqlType(tbl *Table, col int) string {
	for _, row := range tbl.Rows {
		switch v := row[col].(type) {
		case bool:
			return "BIT"
		case Decimal:
			// Preserve the declared precision/scale: reflecting a decimal as
			// BIGINT drops the scale and every aggregate over it is then wrong
			// by 10^scale. SQL Server caps precision at 38.
			p, s := v.Precision, v.Scale
			if p < 1 || p > 38 {
				p = 38
			}
			if s < 0 || s > p {
				s = 0
			}
			return fmt.Sprintf("DECIMAL(%d,%d)", p, s)
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
	case Decimal:
		return x.String() // scale applied; already a valid SQL decimal literal
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
