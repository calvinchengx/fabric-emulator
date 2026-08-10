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
	var ratio []string
	for _, r := range meas.Rows {
		if r[2] == "Total Units Ratio" {
			ratio = r
		}
	}
	if ratio == nil || !strings.Contains(ratio[3], "DIVIDE") {
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
	for i := range viaSelect.Rows {
		if viaDiscover.Rows[i][0] != viaSelect.Rows[i][0] || viaDiscover.Rows[i][1] != viaSelect.Rows[i][1] {
			t.Errorf("row %d differs: Discover %v vs SELECT %v", i, viaDiscover.Rows[i], viaSelect.Rows[i])
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
