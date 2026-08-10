package xmla

import (
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/semanticmodel"
)

// These pin the wire requirements MEASURED against real sempy in e2e/sempy.
// Each one failed in a way that named something other than its cause, which is
// why they are asserted here rather than left to the e2e suite: the e2e run
// needs docker and a .NET runtime, these do not.

func TestEveryDiscoverRowsetCarriesAVersionColumnTypedLong(t *testing.T) {
	// `GetVersionFromDataTable` does `Utils.Verify(obj is long)`. An
	// unsignedLong parses, fails the type test, and surfaces as a bare
	// `TomInternalException` naming nothing.
	m := &semanticmodel.Model{}
	for _, rt := range TOMBatchRequestTypes() {
		rs, err := DiscoverRowset(m, nil, rt)
		if err != nil {
			t.Fatalf("%s: %v", rt, err)
		}
		var idx = -1
		for i, c := range rs.Columns {
			if c == "Version" {
				idx = i
			}
		}
		if idx < 0 {
			t.Fatalf("%s: no Version column; the client refuses the rowset outright", rt)
		}
		if got := rs.xsdType(idx); got != "xsd:long" {
			t.Fatalf("%s: Version is %s, want xsd:long", rt, got)
		}
	}
}

func TestAnUnmodelledTypeStillDeclaresColumns(t *testing.T) {
	// A column-less rowset yields no DataTable; AdjustTableNames then bails on
	// the count mismatch and NOTHING in the batch gets named. Empty of rows is
	// correct; empty of columns breaks every other rowset.
	rs, err := DiscoverRowset(&semanticmodel.Model{}, nil, "TMSCHEMA_PERSPECTIVES")
	if err != nil {
		t.Fatal(err)
	}
	if len(rs.Columns) == 0 {
		t.Fatal("unmodelled type declared no columns")
	}
	if len(rs.Rows) != 0 {
		t.Fatalf("expected zero ROWS for an unmodelled type, got %d", len(rs.Rows))
	}
}

func TestEveryRowsetIsNamedOnTheRootElement(t *testing.T) {
	// AmoDataAdapter renames DataSet tables from <root name="...">; without it
	// `Tables["Model"]` is null and TOM reports a MODEL problem.
	m := &semanticmodel.Model{}
	for _, rt := range TOMBatchRequestTypes() {
		rs, err := DiscoverRowset(m, nil, rt)
		if err != nil {
			t.Fatalf("%s: %v", rt, err)
		}
		if rs.Name == "" {
			t.Fatalf("%s: rowset has no Name", rt)
		}
		if !strings.Contains(string(rs.DiscoverResponse()), `<root name="`) {
			t.Fatalf("%s: emitted root carries no name attribute", rt)
		}
	}
	rs, _ := DiscoverRowset(m, nil, "TMSCHEMA_MODEL")
	if rs.Name != "Model" {
		t.Fatalf("TMSCHEMA_MODEL must be named Model, got %q", rs.Name)
	}
}

func TestIdColumnsAreUnsignedLongNotString(t *testing.T) {
	// All-string gets the NAMES accepted and then fails as
	// `InvalidCastException: System.String -> System.UInt64`.
	rs, err := DiscoverRowset(&semanticmodel.Model{}, nil, "TMSCHEMA_COLUMNS")
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range rs.Columns {
		if c == "ID" || strings.HasSuffix(c, "ID") {
			if got := rs.xsdType(i); got != "xsd:unsignedLong" {
				t.Fatalf("%s is %s, want xsd:unsignedLong", c, got)
			}
		}
	}
}

func TestAnUnnamedRowsetEmitsNoNameAttribute(t *testing.T) {
	// The DAX path has a single unnamed rowset and must be unchanged by the
	// batch work — a name attribute there would be a regression, not a fix.
	rs := Rowset{Columns: []string{"a"}, Rows: [][]string{{"1"}}}
	if strings.Contains(string(rs.ExecuteResponse()), "<root name=") {
		t.Fatal("an unnamed rowset must not emit a name attribute")
	}
}
