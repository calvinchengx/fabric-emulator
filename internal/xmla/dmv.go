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

// DMV parses and answers a `$SYSTEM.TMSCHEMA_*` query against the model.
func DMV(model *semanticmodel.Model, statement string) (Rowset, error) {
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
		cols = append(cols, projection{source: p[1], out: out})
	}
	if len(cols) == 0 {
		return Rowset{}, fmt.Errorf("%s: no columns selected (SELECT * is not supported)", name)
	}

	rows, err := dmvRows(model, name)
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
func dmvRows(model *semanticmodel.Model, name string) ([]map[string]string, error) {
	if model == nil {
		return nil, fmt.Errorf("no model")
	}
	var rows []map[string]string

	switch name {
	case "TMSCHEMA_TABLES":
		for i, t := range model.Tables {
			rows = append(rows, map[string]string{"ID": id(i), "Name": t.Name})
		}

	case "TMSCHEMA_COLUMNS":
		n := 0
		for ti, t := range model.Tables {
			for _, c := range t.Columns {
				n++
				rows = append(rows, map[string]string{
					"ID": id(n - 1), "TableID": id(ti),
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
				"ID": id(ti), "TableID": id(ti), "Name": t.Name + "-Partition",
			})
		}

	case "TMSCHEMA_RELATIONSHIPS":
		for i, r := range model.Relationships {
			rows = append(rows, map[string]string{"ID": id(i), "Name": r.Name})
		}

	case "TMSCHEMA_HIERARCHIES":
		// None: the model format we parse carries no hierarchies, so this is
		// legitimately empty rather than unimplemented. An empty rowset still
		// carries its schema, which is what the client reads first.

	default:
		if strings.HasSuffix(name, "_STORAGES") || name == "TMSCHEMA_DELTA_TABLE_METADATA_STORAGES" {
			return nil, fmt.Errorf("%s reports VertiPaq physical storage (segment counts, "+
				"distinct-state counts) that this emulator does not have; refusing rather "+
				"than returning invented statistics", name)
		}
		return nil, fmt.Errorf("unsupported DMV %s", name)
	}
	return rows, nil
}

func id(i int) string { return strconv.Itoa(i + 1) }
