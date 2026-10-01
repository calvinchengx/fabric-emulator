package server

import (
	"github.com/calvinchengx/fabric-emulator/pkg/onelakesec"
)

// A OneLake security row filter, rendered as a SQL Server predicate for the
// endpoint sync (docs/60, stage 3). The grammar and its parser live in
// pkg/onelakesec, shared with Direct Lake, which evaluates the same filter over
// the rows it reads (docs/54) — so the two surfaces cannot disagree about what a
// filter admits. A refused filter grants no rows here, as Fabric's does:
// "Queries with invalid RLS syntax, or RLS syntax that doesn't match the
// underlying table, result in no rows being shown".

// unknownColumnError is the parser's failure when a filter names a column the
// table does not have — kept distinguishable, via errors.As, from every other
// refusal. The sync treats the two differently: invalid syntax is "no rows
// being shown", a filter that references "a column that no longer exists" puts
// the security sync into an error state (docs/60).
type unknownColumnError = onelakesec.UnknownColumnError

// rlsCollation is the collation OneLake RLS compares text in.
const rlsCollation = onelakesec.RLSCollation

// endpointColumn is one column of an endpoint table: its name as the engine has
// it, and its declared type, for a predicate function's parameter.
type endpointColumn struct {
	Name, Type string
	Text       bool
}

// rowFilter is a translated filter: a boolean T-SQL expression over `r.[col]`,
// and the columns it reads.
type rowFilter struct {
	Expr    string
	Columns []string // actual column names, in first-use order
}

// translateRowFilter validates a filter against its table, in dbo, and renders
// it over `r.[col]`, applying the RLS collation to text.
func translateRowFilter(filter, table string, columns map[string]endpointColumn) (rowFilter, error) {
	cols := make(map[string]onelakesec.FilterColumn, len(columns))
	for k, c := range columns {
		cols[k] = onelakesec.FilterColumn{Name: c.Name, Text: c.Text}
	}
	f, err := onelakesec.ParseRowFilter(filter, "dbo", table, cols)
	if err != nil {
		return rowFilter{}, err
	}
	return rowFilter{Expr: f.SQL(endpointRef), Columns: f.Columns}, nil
}

func endpointRef(c onelakesec.FilterColumn) string {
	ref := "r." + sqlIdent(c.Name)
	if c.Text {
		ref += " COLLATE " + rlsCollation
	}
	return ref
}
