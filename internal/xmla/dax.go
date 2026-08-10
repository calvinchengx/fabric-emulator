package xmla

import (
	"strconv"

	"github.com/calvinchengx/fabric-emulator/internal/semanticmodel"
)

// FromDAX projects a DAX evaluation onto a Rowset.
//
// This is the path `sempy.evaluate_dax` takes: it is XMLA unconditionally —
// `_flat.py:1036` accepts no `use_xmla` parameter and its body is a bare
// `DatasetXmlaClient(...)` call — and arrives as an Execute carrying
// `<Statement>EVALUATE …</Statement>`. It does NOT go through executeQueries,
// which serves `evaluate_measure` and `read_table` instead.
func FromDAX(res *semanticmodel.Result) Rowset {
	if res == nil {
		return Rowset{}
	}
	rs := Rowset{Columns: append([]string(nil), res.Columns...)}
	for _, row := range res.Rows {
		cells := make([]string, len(rs.Columns))
		for i, c := range rs.Columns {
			cells[i] = cell(row[c])
		}
		rs.Rows = append(rs.Rows, cells)
	}
	return rs
}

// cell renders one value for the wire. Blank (DAX's empty result, e.g. DIVIDE
// by zero) is the empty string, matching how the evaluator already reports it.
// Integral floats lose their ".0" so a row count crosses as "3", not "3.0" —
// the client re-types from the schema, and "3.0" in an integer column is a
// conversion the reader should never have been asked to make.
func cell(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'g', -1, 64)
	default:
		return ""
	}
}
