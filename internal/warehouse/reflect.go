package warehouse

import (
	"context"
	"database/sql"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"

	mssql "github.com/microsoft/go-mssqldb"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// Reflect (re)materialises a lakehouse's Delta tables into the SQL engine so the
// warehouse endpoint can query them: for each Tables/<name>, read the Delta
// table and DROP/CREATE/INSERT it into db. Idempotent — safe to call on every
// connect. Returns the names reflected.
//
// This is the unconditional form: every table, every time. It is what the tests
// use and what a caller wants when it has nowhere to keep state. A server
// handling repeated logins should use a Reflector instead — see its doc for why
// reflecting everything on every connect is not merely wasteful but unable to
// converge.
func Reflect(ctx context.Context, db *sql.DB, st *store.Store, itemID string) ([]string, error) {
	return (&Reflector{}).Reflect(ctx, db, st, itemID)
}

// A Reflector is a Reflect that remembers what it already did.
//
// Reflection runs during TDS login, synchronously, before the connection is
// usable. On a lakehouse holding real data that takes minutes, so the client's
// login timeout expires first and it retries. Without memory those retries
// cannot converge: each one DROPs and reloads every table from zero and is
// killed at the same deadline, so the only way to finish is for the whole
// reflection to fit inside one login timeout — which it does only once caches
// are warm enough, by luck. The medallion e2e was passing this way, on attempt
// 10 of 40 with a 12-minute budget; a little more load on the runner and it
// exhausted all 40 and went red.
//
// Remembering fixes the shape of that, not just the speed. Each table's Delta
// log fingerprint is recorded only AFTER that table reflects successfully, so a
// login cancelled halfway leaves the finished tables recorded and the rest not.
// The next attempt resumes instead of restarting, and retries accumulate
// progress. Once everything is current a login costs one directory listing per
// table and no SQL at all.
//
// The zero value is a valid, empty Reflector.
type Reflector struct {
	mu    sync.Mutex              // guards items
	items map[string]*reflectItem // itemID -> its lock and fingerprints
}

type reflectItem struct {
	mu   sync.Mutex        // serialises reflections of this one item
	seen map[string]string // table name -> the fingerprint last reflected
}

func (r *Reflector) item(itemID string) *reflectItem {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.items == nil {
		r.items = map[string]*reflectItem{}
	}
	it, ok := r.items[itemID]
	if !ok {
		it = &reflectItem{seen: map[string]string{}}
		r.items[itemID] = it
	}
	return it
}

// Reflect brings itemID's tables up to date, skipping those whose Delta log has
// not advanced. Returns the tables it actually loaded — not the tables present,
// so an unchanged lakehouse reports nothing rather than reporting work it did
// not do.
func (r *Reflector) Reflect(ctx context.Context, db *sql.DB, st *store.Store, itemID string) ([]string, error) {
	// Per item, not global: a slow reflection of one lakehouse must not block a
	// login to a different one. Concurrent logins to the SAME item queue here,
	// so they share one reflection instead of racing to redo it — which
	// previously meant N clients each DROPping the same table under each other.
	it := r.item(itemID)
	it.mu.Lock()
	defer it.mu.Unlock()

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
		fp, err := deltaFingerprint(st, itemID, name)
		if err != nil {
			// Not a Delta table (no _delta_log): skipped, not fatal — same as a
			// stray folder under Tables/ has always been.
			continue
		}
		if prev, ok := it.seen[name]; ok && prev == fp {
			continue
		}
		tbl, err := ReadDeltaTable(st, itemID, name)
		if err != nil {
			continue
		}
		if err := reflectTable(ctx, db, name, tbl); err != nil {
			// Deliberately not recorded: this table must be retried. Tables
			// already recorded above stay recorded, which is what lets a
			// cancelled login resume rather than restart.
			return done, fmt.Errorf("reflect %q: %w", name, err)
		}
		it.seen[name] = fp
		done = append(done, name)
	}
	return done, nil
}

// deltaFingerprint identifies a Delta table's current state cheaply enough to
// check on every login: the names of its _delta_log commits, which change on
// every write and require listing one directory rather than reading any data.
//
// It deliberately does not read the commit CONTENTS or the Parquet. Reading the
// log to resolve active files would be correct too, and more precise, but it
// costs a read per commit on a path whose whole purpose is to be near-free when
// nothing has changed. A new commit that somehow left the data identical would
// cause one needless reload, which is the harmless direction to be wrong in.
func deltaFingerprint(st *store.Store, itemID, name string) (string, error) {
	logDir := path.Join("Tables", name, "_delta_log")
	entries, err := st.ListOneLakePaths(itemID, logDir, false)
	if err != nil {
		return "", err
	}
	var commits []string
	for _, e := range entries {
		if strings.HasSuffix(e.RelPath, ".json") {
			commits = append(commits, e.RelPath)
		}
	}
	if len(commits) == 0 {
		return "", fmt.Errorf("no _delta_log commits under %q", logDir)
	}
	sort.Strings(commits)
	return strings.Join(commits, "\n"), nil
}

// reflectTable drops and recreates one table, then loads its rows over the TDS
// bulk-copy protocol — how a warehouse is actually loaded: rows stream in
// binary and the server parses no SQL for them at all.
//
// It used to build INSERT ... VALUES text as an alternative, for the SQLite
// handle the unit tests injected. That made load cost scale with CHARACTER
// COUNT rather than row count, and SQL Server's parse cost grows faster than
// linearly with statement size, so a wide table degraded sharply: 20,000 rows x
// 100 columns measured 123s of statement execution against 0.13s of reading the
// Delta. Bulk copy does the same load in about a second.
//
// That branch is gone with the SQLite double. Reflection has exactly one
// production caller, and the backend it passes is only ever built by
// tds.NewSQLServerBackend, so the text path was unreachable in production and
// untestable without a double — as were the nprefix parameter and literal()
// that existed solely to serve it.
func reflectTable(ctx context.Context, db *sql.DB, name string, tbl *Table) error {
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
	return bulkInsert(ctx, db, q, tbl)
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

// quoteIdent wraps an identifier in brackets (T-SQL; SQLite accepts them too).
func quoteIdent(s string) string {
	return "[" + strings.ReplaceAll(s, "]", "]]") + "]"
}
