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
	rs.Types = make([]string, len(rs.Columns))
	for i, c := range rs.Columns {
		rs.Types[i] = daxColumnType(res, c)
	}
	return rs
}

// daxColumnType declares a column's xsd type from the values the evaluator
// produced.
//
// MEASURED 2026-08-10: without this every column was declared `xsd:string`, and
// `sempy.evaluate_dax` handed back a DataFrame whose dtypes were all `string`
// for a table whose model declares `int64` — so a caller summing a measure got
// string concatenation. The dtypes come from the rowset's INLINE SCHEMA, never
// from the cell text, so emitting "3" is not enough on its own.
//
// Inferred from the values rather than from a DAX type system, which this
// evaluator does not have. One divergence follows from that and is stated
// rather than discovered later: a column whose values are all integral is
// declared `xsd:long` even if DAX would have called it a double. That is
// consistent with the bytes on the wire, because `cell` already renders 2.0 as
// "2"; declaring double while emitting "2" would be the inconsistent pair.
// Mixed kinds fall back to `xsd:string`: the wire is text either way, and a
// declared type the values do not all satisfy is a conversion error at the
// client rather than a wrong label.
func daxColumnType(res *semanticmodel.Result, col string) string {
	kind, seen := "", false
	for _, row := range res.Rows {
		v, ok := row[col]
		if !ok || v == nil {
			continue // null carries no type evidence
		}
		var k string
		switch t := v.(type) {
		case string:
			k = "xsd:string"
		case bool:
			k = "xsd:boolean"
		case float64:
			k = "xsd:double"
			if t == float64(int64(t)) {
				k = "xsd:long"
			}
		default:
			k = "xsd:string"
		}
		if !seen {
			kind, seen = k, true
			continue
		}
		if kind != k {
			// long and double are the same kind at different precisions, so one
			// non-integral value widens the column rather than untyping it.
			if (kind == "xsd:long" && k == "xsd:double") ||
				(kind == "xsd:double" && k == "xsd:long") {
				kind = "xsd:double"
				continue
			}
			return "xsd:string"
		}
	}
	if !seen {
		return "xsd:string" // every value null: nothing to infer from
	}
	return kind
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
