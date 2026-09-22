package warehouse

import (
	"context"
	"database/sql"
	"fmt"
	"log"
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

// ReflectWithExternal is Reflect with external shortcut tables (ADLS Gen2,
// Amazon S3, Dataverse) included: without an ExternalDelta, one of those is
// simply not among the tables reflected, the same as a stray non-Delta folder.
func ReflectWithExternal(ctx context.Context, db *sql.DB, st *store.Store, itemID string, external ExternalDelta) ([]string, error) {
	return (&Reflector{External: external}).Reflect(ctx, db, st, itemID)
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
// progress.
//
// A fingerprint alone is NOT enough to skip on, and the first version of this
// made that mistake. It answers "has the source changed" while the caller needs
// "is the target ready", and those come apart whenever the target is missing for
// a reason the Delta log cannot see. The result was worse than what it replaced:
// a slow, honest `Login timeout expired` became a fast login onto a half-built
// database and `Invalid object name 'silver_orders'`, which names neither
// reflection nor the fingerprint. So every skip is now corroborated against what
// the destination actually contains, and anything uncertain is reflected again.
//
// The zero value is a valid, empty Reflector.
type Reflector struct {
	mu    sync.Mutex              // guards items
	items map[string]*reflectItem // itemID -> its lock and fingerprints
	// External reads an ADLS Gen2, Amazon S3 or Dataverse shortcut's Delta table
	// (docs/61). Nil (the zero value) means those tables are not among the ones
	// reflected — the same treatment as a folder with no _delta_log.
	External ExternalDelta
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

	tables, err := tableSources(st, itemID)
	if err != nil {
		return nil, err
	}

	// What is ACTUALLY in the destination right now. One round trip for the
	// whole item, not one per table.
	//
	// A fingerprint answers "has the source changed". The caller needs "is the
	// target ready", and those differ whenever the target is missing for a
	// reason the source cannot see — a fresh database, a dropped table, a
	// reflection that recorded a name and then lost the row. Skipping on the
	// source alone turned a slow-but-honest login timeout into a fast login
	// onto a half-built database, and `Invalid object name 'silver_orders'`
	// names neither reflection nor the fingerprint that caused it.
	//
	// Unknown means reflect everything. A redundant bulk copy costs seconds; a
	// wrong skip costs a missing table and an error that points nowhere.
	present, known := existingTables(ctx, db)

	var done []string
	for _, src := range tables {
		name := src.name
		if src.external != nil && r.External == nil {
			// A real shortcut, but nothing here can read its target: skipped, not
			// fatal, same treatment as a stray folder with no _delta_log.
			continue
		}
		var fp string
		var err error
		if src.external != nil {
			fp, err = deltaFingerprintExternal(r.External, src.external)
		} else {
			fp, err = deltaFingerprint(st, src.item, src.root)
		}
		if err != nil {
			// Not a Delta table (no _delta_log): skipped, not fatal — same as a
			// stray folder under Tables/ has always been.
			continue
		}
		if src.blocked != "" {
			// What was reflected depends on the block as well as the log: a table
			// that is now blocked, or was and no longer is, must be reflected again.
			fp += "\nblocked:" + src.blocked
		}
		if prev, ok := it.seen[name]; ok && prev == fp && known && present[strings.ToLower(name)] {
			continue
		}
		if src.blocked != "" {
			cols, err := blockedColumns(st, src.item, src.root, name)
			if err != nil {
				return done, fmt.Errorf("reflect %q: reading the Delta schema: %w", name, err)
			}
			if err := reflectBlocked(ctx, db, name, cols, src.blocked); err != nil {
				return done, fmt.Errorf("reflect %q: %w", name, err)
			}
			it.seen[name] = fp
			done = append(done, name)
			continue
		}
		var tbl *Table
		if src.external != nil {
			tbl, err = readExternalDeltaTable(r.External, src.external, name)
		} else {
			tbl, err = ReadDeltaTableAt(st, src.item, src.root, name)
		}
		if err != nil {
			// NOT a skip. The fingerprint step already proved this is a Delta
			// table — it found _delta_log commits — so failing to read it now is
			// a real failure, not the stray-folder case above. Continuing here
			// let a login report success with a table missing, which is how a
			// caller ends up querying something that was never created.
			return done, fmt.Errorf("reflect %q: reading the Delta table: %w", name, err)
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

// existingTables lists the tables and views in the destination database, lowercased.
//
// The second return is whether the answer is TRUSTWORTHY. Anything that goes
// wrong — no permission on sys.tables, a cancelled context, an engine without
// it — reports false, and the caller then reflects everything rather than
// trusting a fingerprint it cannot corroborate. Being wrong in the direction of
// extra work is the whole point of the flag.
func existingTables(ctx context.Context, db *sql.DB) (map[string]bool, bool) {
	// Views count: a blocked shortcut is reflected as one (reflectBlocked), and
	// the fingerprint says which of the two it should be.
	rows, err := db.QueryContext(ctx, "SELECT name FROM sys.tables UNION ALL SELECT name FROM sys.views")
	if err != nil {
		return nil, false
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, false
		}
		out[strings.ToLower(n)] = true
	}
	if rows.Err() != nil {
		return nil, false
	}
	return out, true
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
func deltaFingerprint(st *store.Store, itemID, root string) (string, error) {
	logDir := path.Join(root, "_delta_log")
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
	// Where the log is read from is part of the fingerprint: a shortcut re-pointed
	// at another table whose commits happen to be named alike is a different table.
	return itemID + "|" + root + "\n" + strings.Join(commits, "\n"), nil
}

// tableSource is one table the endpoint shows: the name it is shown under, and
// the item and folder its Delta log lives in — its own Tables/<name>, or, for a
// shortcut, the folder the shortcut points at.
type tableSource struct {
	name, item, root string
	// blocked is why the table cannot be read, when it cannot: a OneLake shortcut
	// whose source has row or column security, on an endpoint in delegated identity
	// mode. Such a table is reflected as a view that refuses every read with this
	// reason. Never set alongside external — delegated-mode blocking is a OneLake
	// security question, and an external target carries no OneLake security to ask.
	blocked string
	// external is set instead of item/root for an ADLS Gen2, Amazon S3 or
	// Dataverse shortcut: its Delta table is read over HTTP through Reflector's
	// External, not the store.
	external *store.Shortcut
}

// tableSources lists a lakehouse's tables: every folder under Tables/, every
// OneLake shortcut there, and every ADLS Gen2, Amazon S3 or Dataverse shortcut
// there too — "Shortcuts function as tables in the SQL analytics endpoint" names
// no kind. A folder and a shortcut of one name cannot coexist in OneLake; if the
// store ever holds both, the folder wins, and a OneLake shortcut is preferred
// over an external one of the same name for the same reason (docs/61). An
// external table is marked, not read here: the Reflect loop reads it through
// Reflector.External, since reading it needs a credential this package does not
// resolve.
func tableSources(st *store.Store, itemID string) ([]tableSource, error) {
	dirs, err := st.ListOneLakePaths(itemID, "Tables", false)
	if err != nil {
		return nil, err
	}
	var out []tableSource
	have := map[string]bool{}
	for _, d := range dirs {
		if !d.IsDir {
			continue
		}
		name := strings.TrimPrefix(d.RelPath, "Tables/")
		have[strings.ToLower(name)] = true
		out = append(out, tableSource{name: name, item: itemID, root: d.RelPath})
	}
	shortcuts, err := st.ListShortcuts(itemID)
	if err != nil {
		return nil, err
	}
	var lake *store.Item
	for _, sc := range shortcuts {
		if sc.Path != "Tables" || have[strings.ToLower(sc.Name)] {
			continue
		}
		if sc.IsExternalTarget() {
			if sc.TargetLocation == "" || sc.ConnectionID == "" {
				continue // not a real shortcut this reader can address
			}
			out = append(out, tableSource{name: sc.Name, external: sc})
			continue
		}
		if sc.TargetItem == "" {
			continue
		}
		src := tableSource{name: sc.Name, item: sc.TargetItem, root: sc.TargetPath}
		// The lakehouse is looked up only when there is a shortcut to judge, so a
		// lakehouse with none costs nothing.
		if lake == nil {
			if lake, err = st.GetItemByID(itemID); err != nil {
				return nil, err
			}
		}
		if src.blocked, err = st.DelegatedShortcutBlock(lake, sc); err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	return out, nil
}

// deltaFingerprintExternal is deltaFingerprint for an external shortcut: the
// commit list stands in for the Delta log, and the target's own identity is
// folded in so a shortcut re-pointed at a different connection or location,
// whose commits happen to be named alike, is not mistaken for unchanged.
func deltaFingerprintExternal(external ExternalDelta, sc *store.Shortcut) (string, error) {
	commits, err := activeFilesFingerprint(external, sc)
	if err != nil {
		return "", err
	}
	return sc.TargetType + "|" + sc.ConnectionID + "|" + sc.TargetLocation + "/" + sc.TargetPath + "/" + sc.TargetTable +
		"\n" + strings.Join(commits, "\n"), nil
}

// activeFilesFingerprint lists an external shortcut's commits for
// deltaFingerprintExternal, refusing a target with none the same way
// deltaFingerprint does for a non-Delta folder.
func activeFilesFingerprint(external ExternalDelta, sc *store.Shortcut) ([]string, error) {
	commits, err := external.ExternalDeltaCommits(sc)
	if err != nil {
		return nil, err
	}
	if len(commits) == 0 {
		return nil, fmt.Errorf("no _delta_log commits under shortcut %q", sc.Name)
	}
	return commits, nil
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
// dropReflected removes whatever a reflection left under a name: the table, or the
// view a blocked shortcut is reflected as. A synced OneLake row policy (docs/60)
// holds the table even without schema binding, so DROP TABLE would fail. It is
// dropped with the table's grants; the security sync that runs after reflection
// recreates both, and until it does a reader restricted by them has no SELECT on
// the new table.
func dropReflected(ctx context.Context, db *sql.DB, name string) error {
	q := quoteIdent(name)
	if _, err := db.ExecContext(ctx, `DECLARE @rls nvarchar(max) = N'';
SELECT @rls += N'DROP SECURITY POLICY ' + QUOTENAME(SCHEMA_NAME(p.schema_id)) + N'.' + QUOTENAME(p.name) + N';'
FROM sys.security_policies p JOIN sys.security_predicates sp ON sp.object_id = p.object_id
WHERE sp.target_object_id = OBJECT_ID(@table) AND p.name LIKE 'OLS[_]rls[_]%';
EXEC sp_executesql @rls;`, sql.Named("table", q)); err != nil {
		return err
	}
	// By type, since DROP VIEW and DROP TABLE each refuse the other's object.
	_, err := db.ExecContext(ctx, `IF OBJECT_ID(@table, N'V') IS NOT NULL DROP VIEW `+q+`;
IF OBJECT_ID(@table, N'U') IS NOT NULL DROP TABLE `+q+`;`, sql.Named("table", q))
	return err
}

// blockedColumns names the columns of a Delta table without reading its data: a
// blocked shortcut is never read, only described. A log with no schema is read
// whole, as it has nothing else to say.
func blockedColumns(st *store.Store, itemID, root, name string) ([]string, error) {
	if _, schema, err := activeFiles(st, itemID, root); err == nil {
		if cols := omit(schema.Cols, schema.Nested); len(cols) > 0 {
			return cols, nil
		}
	}
	tbl, err := ReadDeltaTableAt(st, itemID, root, name)
	if err != nil {
		return nil, err
	}
	return tbl.Columns, nil
}

// reflectBlocked reflects a blocked shortcut as a view that refuses every read,
// for every reader, saying why. "Blocks access to that shortcut" binds an owner
// too, which a DENY cannot (db_owner is not bound by one), and a view's own error
// reaches every client as the engine's message. Its columns are the table's, so
// a query that names one is refused for the reason and not for a missing column.
func reflectBlocked(ctx context.Context, db *sql.DB, name string, columns []string, reason string) error {
	if err := dropReflected(ctx, db, name); err != nil {
		return err
	}
	defs := make([]string, len(columns))
	for i, c := range columns {
		defs[i] = "CAST(NULL AS sql_variant) AS " + quoteIdent(c)
	}
	msg := "This shortcut is blocked: its source table has " + reason + ", and a SQL analytics endpoint in delegated identity mode reads OneLake as the item owner, who can only read a table whole. Switch the endpoint to user identity mode to read it."
	_, err := db.ExecContext(ctx, "CREATE VIEW "+quoteIdent(name)+" AS SELECT "+strings.Join(defs, ", ")+
		" FROM (SELECT 1 AS x) AS b WHERE 1 = (SELECT CAST(N'"+strings.ReplaceAll(msg, "'", "''")+"' AS int))")
	return err
}

func reflectTable(ctx context.Context, db *sql.DB, name string, tbl *Table) error {
	q := quoteIdent(name)
	if err := dropReflected(ctx, db, name); err != nil {
		return err
	}
	if len(tbl.Skipped) > 0 {
		// Named, because "some columns might not be available" is what Fabric
		// says and it is a miserable thing to debug without the name.
		log.Printf("warehouse: %s: %d column(s) not representable in SQL and "+
			"omitted (nested types): %v", name, len(tbl.Skipped), tbl.Skipped)
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
	switch t := v.(type) {
	case Decimal:
		return t.String()
	case int16:
		// The bulk-copy encoder's integer arm accepts int, int32, int64,
		// float32 and float64 — and nothing else. An int16 falls through to
		// its default and fails the whole copy with "mssql: invalid type for
		// int column: int16", which is a hard error on every table carrying a
		// Delta smallint.
		//
		// Widening the VALUE is safe and does not undo the width fix: sqlType
		// already read the int16 and declared the column SMALLINT, so the
		// destination type is settled before this runs. Only the wire encoding
		// is affected, and int64 is what the encoder wants.
		//
		// Worth knowing: the only tests that execute this need a real SQL
		// Server (WAREHOUSE_MSSQL_DSN), so it is green on a laptop either way.
		return int64(t)
	case Date:
		// time.Time, not the day count: the destination column is DATE, and
		// handing it the integer would put the bug back one layer down.
		return t.T
	case Timestamp:
		return t.T
	}
	return v
}

// varcharType is what a Delta string becomes.
//
// varchar, NOT nvarchar. Fabric's documented Delta->SQL map is
// `STRING -> varchar(8000)` for a Lakehouse SQL analytics endpoint, and its
// unsupported-types table says of nchar/nvarchar: "Use char and varchar
// respectively, as there's no similar unicode data type in Parquet." Emitting
// nvarchar was a plain divergence — Parquet has one string type and Fabric
// surfaces it as varchar.
//
// The collation Fabric declares (`Latin1_General_100_BIN2_UTF8`) is NOT emitted
// here: it changes comparison and sort semantics on the sidecar, which is a
// larger behavioural change than a type name and is not what this fixes.
const varcharType = "VARCHAR(8000)"

// sqlType picks a column's SQL type from its first non-null value (default
// NVARCHAR). The type names are valid in both SQL Server and SQLite.
//
// Reading the VALUE is sound only because the reader now decodes each logical
// type into a distinct Go type — Date, Timestamp, Decimal, []byte, int32. It
// was not sound before: date, timestamp and int all arrived as int64 and all
// three reflected as BIGINT.
func sqlType(tbl *Table, col int) string {
	for _, row := range tbl.Rows {
		switch v := row[col].(type) {
		case bool:
			return "BIT"
		case Date:
			// The whole point of carrying Date through the reader: a date that
			// reflects as BIGINT reads as 20627 in a report and clashes on any
			// join against a real date.
			return "DATE"
		case Timestamp:
			return "DATETIME2"
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
		case int16:
			// TINYINT, BYTE, SMALLINT and SHORT all map to smallint — Fabric
			// has no tinyint for persisted storage.
			return "SMALLINT"
		case int32:
			// Fabric maps a Delta int to INT. Widening it to BIGINT is not
			// wrong-looking, which is why it survived: it only shows up when a
			// client compares the endpoint's schema against Fabric's.
			return "INT"
		case int64:
			return "BIGINT"
		case float32:
			// Delta FLOAT/REAL -> real; DOUBLE -> float. Reflecting both as
			// FLOAT is the same one-width-too-wide failure as the integers.
			return "REAL"
		case float64:
			return "FLOAT"
		case []byte:
			return "VARBINARY(4000)"
		case string:
			return varcharType
		}
	}
	return varcharType
}

// quoteIdent wraps an identifier in brackets (T-SQL; SQLite accepts them too).
func quoteIdent(s string) string {
	return "[" + strings.ReplaceAll(s, "]", "]]") + "]"
}
