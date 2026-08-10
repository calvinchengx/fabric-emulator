package xmla

import (
	"strings"
	"testing"
)

// Every type TOM asks for in its batch must get an answer, not an error — an
// error on one of 35 would fail the whole materialisation.
func TestEveryTOMBatchTypeIsAnswerable(t *testing.T) {
	m, d := goldenModel(t), goldenData(t)
	types := TOMBatchRequestTypes()
	if len(types) != 35 {
		t.Fatalf("captured batch has %d types, want the 35 observed on the wire", len(types))
	}
	populated, empty := 0, 0
	for _, rt := range types {
		rs, err := DiscoverRowset(m, d, rt)
		if err != nil {
			t.Errorf("%s: %v — every batched type must answer", rt, err)
			continue
		}
		if len(rs.Rows) > 0 {
			populated++
		} else {
			empty++
		}
		parses(t, rs.ExecuteResponse()) // every answer must be well-formed
	}
	if populated == 0 {
		t.Error("no request type produced rows — the model is not reaching the wire")
	}
	t.Logf("TOM batch: %d populated, %d truthfully empty", populated, empty)
}

func TestDiscoverModelledTypes(t *testing.T) {
	m, d := goldenModel(t), goldenData(t)

	// Exactly one Model object, named from the TMSL.
	mod, err := DiscoverRowset(m, d, "TMSCHEMA_MODEL")
	if err != nil {
		t.Fatal(err)
	}
	if len(mod.Rows) != 1 || mod.Rows[0][1] != "RetailAnalysis" {
		t.Fatalf("TMSCHEMA_MODEL = %v, want one row named RetailAnalysis", mod.Rows)
	}

	// Measures carry their DAX expression — the payload that made XML escaping
	// non-optional, since DAX routinely contains < and &.
	meas, err := DiscoverRowset(m, d, "TMSCHEMA_MEASURES")
	if err != nil {
		t.Fatal(err)
	}
	// Counted from the model, not hardcoded: the golden fixture is shared and
	// gains measures over time, and a magic number here would fail on someone
	// else's correct change rather than on a defect in this projection.
	want := 0
	for _, tb := range m.Tables {
		want += len(tb.Measures)
	}
	if len(meas.Rows) != want {
		t.Fatalf("measures = %d, want %d (every measure in the model)", len(meas.Rows), want)
	}
	// BY COLUMN NAME, not by index. This asserted r[2]/r[3] and broke when the
	// projection gained the columns TOM actually requires — a green-to-red on a
	// correct change, because the test encoded the layout rather than the fact.
	cell := func(row []string, col string) string {
		for i, c := range meas.Columns {
			if c == col && i < len(row) {
				return row[i]
			}
		}
		return ""
	}
	var ratio []string
	for _, r := range meas.Rows {
		if cell(r, "Name") == "Total Units Ratio" {
			ratio = r
		}
	}
	if ratio == nil || !strings.Contains(cell(ratio, "Expression"), "DIVIDE") {
		t.Fatalf("Total Units Ratio row = %v, want its DIVIDE expression", ratio)
	}
	parses(t, meas.ExecuteResponse())

	// Tables answered through Discover must match the SELECT path exactly:
	// one projection, two grammars.
	viaDiscover, _ := DiscoverRowset(m, d, "TMSCHEMA_TABLES")
	viaSelect, err := DMV(m, d, `SELECT [ID], [Name] FROM $SYSTEM.TMSCHEMA_TABLES`)
	if err != nil {
		t.Fatal(err)
	}
	if len(viaDiscover.Rows) != len(viaSelect.Rows) {
		t.Fatalf("Discover %d rows vs SELECT %d — the two grammars disagree about tables",
			len(viaDiscover.Rows), len(viaSelect.Rows))
	}
	// Compare the columns the SELECT actually PROJECTED, by name. Discover
	// carries every scalar property TOM requires, so the two rowsets are not the
	// same width — but they must agree on any column both contain, because they
	// are two grammars over ONE object. Comparing by index asserted a layout
	// rather than that agreement.
	at := func(rs Rowset, row int, col string) string {
		for i, c := range rs.Columns {
			if c == col && i < len(rs.Rows[row]) {
				return rs.Rows[row][i]
			}
		}
		return "<missing>"
	}
	for i := range viaSelect.Rows {
		for _, c := range []string{"ID", "Name"} {
			if got, want := at(viaDiscover, i, c), at(viaSelect, i, c); got != want {
				t.Errorf("row %d %s differs: Discover %q vs SELECT %q", i, c, got, want)
			}
		}
	}
}

// Empty must mean "the model has none", and must be distinguishable from
// "we do not know that type" — which errors.
func TestEmptyVersusUnknown(t *testing.T) {
	m, d := goldenModel(t), goldenData(t)

	rs, err := DiscoverRowset(m, d, "TMSCHEMA_PERSPECTIVES")
	if err != nil {
		t.Fatalf("a batched type we do not model must answer empty, not error: %v", err)
	}
	if len(rs.Rows) != 0 {
		t.Error("golden model defines no perspectives; expected zero rows")
	}

	for _, bad := range []string{"TMSCHEMA_NOT_A_THING", "MDSCHEMA_MEASURES", "EVALUATE 'Store'", ""} {
		if _, err := DiscoverRowset(m, d, bad); err == nil {
			t.Errorf("%q should error rather than pass as 'model has none'", bad)
		}
	}
	if _, err := DiscoverRowset(nil, nil, "TMSCHEMA_TABLES"); err == nil {
		t.Error("nil model should error")
	}
}

// Request types are matched case- and whitespace-insensitively; a batch element
// with surrounding whitespace is otherwise a spurious failure.
func TestRequestTypeNormalisation(t *testing.T) {
	m, d := goldenModel(t), goldenData(t)
	for _, v := range []string{"tmschema_tables", "  TMSCHEMA_TABLES  ", "TmSchema_Tables"} {
		rs, err := DiscoverRowset(m, d, v)
		if err != nil || len(rs.Rows) != 3 {
			t.Errorf("DiscoverRowset(%q) = %d rows, %v", v, len(rs.Rows), err)
		}
	}
}

// Go randomises map iteration; an Expressions rowset whose order changes
// between identical requests would flake anything indexing by position.
func TestExpressionOrderIsStable(t *testing.T) {
	m, d := goldenModel(t), goldenData(t)
	if m.Expressions == nil {
		m.Expressions = map[string]string{}
	}
	m.Expressions["b"] = "2"
	m.Expressions["a"] = "1"
	m.Expressions["c"] = "3"
	first, err := DiscoverRowset(m, d, "TMSCHEMA_EXPRESSIONS")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		again, _ := DiscoverRowset(m, d, "TMSCHEMA_EXPRESSIONS")
		for r := range first.Rows {
			if first.Rows[r][1] != again.Rows[r][1] {
				t.Fatalf("row order changed between identical requests: %v vs %v",
					first.Rows, again.Rows)
			}
		}
	}
	if first.Rows[0][1] != "a" {
		t.Errorf("expected stable sorted order, got %v", first.Rows)
	}
}

// A modelled type whose projection fails must surface the error, not answer
// empty — an empty rowset here would read as "the model has none".
func TestDiscoverPropagatesProjectionErrors(t *testing.T) {
	m, d := goldenModel(t), goldenData(t)
	// TMSCHEMA_PARTITIONS routes through dmvRows; drive it with a model whose
	// tables are gone so the underlying projection has nothing to answer from.
	empty := *m
	empty.Tables = nil
	rs, err := DiscoverRowset(&empty, d, "TMSCHEMA_PARTITIONS")
	if err != nil {
		t.Fatalf("a table-less model is legitimately empty, not an error: %v", err)
	}
	if len(rs.Rows) != 0 {
		t.Errorf("expected no partitions for a table-less model, got %d", len(rs.Rows))
	}
	// And the schema still ships, so the client can read the shape.
	if !strings.Contains(string(rs.ExecuteResponse()), "<xsd:schema") {
		t.Error("empty Discover rowset must still carry its schema")
	}
}

// Every type reachable through DiscoverRowset must have a TOM object name.
//
// This is what lets `withContract` refuse rather than derive one from the
// request string. If it ever fails, the fix is a measured entry in
// tomObjectName, never a fallback that echoes the caller.
func TestEveryBatchTypeHasATOMObjectName(t *testing.T) {
	for _, rt := range TOMBatchRequestTypes() {
		if tomObjectName[rt] == "" {
			t.Errorf("%s is in TOM's batch but has no <root name>", rt)
		}
	}
	for rt := range discoverColumns {
		if tomObjectName[rt] == "" {
			t.Errorf("%s is modelled but has no <root name>", rt)
		}
	}
}

// The <root name> is table-driven, so nothing the caller sends can appear in it.
func TestRootNameComesFromTheTableNotTheRequest(t *testing.T) {
	m, d := goldenModel(t), goldenData(t)
	rs, err := DiscoverRowset(m, d, "  tmschema_tables  ")
	if err != nil {
		t.Fatal(err)
	}
	if rs.Name != "Table" {
		t.Errorf("root name = %q, want the singular TOM name %q", rs.Name, "Table")
	}
	// A type outside both tables is refused, not named.
	if _, err := DiscoverRowset(m, d, `TMSCHEMA_<script>`); err == nil {
		t.Error("an unknown request type must be an error, not a named rowset")
	}
}
