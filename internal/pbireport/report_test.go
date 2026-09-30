package pbireport

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// readParts loads a fixture folder the way a report item's definition arrives:
// every file, keyed by its slash-separated path from the folder root.
func readParts(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	parts := map[string][]byte{}
	root := filepath.Join("testdata", dir)
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		parts[filepath.ToSlash(rel)] = b
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return parts
}

func parse(t *testing.T, dir string) *Report {
	t.Helper()
	r, err := Parse(readParts(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestPBIRPagesFollowPageOrderAndCarryTheirVisuals(t *testing.T) {
	r := parse(t, "pbir")
	if r.Format != "PBIR" {
		t.Errorf("format %q", r.Format)
	}
	var titles []string
	for _, p := range r.Pages {
		titles = append(titles, p.Title)
	}
	// pages.json lists p2 before p1; the folder order is the reverse.
	if !reflect.DeepEqual(titles, []string{"Overview", "Territories"}) {
		t.Errorf("pages %v, want pages.json's pageOrder", titles)
	}
	v := r.Pages[1].Visuals[0]
	if v.Title != "Units by territory" || v.Type != "clusteredBarChart" {
		t.Errorf("visual %q %q", v.Title, v.Type)
	}
	want := []Field{
		{Role: "Category", Kind: "Column", Table: "Store", Name: "Territory", Reference: "'Store'[Territory]"},
		{Role: "Y", Kind: "Measure", Table: "Sales", Name: "TotalUnits", Reference: "'Sales'[TotalUnits]"},
		{Role: "Y", Kind: "Aggregation", Table: "Sales", Name: "Units", Aggregation: "Max", Reference: "MAX('Sales'[Units])"},
	}
	if !reflect.DeepEqual(v.Fields, want) {
		t.Errorf("fields\n got %+v\nwant %+v", v.Fields, want)
	}
}

func TestPBIRFiltersAtEveryLevelResolveTheirSourceAlias(t *testing.T) {
	r := parse(t, "pbir")
	check := func(level string, got []Filter, want Filter) {
		t.Helper()
		if len(got) != 1 {
			t.Fatalf("%s: %d filters", level, len(got))
		}
		g := got[0]
		g.Condition = nil
		if !reflect.DeepEqual(g, want) {
			t.Errorf("%s filter\n got %+v\nwant %+v", level, g, want)
		}
	}
	// The condition names its column through the From alias `b`, not the entity.
	check("report", r.Filters, Filter{Name: "reportScenario", Type: "Categorical",
		Field: "'Budget'[Scenario]", Operator: "In", Values: []any{"Actual", "Forecast"}})
	check("page", r.Pages[1].Filters, Filter{Name: "notEast", Type: "Categorical",
		Field: "'Store'[Territory]", Operator: "NotIn", Values: []any{"East"}})
	check("visual", r.Pages[1].Visuals[0].Filters, Filter{Name: "bigSales", Type: "Advanced",
		Field: "'Sales'[TotalUnits]", Operator: ">=", Values: []any{int64(1000)}})
}

func TestPBIRReportMeasuresAndTheBoundSemanticModel(t *testing.T) {
	r := parse(t, "pbir")
	want := []Measure{{Table: "Sales", Name: "Units per Store",
		Expression: "DIVIDE([TotalUnits], DISTINCTCOUNT('Store'[StoreKey]))"}}
	if !reflect.DeepEqual(r.Measures, want) {
		t.Errorf("measures %+v", r.Measures)
	}
	if r.Dataset.SemanticModelID != "5b2c1e0a-8f0d-4e55-9a4b-7a9d2f3c1b10" {
		t.Errorf("semanticmodelid from the connection string: %+v", r.Dataset)
	}
}

func TestLegacyReportJSONParsesItsEmbeddedStrings(t *testing.T) {
	r := parse(t, "legacy")
	if r.Format != "PBIR-Legacy" || len(r.Pages) != 1 || r.Pages[0].Title != "Detail" {
		t.Fatalf("legacy report: %+v", r)
	}
	p := r.Pages[0]
	if len(p.Filters) != 1 || p.Filters[0].Field != "'Store'[Territory]" ||
		!reflect.DeepEqual(p.Filters[0].Values, []any{"West"}) {
		t.Errorf("page filter %+v", p.Filters)
	}
	v := p.Visuals[0]
	want := []Field{
		{Role: "Values", Kind: "Column", Table: "Store", Name: "Territory", Reference: "'Store'[Territory]"},
		{Role: "Values", Kind: "Aggregation", Table: "Sales", Name: "Units", Aggregation: "Sum", Reference: "SUM('Sales'[Units])"},
	}
	if v.Title != "Units table" || v.Type != "tableEx" || !reflect.DeepEqual(v.Fields, want) {
		t.Errorf("visual %q %q\n got %+v\nwant %+v", v.Title, v.Type, v.Fields, want)
	}
	if len(r.Measures) != 1 || r.Measures[0].Name != "Double Units" || r.Measures[0].Table != "Sales" {
		t.Errorf("modelExtensions measures %+v", r.Measures)
	}
	if r.Dataset.Path != "../Retail.SemanticModel" || r.Dataset.SemanticModelID != "" {
		t.Errorf("byPath reference %+v", r.Dataset)
	}
}

func TestAFilterItCannotRestateKeepsItsRawCondition(t *testing.T) {
	raw := `{"filterConfig":{"filters":[{"name":"f","type":"Advanced",
	  "field":{"Column":{"Expression":{"SourceRef":{"Entity":"Store"}},"Property":"Territory"}},
	  "filter":{"Version":2,"From":[{"Name":"s","Entity":"Store","Type":0}],
	    "Where":[{"Condition":{"Contains":{"Left":{"Column":{"Expression":{"SourceRef":{"Source":"s"}},"Property":"Territory"}},
	      "Right":{"Literal":{"Value":"'es'"}}}}}]}}]}}`
	parts := map[string][]byte{"definition/report.json": []byte(raw)}
	r, err := Parse(parts)
	if err != nil {
		t.Fatal(err)
	}
	f := r.Filters[0]
	if f.Operator != "Advanced" || f.Field != "'Store'[Territory]" || !strings.Contains(string(f.Condition), "Contains") {
		t.Errorf("an unrestated filter must say so and keep the condition: %+v", f)
	}
}

func TestLiteralsDecodeByTheirSuffix(t *testing.T) {
	for in, want := range map[string]any{
		"'O''Brien'": "O'Brien", "24L": int64(24), "2.5D": 2.5, "3.25M": 3.25,
		"true": true, "false": false, "null": nil,
		"datetime'2024-01-31T00:00:00'": "2024-01-31T00:00:00",
	} {
		if got := literal(in); !reflect.DeepEqual(got, want) {
			t.Errorf("literal(%q) = %#v, want %#v", in, got, want)
		}
	}
}

func TestParseRefusesWhatIsNotAReportDefinition(t *testing.T) {
	if _, err := Parse(map[string][]byte{"definition.pbir": []byte(`{}`)}); err == nil ||
		!strings.Contains(err.Error(), "report.json") {
		t.Errorf("no report body: %v", err)
	}
	bad := map[string][]byte{"definition/report.json": []byte(`{`)}
	if _, err := Parse(bad); err == nil || !strings.Contains(err.Error(), "definition/report.json") {
		t.Errorf("a broken part must be named: %v", err)
	}
	legacy := map[string][]byte{"report.json": []byte(`{"sections":[{"name":"s","visualContainers":[{"config":"{"}]}]}`)}
	if _, err := Parse(legacy); err == nil || !strings.Contains(err.Error(), "visual") {
		t.Errorf("a broken embedded visual config must be named: %v", err)
	}
}

func TestReportMarshalsWithTheNamesAgentsQuery(t *testing.T) {
	// The Fabric IQ skill's example JMESPath reads ReportMetadata.Pages[].Visuals[].Title,
	// .Filters and .Measures; the field names are part of the contract.
	b, err := json.Marshal(parse(t, "pbir"))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{`"Pages"`, `"Visuals"`, `"Title"`, `"Filters"`, `"Measures"`, `"Expression"`} {
		if !strings.Contains(string(b), k) {
			t.Errorf("marshalled report lacks %s", k)
		}
	}
}
