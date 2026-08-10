package xmla

import (
	"bytes"
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/semanticmodel"
)

// parses asserts the payload is well-formed XML once the trailing marker byte
// is removed — the check that catches an unescaped value, which is otherwise
// invisible until a real client rejects it.
func parses(t *testing.T, payload []byte) {
	t.Helper()
	if len(payload) == 0 || payload[len(payload)-1] != payloadComplete {
		t.Fatalf("payload must end with the 0x00 completion byte")
	}
	dec := xml.NewDecoder(bytes.NewReader(payload[:len(payload)-1]))
	for {
		_, err := dec.Token()
		if err != nil {
			if err.Error() == "EOF" {
				return
			}
			t.Fatalf("payload is not well-formed XML: %v", err)
		}
	}
}

func TestEnvelopeShape(t *testing.T) {
	rs := Rowset{Columns: []string{"A", "B"}, Rows: [][]string{{"1", "x"}, {"2", "y"}}}
	got := string(rs.ExecuteResponse())
	parses(t, rs.ExecuteResponse())

	for _, want := range []string{
		`<ExecuteResponse xmlns="urn:schemas-microsoft-com:xml-analysis">`,
		`<root xmlns="urn:schemas-microsoft-com:xml-analysis:rowset"`,
		`<xsd:schema targetNamespace="urn:schemas-microsoft-com:xml-analysis:rowset"`,
		`<xsd:element sql:field="A" name="A" type="xsd:string" minOccurs="0"/>`,
		`<row><A>1</A><B>x</B></row>`,
		`<row><A>2</A><B>y</B></row>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("envelope missing %q", want)
		}
	}
	// The schema must precede the first row: the client reads the shape before
	// the data, which is why an empty root reads as "unrecognizable".
	if strings.Index(got, "<xsd:schema") > strings.Index(got, "<row>") {
		t.Error("inline XSD must come before the first row")
	}
}

// Execute and Discover differ only by wrapper; the client rejects each in its
// own words, so a drift between them would be diagnosed twice over.
func TestExecuteAndDiscoverDifferOnlyByWrapper(t *testing.T) {
	rs := Rowset{Columns: []string{"C"}, Rows: [][]string{{"v"}}}
	e := strings.ReplaceAll(string(rs.ExecuteResponse()), "ExecuteResponse", "X")
	d := strings.ReplaceAll(string(rs.DiscoverResponse()), "DiscoverResponse", "X")
	if e != d {
		t.Error("Execute and Discover payloads differ by more than the wrapper element")
	}
}

// The stub this was modelled on interpolated values raw. DAX expressions carry
// <, > and & as a matter of course, so that produces malformed XML on ordinary
// model content — not just on hostile input.
func TestValuesAndColumnsAreEscaped(t *testing.T) {
	rs := Rowset{
		Columns: []string{"Expression"},
		Rows:    [][]string{{`IF([x] < 1 && [y] > 2, "a", "b")`}},
	}
	payload := rs.ExecuteResponse()
	parses(t, payload)
	if bytes.Contains(payload, []byte(`< 1 &&`)) {
		t.Error("value was interpolated raw; XML escaping is missing")
	}
	if !bytes.Contains(payload, []byte("&lt;")) || !bytes.Contains(payload, []byte("&amp;")) {
		t.Error("expected escaped < and & in the emitted value")
	}
}

func TestEmptyAndRaggedRows(t *testing.T) {
	// No rows at all is legal (minOccurs=0) and must still carry the schema.
	empty := Rowset{Columns: []string{"A"}}
	parses(t, empty.ExecuteResponse())
	if !strings.Contains(string(empty.ExecuteResponse()), "<xsd:schema") {
		t.Error("an empty rowset must still emit its schema")
	}
	// A short row omits trailing cells rather than emitting empty elements.
	ragged := Rowset{Columns: []string{"A", "B"}, Rows: [][]string{{"1"}}}
	got := string(ragged.ExecuteResponse())
	parses(t, ragged.ExecuteResponse())
	if !strings.Contains(got, "<row><A>1</A></row>") {
		t.Errorf("short row should omit the missing cell, got %q", got)
	}
}

// FromDAX is the evaluate_dax path. Driven off the same golden model the
// executeQueries e2e uses, so both transports answer from one source of truth.
func TestFromDAXOverGoldenModel(t *testing.T) {
	fx := filepath.Join("..", "..", "e2e", "semantic-model", "fixtures")
	bim, err := os.ReadFile(filepath.Join(fx, "retail.bim"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(fx, "seed_data.json"))
	if err != nil {
		t.Fatal(err)
	}
	model, err := semanticmodel.ParseTMSL(bim)
	if err != nil {
		t.Fatal(err)
	}
	data, err := semanticmodel.ParseData(raw)
	if err != nil {
		t.Fatal(err)
	}
	res, err := semanticmodel.Evaluate(model, data,
		`EVALUATE SUMMARIZECOLUMNS('Time'[FiscalYear], 'Time'[FiscalMonth], "TotalUnits", [TotalUnits])`)
	if err != nil {
		t.Fatal(err)
	}
	rs := FromDAX(res)
	if len(rs.Rows) != 4 {
		t.Fatalf("rows = %d, want 4", len(rs.Rows))
	}
	payload := rs.ExecuteResponse()
	parses(t, payload)
	// 60000 must cross as an integer, not 60000.0 — the client re-types from
	// the schema and should never be handed a conversion it did not ask for.
	if !bytes.Contains(payload, []byte(">60000<")) {
		t.Errorf("expected an integral value to cross without a decimal tail:\n%s", payload)
	}
	if bytes.Contains(payload, []byte("60000.0")) {
		t.Error("integral float leaked its .0 onto the wire")
	}
}

func TestFromDAXNilAndBlank(t *testing.T) {
	if got := FromDAX(nil); len(got.Columns) != 0 || len(got.Rows) != 0 {
		t.Error("nil result should give an empty rowset, not panic")
	}
	// DAX blank (DIVIDE by zero) crosses as the empty string.
	res := &semanticmodel.Result{Columns: []string{"v"}, Rows: []map[string]any{{"v": nil}}}
	rs := FromDAX(res)
	if rs.Rows[0][0] != "" {
		t.Errorf("blank should render as empty, got %q", rs.Rows[0][0])
	}
	parses(t, rs.ExecuteResponse())
}

func TestCellRendering(t *testing.T) {
	// A slice of pairs, not a map: an unhashable input (the unsupported-type
	// case this is here to cover) cannot be a map key.
	cases := []struct {
		in   any
		want string
	}{
		{nil, ""}, {"s", "s"}, {true, "true"}, {false, "false"},
		{float64(3), "3"}, {2.5, "2.5"}, {-1.0, "-1"},
		{[]int{1}, ""}, // unsupported type renders empty rather than panicking
	}
	for _, c := range cases {
		if got := cell(c.in); got != c.want {
			t.Errorf("cell(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// DAX column names contain brackets, which are illegal in XML element names —
// emitting them raw makes the payload unparseable, not merely odd. XMLA's
// documented answer is the _xHHHH_ escape.
func TestEncodeName(t *testing.T) {
	cases := map[string]string{
		"TotalUnits":       "TotalUnits",
		"Time[FiscalYear]": "Time_x005B_FiscalYear_x005D_",
		"[TotalUnits]":     "_x005B_TotalUnits_x005D_",
		"Order Details":    "Order_x0020_Details",
		"2024":             "_x0032_024", // may not start with a digit
		"a_b":              "a_x005F_b",  // literal _ escaped, so decoding stays unambiguous
		"":                 "_x0020_",
	}
	for in, want := range cases {
		if got := EncodeName(in); got != want {
			t.Errorf("EncodeName(%q) = %q, want %q", in, got, want)
		}
	}
}

// The regression this whole encoding exists for: a real DAX result must
// produce well-formed XML, and the true name must survive in sql:field.
func TestDAXColumnNamesSurviveAsWellFormedXML(t *testing.T) {
	res := &semanticmodel.Result{
		Columns: []string{"Time[FiscalYear]", "[TotalUnits]"},
		Rows:    []map[string]any{{"Time[FiscalYear]": "FY2013", "[TotalUnits]": float64(60000)}},
	}
	payload := FromDAX(res).ExecuteResponse()
	parses(t, payload) // would fail outright on raw brackets
	s := string(payload)
	if !strings.Contains(s, `sql:field="Time[FiscalYear]"`) {
		t.Error("the true column name must survive verbatim in sql:field")
	}
	if !strings.Contains(s, "<Time_x005B_FiscalYear_x005D_>FY2013</Time_x005B_FiscalYear_x005D_>") {
		t.Errorf("row element should use the encoded name:\n%s", s)
	}
}

// Astral-plane runes take the surrogate-pair path: XMLA's escape is UCS-2, so
// a rune above the BMP encodes as two _xHHHH_ escapes, not one.
func TestEncodeNameAstral(t *testing.T) {
	got := EncodeName("a\U0001F600b") // U+1F600, outside the BMP
	want := "a_xD83D__xDE00_b"
	if got != want {
		t.Errorf("EncodeName(astral) = %q, want %q", got, want)
	}
	rs := Rowset{Columns: []string{"a\U0001F600b"}, Rows: [][]string{{"v"}}}
	parses(t, rs.ExecuteResponse())
}

// A DAX result declares its columns' types, because a client reads dtypes from
// the inline schema and never from the cell text.
//
// MEASURED: before this, sempy handed back every column as `string` for a table
// whose model declares int64, so arithmetic on a measure concatenated.
func TestFromDAXDeclaresColumnTypes(t *testing.T) {
	res := &semanticmodel.Result{
		Columns: []string{"n", "big", "frac", "name", "flag", "empty", "mixed"},
		Rows: []map[string]any{
			{"n": 1.0, "big": 2.0, "frac": 2.5, "name": "a", "flag": true,
				"empty": nil, "mixed": 1.0},
			{"n": 3.0, "big": 4.5, "frac": 0.5, "name": "b", "flag": false,
				"empty": nil, "mixed": "two"},
		},
	}
	rs := FromDAX(res)
	want := map[string]string{
		"n":     "xsd:long",   // every value integral
		"big":   "xsd:double", // one non-integral widens rather than untypes
		"frac":  "xsd:double", // never integral
		"name":  "xsd:string",
		"flag":  "xsd:boolean",
		"empty": "xsd:string", // all null: nothing to infer from
		"mixed": "xsd:string", // genuinely mixed kinds
	}
	for i, c := range rs.Columns {
		if got := rs.xsdType(i); got != want[c] {
			t.Errorf("column %q typed %q, want %q", c, got, want[c])
		}
	}
	// The declared type must reach the wire, not just the struct.
	payload := string(rs.ExecuteResponse())
	if !strings.Contains(payload, `type="xsd:long"`) ||
		!strings.Contains(payload, `type="xsd:boolean"`) {
		t.Error("declared types did not reach the inline schema")
	}
}

// The declared type has to agree with the bytes: `cell` renders 2.0 as "2", so
// an all-integral column declared double would be a schema the values fail.
func TestFromDAXTypesAgreeWithTheRenderedCells(t *testing.T) {
	res := &semanticmodel.Result{
		Columns: []string{"v"},
		Rows:    []map[string]any{{"v": 2.0}, {"v": 3.0}},
	}
	rs := FromDAX(res)
	if rs.xsdType(0) != "xsd:long" {
		t.Fatalf("declared %q for integral values", rs.xsdType(0))
	}
	for _, row := range rs.Rows {
		if strings.Contains(row[0], ".") {
			t.Errorf("cell %q has a fraction under an xsd:long column", row[0])
		}
	}
}

// A value kind the evaluator does not currently produce must still be DECLARED
// consistently with what `cell` renders for it.
//
// The evaluator emits nil/string/bool/float64 today, so this arm is defensive.
// It is tested rather than left unexercised because the failure it prevents is
// silent: a column declared numeric whose cells render blank is a schema the
// values do not satisfy, and the client reports a conversion error far from
// the cause.
func TestFromDAXTypesAnUnknownValueKindAsString(t *testing.T) {
	res := &semanticmodel.Result{
		Columns: []string{"odd", "absent"},
		Rows:    []map[string]any{{"odd": []int{1, 2}}},
	}
	rs := FromDAX(res)
	if got := rs.xsdType(0); got != "xsd:string" {
		t.Errorf("unknown kind declared %q, want xsd:string", got)
	}
	// A column named in Columns but missing from every row carries no evidence.
	if got := rs.xsdType(1); got != "xsd:string" {
		t.Errorf("absent column declared %q, want xsd:string", got)
	}
	if rs.Rows[0][0] != "" {
		t.Errorf("unknown kind rendered %q; the declared type says string and "+
			"the renderer must not disagree", rs.Rows[0][0])
	}
	parses(t, rs.ExecuteResponse())
}
