package warehouse

// Binding Phase 1 (ReadDeltaTableAsOf) to Phase 2/3's hook in package tsql
// (docs/35-warehouse-time-travel.md, Phase 3). This file is the only place
// that knows both halves: how to read a Delta table's history (this package)
// and what shape a resolved table needs to be in for Adapt to splice it into
// a statement as a #temp table (tsql.TimeTravelSnapshot). Everything else
// about materialising the hint lives in package tsql, which has no idea what
// a Delta table or a commit log is.
//
// # Why literal SQL text, not a bulk copy
//
// reflectTable loads a table over the TDS bulk-copy protocol specifically
// because INSERT ... VALUES text scales with character count, not row count,
// and measured 1000x slower on a wide table (reflect.go). That option is not
// available here: a time-travel hint is resolved inside Adapt, which runs at
// the wire layer on a statement's TEXT, before it is forwarded to the engine
// over a connection this package never sees (internal/tds splices the
// client's session to the backend byte-for-byte). The only channel available
// to hand the engine a historical snapshot is the statement itself, so this
// is literal INSERT text — the same trade-off docs/35 names directly: "a
// materialised snapshot is not a Fabric MPP snapshot... performance
// characteristics do not [match], and nothing here should claim otherwise."
// It is sized to a time-travel query's own result set, not a full reflect, so
// the cost this trades away is smaller than it looks.
//
// # Schema as of the timestamp vs today
//
// ReadDeltaTableAsOf already recovers the schema that was in effect at a given
// commit — it has to, to assemble the right Parquet columns. ReadDeltaTable
// (no stopping condition) recovers today's. The gap between the two is
// exactly what a consumer's SELECT * sees that the hint should have refused
// (TimeTravelSnapshot.CurrentColumns, checked in package tsql).

import (
	"encoding/hex"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/calvinchengx/fabric-emulator/internal/tsql"
)

// TimeTravelResolver returns a tsql.TimeTravelResolver bound to one
// lakehouse item: given a table name and an instant, it answers with that
// table's historical version, ready for Adapt to materialise as a #temp
// table (docs/35, Phase 3).
//
// Scope: only a table living under the item's own Tables/<name> — a plain
// lakehouse table — is resolved. A shortcut (OneLake or external) answers
// ok=false, the same "leave it alone" treatment a #temp table or an unrelated
// object gets, rather than a wrong answer: ReadDeltaTableAsOf itself only
// knows how to read Tables/<name>, and extending it to a shortcut's own
// target is new surface this phase does not need to cover to be shippable —
// Phase 3's bar is the SQL analytics endpoint's own tables, which is where
// the real multi-commit Delta history lives (docs/35, "Two surfaces, and they
// are not equally hard").
func TimeTravelResolver(st *store.Store, itemID string) tsql.TimeTravelResolver {
	return func(table string, asOf time.Time) (*tsql.TimeTravelSnapshot, bool, error) {
		// Resolved case-insensitively: SQL Server's default collation is
		// CI_AS (reflect.go's defaultCollation), so a client may legitimately
		// write `dbo.customer` against a table whose OneLake folder — and
		// therefore its exact name here — is "Customer". Looking it up by
		// exact case would silently fail to time-travel a reference the real
		// engine resolves just fine.
		name, found, err := findLakehouseTableName(st, itemID, table)
		if err != nil || !found {
			return nil, false, err
		}
		root := path.Join("Tables", name)
		// Cheap existence/shape check, same test Reflect's own loop uses to
		// decide "not a Delta table: skip it, not an error" (reflect.go).
		if _, err := deltaFingerprint(st, itemID, root); err != nil {
			return nil, false, nil
		}

		asOfTbl, err := ReadDeltaTableAsOf(st, itemID, name, asOf)
		if err != nil {
			// It IS a Delta table, but the instant cannot be honoured — most
			// often a timestamp before the table's first commit. A real
			// failure, not a "nothing to do here".
			return nil, false, err
		}
		current, err := ReadDeltaTable(st, itemID, name)
		if err != nil {
			return nil, false, err
		}

		types := make([]string, len(asOfTbl.Columns))
		for i := range asOfTbl.Columns {
			types[i] = sqlType(asOfTbl, i)
		}
		rows := make([][]string, len(asOfTbl.Rows))
		for r, row := range asOfTbl.Rows {
			lits := make([]string, len(asOfTbl.Columns))
			for c := range lits {
				var v any
				if c < len(row) {
					v = row[c]
				}
				lits[c] = sqlLiteral(v)
			}
			rows[r] = lits
		}

		return &tsql.TimeTravelSnapshot{
			Columns:        asOfTbl.Columns,
			SQLTypes:       types,
			RowLiterals:    rows,
			CurrentColumns: current.Columns,
		}, true, nil
	}
}

// findLakehouseTableName resolves table to the exact-case name of a folder
// under this item's Tables/, matching case-insensitively (strings.EqualFold)
// since that is what the SQL engine itself does. found=false, err=nil means
// no such folder at all — not necessarily a failure, since the name may be a
// shortcut, a system object, or simply not a table.
func findLakehouseTableName(st *store.Store, itemID, table string) (string, bool, error) {
	dirs, err := st.ListOneLakePaths(itemID, "Tables", false)
	if err != nil {
		return "", false, nil
	}
	for _, d := range dirs {
		if !d.IsDir {
			continue
		}
		name := strings.TrimPrefix(d.RelPath, "Tables/")
		if strings.EqualFold(name, table) {
			return name, true, nil
		}
	}
	return "", false, nil
}

// sqlLiteral renders one reflected value as a T-SQL literal expression, ready
// to splice into an INSERT ... VALUES list. The inverse of bulkValue, which
// does the same job for the parameterised bulk-copy path reflectTable uses.
func sqlLiteral(v any) string {
	switch t := v.(type) {
	case nil:
		return "NULL"
	case bool:
		if t {
			return "1"
		}
		return "0"
	case int16:
		return strconv.FormatInt(int64(t), 10)
	case int32:
		return strconv.FormatInt(int64(t), 10)
	case int64:
		return strconv.FormatInt(t, 10)
	case float32:
		// bitSize 32 so the shortest decimal that round-trips the float32 is
		// used, rather than one with float64-widening noise in its tail
		// (1.1 becoming 1.100000023841858).
		return strconv.FormatFloat(float64(t), 'g', -1, 32)
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	case Decimal:
		// Its exact unscaled digits with the decimal point placed — already a
		// valid SQL literal (reflect.go's bulkValue relies on the same fact).
		return t.String()
	case Date:
		return "'" + t.String() + "'"
	case Timestamp:
		return "'" + t.String() + "'"
	case []byte:
		return "0x" + hex.EncodeToString(t)
	case string:
		return "N'" + strings.ReplaceAll(t, "'", "''") + "'"
	default:
		// Unreached by any value goValue produces; a safe fallback rather than
		// a panic if a new Delta type is ever added to readParquet.
		return "N'" + strings.ReplaceAll(fmt.Sprint(t), "'", "''") + "'"
	}
}
