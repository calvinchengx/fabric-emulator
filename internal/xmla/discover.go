package xmla

import (
	"fmt"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/semanticmodel"
)

// The TOM metadata path. `Microsoft.AnalysisServices.Tabular` materialises a
// Database by sending ONE Execute whose Command is a `<Batch>` of ~35
// `<Discover>` elements — every TMSCHEMA_* request type at once, each
// restricted only by `<DatabaseName>`. That is a different grammar from the SQL
// `SELECT … FROM $SYSTEM.TMSCHEMA_*` that sempy's own Python issues through
// evaluate_dax (see dmv.go); both arrive as an Execute, which is why the two
// were conflated once already.
//
// This file is the seam a batch handler calls once per <Discover>: request type
// in, rowset out. The transport — parsing the Batch, pairing responses to
// requests — belongs to e2e/xmla and is measured there against a real client.

// tomBatchRequestTypes are the request types a real TOM client was observed to
// ask for in a single batch. Kept as a list because "which types must be
// answered" is a wire fact, not a modelling choice, and a handler enumerating
// them from here cannot drift from what was captured.
var tomBatchRequestTypes = []string{
	"TMSCHEMA_MODEL", "TMSCHEMA_DATA_SOURCES", "TMSCHEMA_TABLES", "TMSCHEMA_COLUMNS",
	"TMSCHEMA_ATTRIBUTE_HIERARCHIES", "TMSCHEMA_PARTITIONS", "TMSCHEMA_RELATIONSHIPS",
	"TMSCHEMA_MEASURES", "TMSCHEMA_HIERARCHIES", "TMSCHEMA_LEVELS", "TMSCHEMA_ANNOTATIONS",
	"TMSCHEMA_KPIS", "TMSCHEMA_CULTURES", "TMSCHEMA_OBJECT_TRANSLATIONS",
	"TMSCHEMA_LINGUISTIC_METADATA", "TMSCHEMA_PERSPECTIVES", "TMSCHEMA_PERSPECTIVE_TABLES",
	"TMSCHEMA_PERSPECTIVE_COLUMNS", "TMSCHEMA_PERSPECTIVE_HIERARCHIES",
	"TMSCHEMA_PERSPECTIVE_MEASURES", "TMSCHEMA_ROLES", "TMSCHEMA_ROLE_MEMBERSHIPS",
	"TMSCHEMA_TABLE_PERMISSIONS", "TMSCHEMA_VARIATIONS", "TMSCHEMA_EXTENDED_PROPERTIES",
	"TMSCHEMA_EXPRESSIONS", "TMSCHEMA_COLUMN_PERMISSIONS", "TMSCHEMA_DETAIL_ROWS_DEFINITIONS",
	"TMSCHEMA_CALCULATION_GROUPS", "TMSCHEMA_CALCULATION_ITEMS",
	"TMSCHEMA_ALTERNATE_OF_DEFINITIONS", "TMSCHEMA_REFRESH_POLICIES",
	"TMSCHEMA_FORMAT_STRING_DEFINITIONS", "TMSCHEMA_QUERY_GROUPS",
	"TMSCHEMA_CHANGED_PROPERTIES",
}

// TOMBatchRequestTypes returns the captured batch, for a handler to iterate.
func TOMBatchRequestTypes() []string {
	return append([]string(nil), tomBatchRequestTypes...)
}

// discoverColumns are the columns emitted per request type. Unlike the sempy
// SELECT path — where the client names the columns it wants and we project
// them — a Discover names only the rowset, so the SHAPE is ours to declare.
//
// Only the types this emulator models carry columns. For the rest the model
// genuinely has nothing (our TMSL defines no perspectives, KPIs, roles,
// cultures or translations), so zero rows is the truthful answer rather than a
// placeholder — see DiscoverRowset.
var discoverColumns = map[string][]string{
	"TMSCHEMA_MODEL":         {"ID", "Name"},
	"TMSCHEMA_TABLES":        {"ID", "Name"},
	"TMSCHEMA_COLUMNS":       {"ID", "TableID", "ExplicitName"},
	"TMSCHEMA_PARTITIONS":    {"ID", "TableID", "Name"},
	"TMSCHEMA_RELATIONSHIPS": {"ID", "Name"},
	"TMSCHEMA_MEASURES":      {"ID", "TableID", "Name", "Expression"},
	"TMSCHEMA_EXPRESSIONS":   {"ID", "Name", "Expression"},
}

// DiscoverRowset answers one `<Discover RequestType=TMSCHEMA_*>` from the model.
//
// Three outcomes, deliberately distinct:
//   - a type we model      → populated rowset
//   - a type in TOM's batch we do not model → EMPTY rowset, which is the true
//     answer for these models (no perspectives, no KPIs, no roles …), not a stub
//   - anything else        → error, so an unrecognised type is never mistaken
//     for "this model has none of those"
//
// UNMEASURED, and the reason this returns an empty rowset rather than pretending
// otherwise: whether TOM accepts an empty rowset whose schema declares no
// columns. If it does not, the open question becomes "what are the columns of
// the 28 types we do not model", which is materially more expensive. e2e/xmla
// is running exactly that experiment.
func DiscoverRowset(model *semanticmodel.Model, data semanticmodel.Data, requestType string) (Rowset, error) {
	name := strings.ToUpper(strings.TrimSpace(requestType))
	if !strings.HasPrefix(name, "TMSCHEMA_") {
		return Rowset{}, fmt.Errorf("not a TMSCHEMA Discover request type: %q", requestType)
	}
	if model == nil {
		return Rowset{}, fmt.Errorf("no model")
	}

	cols, modelled := discoverColumns[name]
	if !modelled {
		if inTOMBatch(name) {
			// Truthfully empty: the model defines none of these objects.
			return Rowset{}, nil
		}
		return Rowset{}, fmt.Errorf("unknown TMSCHEMA request type %q", name)
	}

	rs := Rowset{Columns: cols}
	rows, err := discoverRows(model, data, name)
	if err != nil {
		return Rowset{}, err
	}
	for _, src := range rows {
		out := make([]string, len(cols))
		for i, c := range cols {
			out[i] = src[c]
		}
		rs.Rows = append(rs.Rows, out)
	}
	return rs, nil
}

func inTOMBatch(name string) bool {
	for _, t := range tomBatchRequestTypes {
		if t == name {
			return true
		}
	}
	return false
}

// discoverRows materialises the modelled types. TABLES/COLUMNS/PARTITIONS/
// RELATIONSHIPS reuse dmvRows so the SELECT path and the Discover path cannot
// disagree about the same objects — one projection, two grammars.
func discoverRows(model *semanticmodel.Model, data semanticmodel.Data, name string) ([]map[string]string, error) {
	switch name {
	case "TMSCHEMA_MODEL":
		// Exactly one Model object per database.
		return []map[string]string{{"ID": "1", "Name": model.Name}}, nil

	case "TMSCHEMA_MEASURES":
		var rows []map[string]string
		n := 0
		for ti, t := range model.Tables {
			for _, m := range t.Measures {
				n++
				rows = append(rows, map[string]string{
					"ID": id(n - 1), "TableID": id(ti),
					"Name": m.Name, "Expression": m.Expression,
				})
			}
		}
		return rows, nil

	case "TMSCHEMA_EXPRESSIONS":
		var rows []map[string]string
		i := 0
		for _, k := range sortedKeys(model.Expressions) {
			rows = append(rows, map[string]string{"ID": id(i), "Name": k, "Expression": model.Expressions[k]})
			i++
		}
		return rows, nil

	default:
		return dmvRows(model, data, name)
	}
}

// sortedKeys keeps Expression rows in a stable order: Go map iteration is
// randomised, and a rowset whose row order changes between identical requests
// is a flake generator for anything that diffs or indexes by position.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
