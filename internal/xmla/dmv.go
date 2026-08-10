package xmla

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/semanticmodel"
)

// The `$SYSTEM.TMSCHEMA_*` metadata DMVs, projected from the emulator's own
// model. These arrive as an `Execute` carrying a SQL-ish `<Statement>`, not as
// a `Discover` — the same envelope and rowset writer as DAX `EVALUATE`, with a
// different statement grammar. sempy issues them through `evaluate_dax`, e.g.
//
//	SELECT [ID] AS [SemPyTableID], [Name] AS [SemPyTableName]
//	FROM $SYSTEM.TMSCHEMA_TABLES
//
// The columns implemented here are the ones sempy 0.14.2 actually selects,
// enumerated from the installed wheel rather than from the spec: the Learn
// pages for these rowsets are stubs, and guessing wire contracts has been
// expensive on this workstream. A selected column we do not model is an error,
// not an empty string — a plausible blank is how a wrong shape survives.

// dmvStatement matches `SELECT <projection> FROM $SYSTEM.<NAME>`.
var dmvStatement = regexp.MustCompile(`(?is)^\s*SELECT\s+(.+?)\s+FROM\s+\$SYSTEM\.(\w+)\s*;?\s*$`)

// dmvProjection matches `[Col]` or `[Col] AS [Alias]`.
var dmvProjection = regexp.MustCompile(`(?i)\[(\w+)\]\s*(?:AS\s*\[(\w+)\])?`)

// IsDMV reports whether a statement is a `$SYSTEM.*` DMV query rather than DAX.
func IsDMV(statement string) bool { return dmvStatement.MatchString(statement) }

// projection is one selected column: its source name and the name it is
// returned under (the alias when given — sempy joins DataFrames on the alias,
// so returning the source name would silently break its merges).
type projection struct{ source, out string }

// dmvIdent is what may become an XML element name in the response.
//
// dmvProjection's `\w+` groups already exclude the markup characters, but that
// is an implied invariant three regexes away from the writer. This states it at
// the boundary it protects: a selected name that is not a plain identifier is
// rejected before it can reach the rowset, and loosening the parser cannot
// silently loosen the writer.
var dmvIdent = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// DMV parses and answers a `$SYSTEM.TMSCHEMA_*` query against the model and its
// data. Data is required because the storage rowsets are *derived* from the
// rows we hold — record counts and distinct-value counts are exact, not
// estimates.
func DMV(model *semanticmodel.Model, data semanticmodel.Data, statement string) (Rowset, error) {
	m := dmvStatement.FindStringSubmatch(statement)
	if m == nil {
		return Rowset{}, fmt.Errorf("not a $SYSTEM DMV statement")
	}
	name := strings.ToUpper(m[2])

	var cols []projection
	for _, p := range dmvProjection.FindAllStringSubmatch(m[1], -1) {
		out := p[1]
		if p[2] != "" {
			out = p[2]
		}
		if !dmvIdent.MatchString(out) {
			return Rowset{}, fmt.Errorf("%s: %q is not a usable column name", name, out)
		}
		cols = append(cols, projection{source: p[1], out: out})
	}
	if len(cols) == 0 {
		return Rowset{}, fmt.Errorf("%s: no columns selected (SELECT * is not supported)", name)
	}

	rows, err := dmvRows(model, data, name)
	if err != nil {
		return Rowset{}, err
	}

	rs := Rowset{}
	for _, c := range cols {
		rs.Columns = append(rs.Columns, c.out)
	}
	for _, src := range rows {
		out := make([]string, len(cols))
		for i, c := range cols {
			v, ok := src[c.source]
			if !ok {
				return Rowset{}, fmt.Errorf("%s has no column %q", name, c.source)
			}
			out[i] = v
		}
		rs.Rows = append(rs.Rows, out)
	}
	return rs, nil
}

// dmvRows materialises one DMV as source-named cells. IDs are 1-based
// declaration order: TMSCHEMA IDs are opaque integers whose only contract is
// that they join (sempy merges partitions to tables on TableID), so stable
// ordinals satisfy the consumer without inventing server internals.
func dmvRows(model *semanticmodel.Model, data semanticmodel.Data, name string) ([]map[string]string, error) {
	if model == nil {
		return nil, fmt.Errorf("no model")
	}
	var rows []map[string]string

	switch name {
	case "TMSCHEMA_TABLES":
		for i, t := range model.Tables {
			rows = append(rows, map[string]string{"ID": objID("TMSCHEMA_TABLES", i),
				"ModelID": objID("TMSCHEMA_MODEL", 0), "Name": t.Name})
		}

	case "TMSCHEMA_COLUMNS":
		n := 0
		for ti, t := range model.Tables {
			for _, c := range t.Columns {
				n++
				rows = append(rows, map[string]string{
					"ID": objID("TMSCHEMA_COLUMNS", n-1), "TableID": objID("TMSCHEMA_TABLES", ti),
					"ExplicitName": c.Name, "Name": c.Name,
				})
			}
		}

	case "TMSCHEMA_PARTITIONS":
		// One partition per table: the emulator stores a table's rows as a
		// single unit (import) or one Delta entity (Direct Lake), so a second
		// partition would be a fiction the storage cannot back.
		for ti, t := range model.Tables {
			rows = append(rows, map[string]string{
				"ID":      objID("TMSCHEMA_PARTITIONS", ti),
				"TableID": objID("TMSCHEMA_TABLES", ti), "Name": t.Name + "-Partition",
			})
		}

	case "TMSCHEMA_RELATIONSHIPS":
		for i, r := range model.Relationships {
			rows = append(rows, map[string]string{"ID": objID("TMSCHEMA_RELATIONSHIPS", i), "Name": r.Name})
		}

	case "TMSCHEMA_HIERARCHIES":
		// None: the model format we parse carries no hierarchies, so this is
		// legitimately empty rather than unimplemented. An empty rowset still
		// carries its schema, which is what the client reads first.

	// --- storage rowsets -----------------------------------------------------
	// These are DERIVED, not invented. An earlier revision refused the whole
	// family as "VertiPaq physical statistics we do not have"; that was too
	// broad. Record counts and distinct-value counts are exact from the rows we
	// already hold, and segment count follows a documented formula, so refusing
	// them was withholding an answer we can give correctly.

	case "TMSCHEMA_PARTITION_STORAGES":
		// One storage per partition, and one partition per table (above), so
		// the ids line up by construction.
		for ti := range model.Tables {
			rows = append(rows, map[string]string{"ID": objID("TMSCHEMA_PARTITION_STORAGES", ti),
				"PartitionID": objID("TMSCHEMA_PARTITIONS", ti)})
		}

	case "TMSCHEMA_SEGMENT_MAP_STORAGES":
		for ti, t := range model.Tables {
			n := len(data.Rows(t.Name))
			rows = append(rows, map[string]string{
				"PartitionStorageID": id(ti),
				"RecordCount":        strconv.Itoa(n),
				"SegmentCount":       strconv.Itoa(segmentCount(n)),
			})
		}

	case "TMSCHEMA_COLUMN_STORAGES":
		n := 0
		for _, t := range model.Tables {
			for _, c := range t.Columns {
				n++
				rows = append(rows, map[string]string{
					"ColumnID":                  id(n - 1),
					"Statistics_DistinctStates": strconv.Itoa(distinctValues(data, t.Name, c.Name)),
				})
			}
		}

	default:
		if name == "TMSCHEMA_DELTA_TABLE_METADATA_STORAGES" {
			// TableName we have; FallbackReason we do not. Direct Lake falls
			// back to DirectQuery for specific documented causes (a SQL view,
			// SQL-based RLS, an unprocessed table) — none of which apply to a
			// table reading Delta here, so "no fallback" is semantically right.
			// Its *wire encoding* is unverified, and a wrong enum reads as a
			// real fallback reason, so this stays refused until a live client
			// names it. Narrow refusal, not a family-wide one.
			return nil, fmt.Errorf("%s: TableName is available but FallbackReason's "+
				"encoding is unverified; refusing rather than guessing an enum that "+
				"would read as a genuine fallback cause", name)
		}
		return nil, fmt.Errorf("unsupported DMV %s", name)
	}
	return rows, nil
}

// defaultSegmentRowCount is VertiPaq's documented DefaultSegmentRowCount:
// "the number of data rows per segment … The default value is 8,388,608
// (8*1024*1024) rows" (Analysis Services general server properties).
const defaultSegmentRowCount = 8 * 1024 * 1024

// segmentCount derives a partition's segment count from its row count. The
// same spec states "every table partition has at least one segment of data",
// so an empty table is 1, not 0.
func segmentCount(rows int) int {
	if rows <= 0 {
		return 1
	}
	return (rows + defaultSegmentRowCount - 1) / defaultSegmentRowCount
}

// distinctValues counts a column's distinct values — what
// Statistics_DistinctStates reports. Exact here, because the emulator holds
// every row rather than a compressed dictionary to estimate from.
func distinctValues(data semanticmodel.Data, table, column string) int {
	seen := map[string]struct{}{}
	for _, r := range data.Rows(table) {
		seen[fmt.Sprint(r[column])] = struct{}{}
	}
	return len(seen)
}

func id(i int) string { return strconv.Itoa(i + 1) }

// objID places an object's id in a MODEL-WIDE space. TOM assembles one object
// graph from every rowset, so per-rowset counters collide:
// `Duplicate object ID 1, first in 'Tabular.Model', another one in ...`.
// It lives here, in the shared row source, so the Discover and SQL-SELECT
// grammars cannot disagree about the same object's id — two lists answering one
// question is the defect this repo has a rule about.
func objID(kind string, i int) string { return strconv.Itoa(idBase[kind] + i + 1) }
