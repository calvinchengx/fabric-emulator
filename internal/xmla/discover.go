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
// The columns are each TOM type's SCALAR properties, read off
// Microsoft.AnalysisServices.Tabular.dll. TOM reads them BY NAME and names the
// one it is missing — `ArgumentException: Column 'Culture' does not belong to
// table Model` — so a short list is not a smaller answer, it is a refused one.
var discoverColumns = map[string][]string{
	"TMSCHEMA_MODEL": {"ID", "Name", "Description", "StorageLocation",
		"DefaultMode", "DefaultDataView", "Culture", "Collation",
		"ModifiedTime", "StructureModifiedTime", "ForceUniqueNames",
		"DiscourageImplicitMeasures", "DiscourageReportMeasures",
		"DataSourceVariablesOverrideBehavior", "DataSourceDefaultMaxConnections",
		"SourceQueryCulture", "DiscourageCompositeModels", "DisableAutoExists",
		"MaxParallelismPerRefresh", "MaxParallelismPerQuery",
		"DefaultPowerBIDataSourceVersion"},
	"TMSCHEMA_TABLES": {"ID", "ModelID", "Name", "DataCategory", "Description",
		"IsHidden", "ModifiedTime", "StructureModifiedTime",
		"ShowAsVariationsOnly", "IsPrivate", "AlternateSourcePrecedence",
		"ExcludeFromModelRefresh", "LineageTag", "SourceLineageTag",
		"SystemManaged", "ExcludeFromAutomaticAggregations"},
	"TMSCHEMA_COLUMNS": {"ID", "TableID", "ExplicitName", "SourceColumn",
		"DataCategory", "Description", "IsHidden", "State", "IsUnique", "IsKey",
		"IsNullable", "Alignment", "TableDetailPosition", "IsDefaultLabel",
		"IsDefaultImage", "SummarizeBy", "Type", "FormatString",
		"IsAvailableInMDX", "ModifiedTime", "StructureModifiedTime",
		"RefreshedTime", "KeepUniqueRows", "DisplayOrdinal", "ErrorMessage",
		"SourceProviderType", "DisplayFolder", "EncodingHint", "LineageTag",
		"SourceLineageTag", "ExplicitDataType", "IsDataTypeInferred"},
	"TMSCHEMA_PARTITIONS": {"ID", "TableID", "Name", "Description", "State",
		"Mode", "DataView", "ModifiedTime", "RefreshedTime", "ErrorMessage",
		"RetainDataTillForceCalculate", "Type"},
	"TMSCHEMA_RELATIONSHIPS": {"ID", "Name", "IsActive", "Type",
		"CrossFilteringBehavior", "JoinOnDateBehavior",
		"RelyOnReferentialIntegrity", "State", "ModifiedTime", "RefreshedTime",
		"SecurityFilteringBehavior", "FromCardinality", "ToCardinality"},
	"TMSCHEMA_MEASURES": {"ID", "TableID", "Name", "Description", "DataType",
		"Expression", "FormatString", "IsHidden", "State", "ModifiedTime",
		"StructureModifiedTime", "IsSimpleMeasure", "ErrorMessage",
		"DisplayFolder", "DataCategory", "LineageTag", "SourceLineageTag"},
	"TMSCHEMA_EXPRESSIONS": {"ID", "Name", "Expression", "Description",
		"Kind", "ModifiedTime", "StructureModifiedTime", "LineageTag"},
}

// enumDefaults are the values an unset ENUM column must carry. Zero is not a
// member of several of them — `The value '0' is unexpected for type
// 'ColumnType'` — while ModeType.Import and DataViewType.Full ARE 0, so a
// blanket "use 1" is wrong in the other direction. Read off the assembly.
var enumDefaults = map[string]string{
	"Type": "1", "State": "1", "DataType": "2", "SummarizeBy": "1",
	"Alignment": "1", "SourceType": "4", "Mode": "0", "DataView": "0",
	"ExplicitDataType": "2", "EncodingHint": "0", "DefaultMode": "0",
	"DefaultDataView": "0", "DefaultPowerBIDataSourceVersion": "0",
	"DataSourceVariablesOverrideBehavior": "0", "Kind": "0",
	"CrossFilteringBehavior": "1", "JoinOnDateBehavior": "0",
	"SecurityFilteringBehavior": "1", "FromCardinality": "2",
	"ToCardinality": "1",
}

// idBase keeps object ids UNIQUE ACROSS THE MODEL. TOM builds one object graph
// from all the rowsets, so a per-rowset counter collides:
// `Duplicate object ID 1, first in 'Tabular.Model', another one in ...`.
var idBase = map[string]int{
	"TMSCHEMA_MODEL": 0, "TMSCHEMA_TABLES": 1000, "TMSCHEMA_COLUMNS": 2000,
	"TMSCHEMA_MEASURES": 3000, "TMSCHEMA_PARTITIONS": 4000,
	"TMSCHEMA_RELATIONSHIPS": 5000, "TMSCHEMA_EXPRESSIONS": 6000,
	"TMSCHEMA_PARTITION_STORAGES": 7000, "TMSCHEMA_COLUMN_STORAGES": 8000,
	"TMSCHEMA_SEGMENT_MAP_STORAGES": 9000, "TMSCHEMA_HIERARCHIES": 10000,
}

// versionColumn is on EVERY TMSCHEMA rowset. Without it the client refuses the
// rowset outright — `ResponseFormatException: The rowset is missing a Version
// column` — and it must be typed xsd:LONG, not unsignedLong:
// `DdlUtil.GetVersionFromDataTable` does `Utils.Verify(obj is long)`, an
// ASSERTION, so an unsignedLong parses cleanly, fails the type test, and
// surfaces as a bare `TomInternalException: An internal error has occured` with
// nothing named at all.
const versionColumn = "Version"

// minimalColumns is what a type we do NOT model still has to send. Measured
// against real sempy: an empty rowset whose schema declares NO columns produces
// no DataTable, `AmoDataAdapter.AdjustTableNames` bails out on the count
// mismatch, and NOTHING gets named — so one column-less rowset breaks table
// naming for all ~35 and `Tables["Model"]` comes back null. Empty is not free.
var minimalColumns = []string{"ID", "Name"}

// xsdTypeFor is the declared type per column. The schema is the CAST contract:
// ids are unsignedLong, Version is long, everything else crosses as a string.
// clrKind is each column's CLR type, from the TOM property it mirrors. The
// schema is the CAST contract: a DateTime declared as a string fails as
// `InvalidCastException: Unable to cast System.String to System.DateTime`, and
// the value must PARSE as that type too — an empty cell in a dateTime column is
// not "unset", it is unparseable.
var clrKind = map[string]string{
	"ModifiedTime": "dateTime", "StructureModifiedTime": "dateTime",
	"RefreshedTime":    "dateTime",
	"ForceUniqueNames": "boolean", "DiscourageImplicitMeasures": "boolean",
	"DiscourageReportMeasures": "boolean", "DiscourageCompositeModels": "boolean",
	"IsHidden": "boolean", "ShowAsVariationsOnly": "boolean",
	"IsPrivate": "boolean", "ExcludeFromModelRefresh": "boolean",
	"SystemManaged": "boolean", "ExcludeFromAutomaticAggregations": "boolean",
	"IsUnique": "boolean", "IsKey": "boolean", "IsNullable": "boolean",
	"IsDefaultLabel": "boolean", "IsDefaultImage": "boolean",
	"IsAvailableInMDX": "boolean", "KeepUniqueRows": "boolean",
	"IsDataTypeInferred": "boolean", "IsSimpleMeasure": "boolean",
	"RetainDataTillForceCalculate": "boolean", "IsActive": "boolean",
	"RelyOnReferentialIntegrity":      "boolean",
	"DataSourceDefaultMaxConnections": "int", "DisableAutoExists": "int",
	"MaxParallelismPerRefresh": "int", "MaxParallelismPerQuery": "int",
	"AlternateSourcePrecedence": "int", "TableDetailPosition": "int",
	"DisplayOrdinal": "int",
}

// zeroFor is the value an unset column of each kind must carry so it parses.
var zeroFor = map[string]string{
	"dateTime": "2020-01-01T00:00:00", "boolean": "false", "int": "0",
}

func xsdTypeFor(col string) string {
	switch {
	case col == versionColumn:
		return "xsd:long"
	case col == "ID" || strings.HasSuffix(col, "ID"):
		return "xsd:unsignedLong"
	case enumDefaults[col] != "":
		// Enums cross the wire as their integer value.
		return "xsd:int"
	case clrKind[col] != "":
		return "xsd:" + clrKind[col]
	default:
		return "xsd:string"
	}
}

// tomObjectName is the <root name="..."> TOM expects, which is the singular
// object name rather than the request type. `DdlUtil.ObtainModelTable` looks up
// `dataSet.Tables["Model"]` by that name.
//
// TOTAL over the union of `discoverColumns` and `tomBatchRequestTypes`, asserted
// by TestEveryBatchTypeHasATOMObjectName. Totality is the point: the name used to
// be derived from the request string when the map missed, which put caller-
// supplied text in the response body (CodeQL go/reflected-xss) and hid the
// unmeasured names inside a string transform.
//
// The seven marked `measured` are read off Microsoft.AnalysisServices.Tabular.dll.
// The rest are the previous derivation's output, FROZEN VERBATIM so this change
// moves no bytes on the wire — they are NOT measured, and their oddities
// (`Datasources`, not `DataSources`) are that derivation's artifacts, not TOM's
// names. They are only ever the name of a zero-row rowset, which is why nothing
// has yet forced them to be right. Correct them when something measures them.
var tomObjectName = map[string]string{
	"TMSCHEMA_MODEL":                     "Model",        // measured
	"TMSCHEMA_TABLES":                    "Table",        // measured
	"TMSCHEMA_COLUMNS":                   "Column",       // measured
	"TMSCHEMA_PARTITIONS":                "Partition",    // measured
	"TMSCHEMA_RELATIONSHIPS":             "Relationship", // measured
	"TMSCHEMA_MEASURES":                  "Measure",      // measured
	"TMSCHEMA_EXPRESSIONS":               "Expression",   // measured
	"TMSCHEMA_DATA_SOURCES":              "Datasources",
	"TMSCHEMA_ATTRIBUTE_HIERARCHIES":     "Attributehierarchies",
	"TMSCHEMA_HIERARCHIES":               "Hierarchies",
	"TMSCHEMA_LEVELS":                    "Levels",
	"TMSCHEMA_ANNOTATIONS":               "Annotations",
	"TMSCHEMA_KPIS":                      "Kpis",
	"TMSCHEMA_CULTURES":                  "Cultures",
	"TMSCHEMA_OBJECT_TRANSLATIONS":       "Objecttranslations",
	"TMSCHEMA_LINGUISTIC_METADATA":       "Linguisticmetadata",
	"TMSCHEMA_PERSPECTIVES":              "Perspectives",
	"TMSCHEMA_PERSPECTIVE_TABLES":        "Perspectivetables",
	"TMSCHEMA_PERSPECTIVE_COLUMNS":       "Perspectivecolumns",
	"TMSCHEMA_PERSPECTIVE_HIERARCHIES":   "Perspectivehierarchies",
	"TMSCHEMA_PERSPECTIVE_MEASURES":      "Perspectivemeasures",
	"TMSCHEMA_ROLES":                     "Roles",
	"TMSCHEMA_ROLE_MEMBERSHIPS":          "Rolememberships",
	"TMSCHEMA_TABLE_PERMISSIONS":         "Tablepermissions",
	"TMSCHEMA_VARIATIONS":                "Variations",
	"TMSCHEMA_EXTENDED_PROPERTIES":       "Extendedproperties",
	"TMSCHEMA_COLUMN_PERMISSIONS":        "Columnpermissions",
	"TMSCHEMA_DETAIL_ROWS_DEFINITIONS":   "Detailrowsdefinitions",
	"TMSCHEMA_CALCULATION_GROUPS":        "Calculationgroups",
	"TMSCHEMA_CALCULATION_ITEMS":         "Calculationitems",
	"TMSCHEMA_ALTERNATE_OF_DEFINITIONS":  "Alternateofdefinitions",
	"TMSCHEMA_REFRESH_POLICIES":          "Refreshpolicies",
	"TMSCHEMA_FORMAT_STRING_DEFINITIONS": "Formatstringdefinitions",
	"TMSCHEMA_QUERY_GROUPS":              "Querygroups",
	"TMSCHEMA_CHANGED_PROPERTIES":        "Changedproperties",
}

// withContract stamps the wire requirements onto a rowset: the Version column,
// a type per column, and the <root> name. Applied to EVERY rowset, modelled or
// not, because the client's checks do not distinguish them.
// A request type with no entry is an ERROR rather than a derived or blank name:
// an unnamed rowset is the plausible-blank failure this package refuses
// elsewhere, and deriving one would put the caller's bytes in the response.
func withContract(rs Rowset, requestType string) (Rowset, error) {
	name, ok := tomObjectName[requestType]
	if !ok {
		return Rowset{}, fmt.Errorf("no TOM object name for %q", requestType)
	}
	rs.Columns = append(append([]string(nil), rs.Columns...), versionColumn)
	for i := range rs.Rows {
		rs.Rows[i] = append(append([]string(nil), rs.Rows[i]...), "1")
	}
	rs.Types = make([]string, len(rs.Columns))
	for i, c := range rs.Columns {
		rs.Types[i] = xsdTypeFor(c)
	}
	rs.Name = name
	return rs, nil
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
// MEASURED 2026-08-10 against real sempy (e2e/sempy): TOM does NOT accept a
// rowset whose schema declares no columns, so an unmodelled type still sends a
// minimal typed column list with zero rows. The earlier note here recorded this
// as unmeasured and is deleted rather than annotated.
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
			// Truthfully EMPTY OF ROWS — the model defines none of these
			// objects — but NOT empty of columns. A column-less rowset yields
			// no DataTable, and that breaks naming for every other rowset in
			// the batch (see minimalColumns).
			return withContract(Rowset{Columns: minimalColumns}, name)
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
			// An empty ENUM cell is not "unset", it is an INVALID MEMBER:
			// `The value '0' is unexpected for type 'ColumnType'`.
			v := src[c]
			if v == "" {
				// An unset cell must still PARSE as its declared type.
				if e := enumDefaults[c]; e != "" {
					v = e
				} else if z := zeroFor[clrKind[c]]; z != "" {
					v = z
				} else if c == versionColumn {
					v = "1"
				} else if c == "ID" || strings.HasSuffix(c, "ID") {
					v = "0"
				}
			}
			out[i] = v
		}
		rs.Rows = append(rs.Rows, out)
	}
	return withContract(rs, name)
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
		return []map[string]string{{"ID": objID("TMSCHEMA_MODEL", 0), "Name": model.Name}}, nil

	case "TMSCHEMA_MEASURES":
		var rows []map[string]string
		n := 0
		for ti, t := range model.Tables {
			for _, m := range t.Measures {
				n++
				rows = append(rows, map[string]string{
					"ID": objID("TMSCHEMA_MEASURES", n-1), "TableID": objID("TMSCHEMA_TABLES", ti),
					"Name": m.Name, "Expression": m.Expression,
				})
			}
		}
		return rows, nil

	case "TMSCHEMA_EXPRESSIONS":
		var rows []map[string]string
		i := 0
		for _, k := range sortedKeys(model.Expressions) {
			rows = append(rows, map[string]string{"ID": objID("TMSCHEMA_EXPRESSIONS", i),
				"Name": k, "Expression": model.Expressions[k]})
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
