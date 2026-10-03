package warehouse

// Warehouse write versioning and retention (docs/35-warehouse-time-travel.md,
// Phases 4 and 5).
//
// A Lakehouse table has a version history for free: it is Delta, and Delta
// keeps every commit. A Warehouse table lives in the SQL Server sidecar and has
// none. This file gives it one by committing to Delta, under the item's own
// Tables/<name>, after each statement the TDS front saw the engine accept --
// the same place a real Warehouse keeps its data -- so the existing reader
// (ReadDeltaTableAsOf) and the existing resolver (TimeTravelResolver) serve a
// warehouse unchanged.
//
// # Semantics, taken from Fabric rather than invented
//
//   - A version is a statement, not a transaction or a time slice.
//   - History belongs to the table OBJECT. sp_rename moves it with the table;
//     DROP TABLE ends it. A dbt rebuild (build a temp, swap) therefore starts a
//     new history for the swapped-in table, as it does on a real Warehouse,
//     where the dropped table's past is not reachable by name.
//
// # What is deliberately not versioned, so nobody finds out by surprise
//
//   - Writes the TDS front cannot see: a stored procedure's body, a
//     BULK INSERT/bcp stream, a pipeline Script activity (which uses the
//     control-plane connection, not the wire), a statement whose response
//     carries a result set (UPDATE ... OUTPUT).
//   - A table outside the dbo schema, and one over MaxVersionedRows.
//   - A VIEW: it holds no rows.
//
// A skipped table is logged by the caller, never silent.

import (
	"context"
	"database/sql"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/calvinchengx/fabric-emulator/internal/tsql"
)

// MaxVersionedRows bounds the table a statement will snapshot. A snapshot reads
// the whole table, so a loop of single-row INSERTs into a large table is
// quadratic; past this size the table is left unversioned and the caller says so.
const MaxVersionedRows = 500_000

// versionMu serialises the read-the-log, write-the-next-commit step. Two
// sessions writing one table would otherwise both compute the same next
// version and the second commit would overwrite the first.
var versionMu sync.Mutex

// SnapshotTable commits the table's current contents as the next version of
// its Delta history. skipped is non-empty when the table was deliberately not
// versioned (and why); the error is for a failure that should not have happened.
func SnapshotTable(ctx context.Context, db *sql.DB, st *store.Store, itemID, schema, name string) (actual, skipped string, err error) {
	if schema != "" && !strings.EqualFold(schema, "dbo") {
		return "", "schema " + schema + " is not versioned (only dbo)", nil
	}
	it, err := st.GetItemByID(itemID)
	if err != nil {
		return "", "", fmt.Errorf("version %q: item %q not found: %w", name, itemID, err)
	}
	// The engine's own spelling of the name: a CI database lets a client write
	// dbo.CUSTOMER for a table created as Customer, and two folders for one
	// table would split its history.
	err = db.QueryRowContext(ctx,
		"SELECT TABLE_NAME FROM INFORMATION_SCHEMA.TABLES WHERE TABLE_TYPE='BASE TABLE' AND TABLE_SCHEMA='dbo' AND TABLE_NAME=@p1",
		name).Scan(&actual)
	if err == sql.ErrNoRows {
		return "", "not a base table", nil // a view, or already gone
	}
	if err != nil {
		return "", "", fmt.Errorf("version %q: %w", name, err)
	}
	var n int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT_BIG(*) FROM ["+strings.ReplaceAll(actual, "]", "]]")+"]").Scan(&n); err != nil {
		return "", "", fmt.Errorf("version %q: counting rows: %w", actual, err)
	}
	if n > MaxVersionedRows {
		return "", fmt.Sprintf("%d rows is over the %d-row versioning limit", n, MaxVersionedRows), nil
	}
	tbl, kinds, err := readSQLTable(ctx, db, strings.ReplaceAll(actual, "]", "]]"))
	if err != nil {
		return "", "", fmt.Errorf("version %q: reading: %w", actual, err)
	}
	return actual, "", commitVersion(st, it.WorkspaceID, itemID, actual, tbl, kinds)
}

// commitVersion writes tbl as the next commit of Tables/<name>: the new data
// file added, every earlier one removed, the schema restated.
func commitVersion(st *store.Store, wsID, itemID, name string, tbl *Table, kinds []colType) error {
	pq, err := encodeParquet(tbl, kinds)
	if err != nil {
		return err
	}
	versionMu.Lock()
	defer versionMu.Unlock()
	root := path.Join("Tables", name)
	version, err := nextCommitVersion(st, itemID, root)
	if err != nil {
		return err
	}
	var removes []string
	if version > 0 {
		if removes, _, err = activeFiles(st, itemID, root); err != nil {
			return err
		}
	}
	dataFile := fmt.Sprintf("part-%d.parquet", version)
	if err := st.CreateOneLakePath(&store.OneLakePath{
		WorkspaceID: wsID, ItemID: itemID, RelPath: path.Join(root, dataFile), Content: pq,
	}, false); err != nil {
		return err
	}
	return st.CreateOneLakePath(&store.OneLakePath{
		WorkspaceID: wsID, ItemID: itemID,
		RelPath: path.Join(root, "_delta_log", commitFileName(version)),
		Content: commitJSONMeta(tbl.Columns, kinds, dataFile, len(pq), len(tbl.Rows), removes, version, st.Now()*1000, true),
	}, false)
}

// DropTableHistory ends a dropped table's history: its Delta folder goes.
func DropTableHistory(st *store.Store, itemID, name string) error {
	versionMu.Lock()
	defer versionMu.Unlock()
	actual, found, err := findLakehouseTableName(st, itemID, name)
	if err != nil || !found {
		return err
	}
	return st.DeleteOneLakePath(itemID, path.Join("Tables", actual))
}

// RenameTableHistory moves a renamed table's history with it.
func RenameTableHistory(st *store.Store, itemID, from, to string) error {
	versionMu.Lock()
	defer versionMu.Unlock()
	actual, found, err := findLakehouseTableName(st, itemID, from)
	if err != nil || !found {
		return err
	}
	return st.RenameOneLakePath(itemID, path.Join("Tables", actual), path.Join("Tables", to))
}

// RetentionCutoff is the oldest instant a time-travel query may name.
func RetentionCutoff(st *store.Store, days int) time.Time {
	return time.Unix(st.Now(), 0).Add(-time.Duration(days) * 24 * time.Hour)
}

type logCommit struct {
	ts      int64
	dated   bool
	adds    []string
	removes []string
}

// ExpireVersions removes the data files only versions older than the retention
// window can reach, and returns how many it removed.
//
// The commit LOG is kept whole. Deleting leading commits would leave a log that
// delta-rs and Spark cannot replay (they need version 0 or a checkpoint), and
// the log is metadata -- it is the data files that cost space. This is Delta's
// own VACUUM: history is what the log says, and which of it still has bytes
// behind it is a separate question.
//
// The state at the cutoff itself is kept, since a query for exactly the cutoff
// instant must still be answerable: the base version is the last commit made at
// or before it, and every file live at the base or added after it is protected.
// Nothing the current table needs can be removed, because the current state is
// live at the newest version, which is never before the base.
func ExpireVersions(st *store.Store, itemID, name string, retentionDays int) (int, error) {
	versionMu.Lock()
	defer versionMu.Unlock()
	root := path.Join("Tables", name)
	entries, err := st.ListOneLakePaths(itemID, path.Join(root, "_delta_log"), false)
	if err != nil {
		return 0, err
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.RelPath, ".json") {
			names = append(names, e.RelPath)
		}
	}
	sort.Strings(names)
	commits := make([]logCommit, len(names))
	for i, n := range names {
		p, err := st.GetOneLakePath(itemID, n)
		if err != nil {
			return 0, err
		}
		actions, err := parseCommit(n, p.Content)
		if err != nil {
			return 0, err
		}
		c := &commits[i]
		c.ts, c.dated = commitTimestamp(actions)
		for _, a := range actions {
			if a.Add != nil {
				c.adds = append(c.adds, a.Add.Path)
			}
			if a.Remove != nil {
				c.removes = append(c.removes, a.Remove.Path)
			}
		}
	}
	cutoff := RetentionCutoff(st, retentionDays).UnixMilli()
	base := -1
	for i, c := range commits {
		if c.dated && c.ts <= cutoff {
			base = i
		}
	}
	if base <= 0 {
		return 0, nil // nothing older than the one version the window must keep
	}
	protected := map[string]bool{}
	for i := 0; i <= base; i++ {
		for _, f := range commits[i].adds {
			protected[f] = true
		}
		for _, f := range commits[i].removes {
			delete(protected, f)
		}
	}
	for _, c := range commits[base+1:] {
		for _, f := range c.adds {
			protected[f] = true
		}
	}
	removed := 0
	for _, c := range commits[:base+1] {
		for _, f := range c.adds {
			if protected[f] {
				continue
			}
			p := path.Join(root, f)
			if _, err := st.GetOneLakePath(itemID, p); err != nil {
				continue // already expired
			}
			if err := st.DeleteOneLakePath(itemID, p); err != nil {
				return removed, err
			}
			removed++
		}
	}
	return removed, nil
}

// WarehouseTimeTravelResolver is TimeTravelResolver for a Warehouse: the same
// reading of the table's Delta history, inside the retention window. An instant
// older than the window is refused by name, rather than failing later on a data
// file that has been expired.
func WarehouseTimeTravelResolver(st *store.Store, itemID string, retentionDays int) tsql.TimeTravelResolver {
	inner := TimeTravelResolver(st, itemID)
	return func(table string, asOf time.Time) (*tsql.TimeTravelSnapshot, bool, error) {
		if cutoff := RetentionCutoff(st, retentionDays); asOf.Before(cutoff) {
			return nil, false, fmt.Errorf("%s is older than this warehouse's %d-day data retention window (oldest available: %s)",
				asOf.UTC().Format(time.RFC3339), retentionDays, cutoff.UTC().Format(time.RFC3339))
		}
		return inner(table, asOf)
	}
}
