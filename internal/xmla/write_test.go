package xmla

import (
	"encoding/json"
	"strings"
	"testing"
)

// capturedWriteBatch is what semantic-link-labs 0.17.0 actually sent when
// `connect_semantic_model(readonly=False)` added one measure, trimmed to the
// structure under test. Kept verbatim in shape (namespaces, the inline schema
// before the rows, the 2014/engine Create) because every one of those is a
// thing the parser has to survive.
const capturedWriteBatch = `<Envelope xmlns="http://schemas.xmlsoap.org/soap/envelope/"><Body>
<Execute xmlns="urn:schemas-microsoft-com:xml-analysis"><Command>
<Batch Transaction="true" xmlns="http://schemas.microsoft.com/analysisservices/2003/engine">
  <Create xmlns="http://schemas.microsoft.com/analysisservices/2014/engine">
    <DatabaseID>25fdad46-8d8d-4d00-8474-145e051a8ecf</DatabaseID>
    <Measures>
      <xs:schema xmlns:xs="http://www.w3.org/2001/XMLSchema" xmlns:sql="urn:schemas-microsoft-com:xml-sql">
        <xs:complexType name="row"><xs:sequence>
          <xs:element name="TableID" type="xs:unsignedLong" sql:field="TableID" minOccurs="0"/>
          <xs:element name="Name" type="xs:string" sql:field="Name" minOccurs="0"/>
        </xs:sequence></xs:complexType>
      </xs:schema>
      <row xmlns="urn:schemas-microsoft-com:xml-analysis:rowset">
        <TableID>1003</TableID><Name>ProbeMeasure</Name>
        <Expression>SUM(Sales[Units])</Expression>
        <LineageTag>ce9558d5-b230-4281-8df8-a684e927f5a1</LineageTag>
      </row>
    </Measures>
    <Annotations>
      <xs:schema xmlns:xs="http://www.w3.org/2001/XMLSchema"/>
      <row xmlns="urn:schemas-microsoft-com:xml-analysis:rowset">
        <ObjectID>1</ObjectID><ObjectType>1</ObjectType>
        <Name>PBI_ProTooling</Name><Value>["SLL"]</Value>
      </row>
    </Annotations>
  </Create>
  <Alter ObjectExpansion="ExpandFull" xmlns="http://schemas.microsoft.com/analysisservices/2014/engine">
    <Tables>
      <xs:schema xmlns:xs="http://www.w3.org/2001/XMLSchema"/>
      <row xmlns="urn:schemas-microsoft-com:xml-analysis:rowset">
        <ID>1003</ID><LineageTag>fae6424e-b591-473d-af24-ec1f45eaca64</LineageTag>
      </row>
    </Tables>
    <Columns>
      <xs:schema xmlns:xs="http://www.w3.org/2001/XMLSchema"/>
      <row xmlns="urn:schemas-microsoft-com:xml-analysis:rowset">
        <ID>2010</ID><LineageTag>1e5e85c8-3da2-4e87-a1f8-fb9d71ae2988</LineageTag>
      </row>
    </Columns>
  </Alter>
</Batch>
</Command></Execute></Body></Envelope>`

// Mirrors e2e/semantic-model/fixtures/retail.bim in SHAPE, because the object
// ids under test are positional: columns are numbered across the whole model in
// table-then-column order, so a fixture with fewer columns silently moves every
// id and the test would assert against the wrong object.
const threeTableBim = `{
  "name": "RetailAnalysis",
  "compatibilityLevel": 1567,
  "model": {
    "culture": "en-US",
    "tables": [
      {"name": "Store", "columns": [
        {"name": "StoreId", "dataType": "int64"}, {"name": "Store", "dataType": "string"},
        {"name": "PostalCode", "dataType": "string"}, {"name": "Territory", "dataType": "string"}]},
      {"name": "Time", "columns": [
        {"name": "MonthKey", "dataType": "int64"}, {"name": "FiscalYear", "dataType": "string"},
        {"name": "FiscalMonth", "dataType": "string"}]},
      {"name": "Sales", "columns": [
        {"name": "StoreId", "dataType": "int64"}, {"name": "MonthKey", "dataType": "int64"},
        {"name": "Units", "dataType": "int64"}, {"name": "UnitsThisYear", "dataType": "int64"},
        {"name": "UnitsLastYear", "dataType": "int64"}],
       "measures": [{"name": "TotalUnits", "expression": "SUM(Sales[Units])"}]}
    ]
  }
}`

func parseBatch(t *testing.T, payload string) []WriteCommand {
	t.Helper()
	cmds, err := ParseWriteBatch(payload)
	if err != nil {
		t.Fatalf("ParseWriteBatch: %v", err)
	}
	return cmds
}

// The parser must read the captured batch, not a tidied version of it.
func TestParseWriteBatchReadsTheCapturedShape(t *testing.T) {
	cmds := parseBatch(t, capturedWriteBatch)
	if len(cmds) != 2 {
		t.Fatalf("commands = %d, want Create and Alter", len(cmds))
	}
	if cmds[0].Kind != "Create" || cmds[1].Kind != "Alter" {
		t.Fatalf("kinds = %q/%q", cmds[0].Kind, cmds[1].Kind)
	}
	if cmds[0].DatabaseID != "25fdad46-8d8d-4d00-8474-145e051a8ecf" {
		t.Errorf("DatabaseID = %q", cmds[0].DatabaseID)
	}
	if len(cmds[0].Sets) != 2 || cmds[0].Sets[0].Object != "Measures" {
		t.Fatalf("Create sets = %+v", cmds[0].Sets)
	}
	row := cmds[0].Sets[0].Rows[0]
	for k, want := range map[string]string{
		"TableID": "1003", "Name": "ProbeMeasure", "Expression": "SUM(Sales[Units])",
	} {
		if row[k] != want {
			t.Errorf("measure row %s = %q, want %q", k, row[k], want)
		}
	}
	// Exactly one row, so nothing in the inline schema was read as data. What
	// guarantees that is the `row` filter in parseWriteSet, NOT the schema
	// skip: the schema's children are <xs:complexType>/<xs:element>, so
	// descending into it yields no rows either way. Verified by mutation, and
	// recorded because an earlier version of this comment credited the skip.
	if len(cmds[0].Sets[0].Rows) != 1 {
		t.Errorf("rows = %d, want 1; something other than <row> was read as data",
			len(cmds[0].Sets[0].Rows))
	}
}

// Field names come back in the same _xHHHH_ form this package emits.
func TestParseWriteBatchDecodesEscapedFieldNames(t *testing.T) {
	payload := `<Batch><Create><Measures><row>` +
		`<Order_x0020_Details>v</Order_x0020_Details></row></Measures></Create></Batch>`
	cmds := parseBatch(t, payload)
	if got := cmds[0].Sets[0].Rows[0]["Order Details"]; got != "v" {
		t.Errorf("escaped field did not decode; row = %v", cmds[0].Sets[0].Rows[0])
	}
}

func TestParseWriteBatchRejectsRubbish(t *testing.T) {
	if _, err := ParseWriteBatch(`<Batch><Refresh/></Batch>`); err == nil {
		t.Error("a batch with no Create or Alter must be an error")
	}
	if _, err := ParseWriteBatch(`<Batch><Create><Measures>`); err == nil {
		t.Error("a truncated batch must be an error, not a partial apply")
	}
}

// The measure has to land on the table its TableID names, and the reconnect
// path reads the STORED bytes, so the assertion is on those.
func TestApplyWriteAddsTheMeasureToTheNamedTable(t *testing.T) {
	out, err := ApplyWrite([]byte(threeTableBim), parseBatch(t, capturedWriteBatch))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Model struct {
			Annotations []struct{ Name, Value string } `json:"annotations"`
			Tables      []struct {
				Name       string `json:"name"`
				LineageTag string `json:"lineageTag"`
				Columns    []struct {
					Name       string `json:"name"`
					LineageTag string `json:"lineageTag"`
				} `json:"columns"`
				Measures []struct{ Name, Expression, LineageTag string } `json:"measures"`
			} `json:"tables"`
		} `json:"model"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("result is not JSON: %v", err)
	}
	sales := doc.Model.Tables[2]
	if sales.Name != "Sales" {
		t.Fatalf("TableID 1003 resolved to %q, want Sales", sales.Name)
	}
	if len(sales.Measures) != 2 {
		t.Fatalf("Sales has %d measures, want the original plus ProbeMeasure",
			len(sales.Measures))
	}
	got := sales.Measures[1]
	if got.Name != "ProbeMeasure" || got.Expression != "SUM(Sales[Units])" {
		t.Errorf("added measure = %+v", got)
	}
	if got.LineageTag == "" {
		t.Error("the LineageTag TOM sent was dropped")
	}
	// The Alter's targets, by id.
	if sales.LineageTag != "fae6424e-b591-473d-af24-ec1f45eaca64" {
		t.Errorf("table 1003 lineageTag = %q", sales.LineageTag)
	}
	// Column 2010 is the 10th column across the model, counted table then
	// column: Store has 4, Time has 3, so it is Sales's third, Units.
	if units := doc.Model.Tables[2].Columns[2]; units.Name != "Units" ||
		units.LineageTag != "1e5e85c8-3da2-4e87-a1f8-fb9d71ae2988" {
		t.Errorf("column id 2010 resolved to %+v, want Sales.Units tagged", units)
	}
	if len(doc.Model.Annotations) != 1 || doc.Model.Annotations[0].Name != "PBI_ProTooling" {
		t.Errorf("annotations = %+v", doc.Model.Annotations)
	}
}

// Everything the emulator does not model must survive the round trip. Writing
// through the PARSED model would silently delete it.
func TestApplyWritePreservesUnmodelledContent(t *testing.T) {
	out, err := ApplyWrite([]byte(threeTableBim), parseBatch(t, capturedWriteBatch))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["compatibilityLevel"] != float64(1567) {
		t.Errorf("compatibilityLevel lost: %v", doc["compatibilityLevel"])
	}
	if doc["model"].(map[string]any)["culture"] != "en-US" {
		t.Error("model.culture was dropped by the rewrite")
	}
}

// Updating an existing measure must not duplicate it, and must not blank the
// fields the row left out.
func TestApplyWriteUpdatesRatherThanDuplicates(t *testing.T) {
	payload := `<Batch><Alter><Measures><row>` +
		`<TableID>1003</TableID><Name>TotalUnits</Name>` +
		`<Description>counts units</Description></row></Measures></Alter></Batch>`
	out, err := ApplyWrite([]byte(threeTableBim), parseBatch(t, payload))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Model struct {
			Tables []struct {
				Measures []struct{ Name, Expression, Description string } `json:"measures"`
			} `json:"tables"`
		} `json:"model"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	ms := doc.Model.Tables[2].Measures
	if len(ms) != 1 {
		t.Fatalf("measures = %d, want the one updated in place", len(ms))
	}
	if ms[0].Description != "counts units" {
		t.Errorf("description not applied: %+v", ms[0])
	}
	if ms[0].Expression != "SUM(Sales[Units])" {
		t.Errorf("a field the row omitted was blanked: %+v", ms[0])
	}
}

// A field or object type we cannot represent is refused BY NAME. Accepting it
// silently is the failure mode this whole path is exposed to: TOM reports
// SaveChanges as successful whenever the server answers at all.
func TestApplyWriteRefusesWhatItCannotRepresent(t *testing.T) {
	for _, tc := range []struct{ name, payload, want string }{
		{"unknown object", `<Batch><Create><Hierarchies><row><ID>1</ID></row>` +
			`</Hierarchies></Create></Batch>`, "Hierarchies"},
		{"unknown field", `<Batch><Alter><Tables><row><ID>1001</ID>` +
			`<RefreshPolicy>x</RefreshPolicy></row></Tables></Alter></Batch>`, "RefreshPolicy"},
		{"unknown table id", `<Batch><Create><Measures><row><TableID>9999</TableID>` +
			`<Name>n</Name></row></Measures></Create></Batch>`, "9999"},
		{"measure with no name", `<Batch><Create><Measures><row>` +
			`<TableID>1001</TableID></row></Measures></Create></Batch>`, "no Name"},
	} {
		_, err := ApplyWrite([]byte(threeTableBim), parseBatch(t, tc.payload))
		if err == nil {
			t.Errorf("%s: applied silently, want a refusal", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q does not name %q", tc.name, err, tc.want)
		}
	}
}

func TestApplyWriteRejectsAnUnusableDefinition(t *testing.T) {
	cmds := parseBatch(t, capturedWriteBatch)
	if _, err := ApplyWrite([]byte(`not json`), cmds); err == nil {
		t.Error("a non-JSON definition must be an error")
	}
	if _, err := ApplyWrite([]byte(`{"name":"x"}`), cmds); err == nil {
		t.Error("a definition with no model object must be an error")
	}
}

// DecodeName must invert EncodeName exactly, or a write can never match the
// object a read named.
func TestDecodeNameInvertsEncodeName(t *testing.T) {
	// "" is excluded deliberately: EncodeName maps it to "_x0020_" because an
	// empty element name is illegal, so the encoder is NOT injective there and
	// decoding gives back a space. That is the encoder's documented choice, not
	// a decoder bug, and asserting a round trip would assert a false property.
	for _, s := range []string{
		"TotalUnits", "Time[FiscalYear]", "[TotalUnits]", "Order Details",
		"2024", "a_b", "😀", "Ünïcodé",
		// Astral IN CONTEXT, not alone: the surrogate pair has to recombine
		// with text on both sides of it, and a decoder that mishandled the
		// second half would still pass on the bare case.
		"a\U0001F600b", "\U0010FFFF!",
	} {
		if got := DecodeName(EncodeName(s)); got != s {
			t.Errorf("DecodeName(EncodeName(%q)) = %q", s, got)
		}
	}
	// Text the encoder never produced is returned verbatim rather than mangled.
	for _, s := range []string{"_x00_", "_xZZZZ_", "plain_name"} {
		if got := DecodeName(s); got != s {
			t.Errorf("DecodeName(%q) = %q, want it unchanged", s, got)
		}
	}
}

// The remaining paths, all of them refusals or in-place updates. They are
// tested because each one is a way a write could report success and quietly do
// the wrong thing.
func TestApplyWriteEdgePaths(t *testing.T) {
	t.Run("non-numeric ids are refused", func(t *testing.T) {
		for _, payload := range []string{
			`<Batch><Create><Measures><row><TableID>abc</TableID><Name>n</Name>` +
				`</row></Measures></Create></Batch>`,
			`<Batch><Alter><Columns><row><ID>abc</ID><LineageTag>t</LineageTag>` +
				`</row></Columns></Alter></Batch>`,
		} {
			if _, err := ApplyWrite([]byte(threeTableBim), parseBatch(t, payload)); err == nil {
				t.Errorf("%s: a non-numeric id must be refused", payload)
			}
		}
	})

	t.Run("out-of-range column id is refused", func(t *testing.T) {
		payload := `<Batch><Alter><Columns><row><ID>2999</ID>` +
			`<LineageTag>t</LineageTag></row></Columns></Alter></Batch>`
		if _, err := ApplyWrite([]byte(threeTableBim), parseBatch(t, payload)); err == nil {
			t.Error("a column id past the end must be refused")
		}
	})

	t.Run("IsHidden crosses as a bool, not the string", func(t *testing.T) {
		payload := `<Batch><Create><Measures><row><TableID>1003</TableID>` +
			`<Name>Hidden</Name><IsHidden>true</IsHidden></row></Measures></Create></Batch>`
		out, err := ApplyWrite([]byte(threeTableBim), parseBatch(t, payload))
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Model struct {
				Tables []struct {
					Measures []map[string]any `json:"measures"`
				} `json:"tables"`
			} `json:"model"`
		}
		if err := json.Unmarshal(out, &doc); err != nil {
			t.Fatal(err)
		}
		got := doc.Model.Tables[2].Measures[1]["isHidden"]
		if got != true {
			t.Errorf("isHidden = %#v, want the JSON boolean true", got)
		}
	})

	t.Run("an annotation of the same name is replaced", func(t *testing.T) {
		bim := `{"model":{"annotations":[{"name":"PBI_ProTooling","value":"old"}],` +
			`"tables":[{"name":"T","columns":[]}]}}`
		payload := `<Batch><Create><Annotations><row><Name>PBI_ProTooling</Name>` +
			`<Value>new</Value></row></Annotations></Create></Batch>`
		out, err := ApplyWrite([]byte(bim), parseBatch(t, payload))
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Model struct {
				Annotations []struct{ Name, Value string } `json:"annotations"`
			} `json:"model"`
		}
		if err := json.Unmarshal(out, &doc); err != nil {
			t.Fatal(err)
		}
		if len(doc.Model.Annotations) != 1 {
			t.Fatalf("annotations = %d, want the one replaced", len(doc.Model.Annotations))
		}
		if doc.Model.Annotations[0].Value != "new" {
			t.Errorf("value = %q, want the new one", doc.Model.Annotations[0].Value)
		}
	})

	t.Run("an annotation with no name is refused", func(t *testing.T) {
		payload := `<Batch><Create><Annotations><row><Value>v</Value></row>` +
			`</Annotations></Create></Batch>`
		if _, err := ApplyWrite([]byte(threeTableBim), parseBatch(t, payload)); err == nil {
			t.Error("an unnamed annotation must be refused")
		}
	})
}

// Elements that are neither the schema nor a row are skipped rather than read.
func TestParseWriteSetSkipsUnknownChildren(t *testing.T) {
	payload := `<Batch><Create><Measures><Unexpected><row><X>1</X></row></Unexpected>` +
		`<row><TableID>1003</TableID><Name>n</Name></row></Measures></Create></Batch>`
	cmds := parseBatch(t, payload)
	rows := cmds[0].Sets[0].Rows
	if len(rows) != 1 || rows[0]["Name"] != "n" {
		t.Errorf("rows = %v; a nested <row> under an unknown element was read", rows)
	}
}

// A stored definition whose tables or columns are not objects must be refused
// rather than panicking on the type assertion.
func TestApplyWriteRefusesAMalformedDefinition(t *testing.T) {
	payload := `<Batch><Alter><Tables><row><ID>1001</ID>` +
		`<LineageTag>t</LineageTag></row></Tables></Alter></Batch>`
	if _, err := ApplyWrite([]byte(`{"model":{"tables":["not an object"]}}`),
		parseBatch(t, payload)); err == nil {
		t.Error("a table that is not an object must be refused")
	}
	// A column entry that is not an object is skipped when flattening, so the
	// id that would have addressed it no longer resolves. Either way the write
	// must not land on the wrong column.
	colPayload := `<Batch><Alter><Columns><row><ID>2001</ID>` +
		`<LineageTag>t</LineageTag></row></Columns></Alter></Batch>`
	if _, err := ApplyWrite([]byte(`{"model":{"tables":[{"name":"T","columns":["x"]}]}}`),
		parseBatch(t, colPayload)); err == nil {
		t.Error("a column that is not an object must not silently match an id")
	}
}

// An unmapped COLUMN field is refused, the same as an unmapped table field.
func TestApplyWriteRefusesAnUnknownColumnField(t *testing.T) {
	payload := `<Batch><Alter><Columns><row><ID>2001</ID>` +
		`<SummarizeBy>sum</SummarizeBy></row></Columns></Alter></Batch>`
	_, err := ApplyWrite([]byte(threeTableBim), parseBatch(t, payload))
	if err == nil || !strings.Contains(err.Error(), "SummarizeBy") {
		t.Errorf("error = %v, want a refusal naming SummarizeBy", err)
	}
}

// Malformed XML inside a row, a set, and a command each surface as an error
// rather than a half-applied batch.
func TestParseWriteBatchSurfacesTruncationAtEveryDepth(t *testing.T) {
	for _, payload := range []string{
		`<Batch><Create><Measures><row><Name>n`,
		`<Batch><Create><Measures><row><Name>n</Name></row>`,
		`<Batch><Create><DatabaseID>d`,
	} {
		if _, err := ParseWriteBatch(payload); err == nil {
			t.Errorf("%q parsed cleanly; truncation must be an error", payload)
		}
	}
}
