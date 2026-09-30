package pbireport

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFieldsBeyondColumnsAndMeasures(t *testing.T) {
	for name, tc := range map[string]struct {
		raw, want string
		ok        bool
	}{
		"hierarchy level": {`{"HierarchyLevel":{"Expression":{"Hierarchy":{"Expression":{"SourceRef":{"Entity":"Date"}},"Hierarchy":"Calendar"}},"Level":"Year"}}`, "'Date'[Year]", true},
		// An aggregate with no DAX spelling this package knows keeps the column.
		"unknown aggregate":    {`{"Aggregation":{"Function":42,"Expression":{"Column":{"Expression":{"SourceRef":{"Entity":"S"}},"Property":"U"}}}}`, "'S'[U]", true},
		"aggregate of nothing": {`{"Aggregation":{"Function":0,"Expression":{"Literal":{"Value":"1L"}}}}`, "", false},
		// A Source alias the query never declared is taken as the table name.
		"undeclared alias":     {`{"Column":{"Expression":{"SourceRef":{"Source":"Store"}},"Property":"Territory"}}`, "'Store'[Territory]", true},
		"a quote in the table": {`{"Column":{"Expression":{"SourceRef":{"Entity":"O'Neil"}},"Property":"X"}}`, "'O''Neil'[X]", true},
		"not a field":          {`{"Literal":{"Value":"1L"}}`, "", false},
		"not json":             {`{`, "", false},
	} {
		f, ok := field(json.RawMessage(tc.raw), nil)
		if ok != tc.ok || f.Reference != tc.want {
			t.Errorf("%s: %+v %v, want %q %v", name, f, ok, tc.want, tc.ok)
		}
	}
	if e := entity(json.RawMessage(`{"Literal":{"Value":"1L"}}`), nil); e != "" {
		t.Errorf("a non-SourceRef has no entity, got %q", e)
	}
}

func TestConditionsItDoesNotRestateAreAdvanced(t *testing.T) {
	col := `{"Column":{"Expression":{"SourceRef":{"Entity":"S"}},"Property":"C"}}`
	lit := `{"Literal":{"Value":"'x'"}}`
	for name, cond := range map[string]string{
		"a tuple In":              `{"In":{"Expressions":[` + col + `,` + col + `],"Values":[[` + lit + `,` + lit + `]]}}`,
		"an In over a table":      `{"In":{"Expressions":[` + col + `],"Values":[[` + lit + `,` + lit + `]]}}`,
		"an In of an expression":  `{"In":{"Expressions":[` + col + `],"Values":[[` + col + `]]}}`,
		"a negated comparison":    `{"Not":{"Expression":{"Comparison":{"ComparisonKind":0,"Left":` + col + `,"Right":` + lit + `}}}}`,
		"a comparison of columns": `{"Comparison":{"ComparisonKind":0,"Left":` + col + `,"Right":` + col + `}}`,
		"an unknown comparison":   `{"Comparison":{"ComparisonKind":9,"Left":` + col + `,"Right":` + lit + `}}`,
		"not json":                `{`,
	} {
		fs := filters([]rawFilter{{Name: "f", Filter: &filterDef{Where: []struct {
			Condition json.RawMessage `json:"Condition"`
		}{{Condition: json.RawMessage(cond)}}}}})
		if fs[0].Operator != "Advanced" || len(fs[0].Values) != 0 {
			t.Errorf("%s: %+v, want Advanced with the raw condition", name, fs[0])
		}
	}
	// Two Where clauses are not one column against values either.
	two := filters([]rawFilter{{Filter: &filterDef{Where: []struct {
		Condition json.RawMessage `json:"Condition"`
	}{{Condition: json.RawMessage(`{}`)}, {Condition: json.RawMessage(`{}`)}}}}})
	if two[0].Operator != "Advanced" {
		t.Errorf("two conditions: %+v", two[0])
	}
	// A filter with no definition at all (a card nobody set) is kept as is.
	bare := filters([]rawFilter{{Name: "empty", Type: "Categorical"}})
	if bare[0].Operator != "" || bare[0].Name != "empty" {
		t.Errorf("empty filter: %+v", bare[0])
	}
}

func TestEachBrokenPartIsNamed(t *testing.T) {
	ok := `{}`
	for part, parts := range map[string]map[string][]byte{
		"definition.pbir":                  {"definition.pbir": []byte(`{`), "definition/report.json": []byte(ok)},
		"definition/reportExtensions.json": {"definition/reportExtensions.json": []byte(`{`)},
		"definition/pages/a/page.json":     {"definition/pages/a/page.json": []byte(`{`)},
		"definition/pages/a/visuals/v/visual.json": {
			"definition/pages/a/page.json": []byte(ok), "definition/pages/a/visuals/v/visual.json": []byte(`{`)},
		"report.json":           {"report.json": []byte(`{`)},
		"report.json config":    {"report.json": []byte(`{"config":"{"}`)},
		"report.json filters":   {"report.json": []byte(`{"filters":"{"}`)},
		"section \"s\" filters": {"report.json": []byte(`{"sections":[{"name":"s","filters":"{"}]}`)},
		"visual 0 filters":      {"report.json": []byte(`{"sections":[{"name":"s","visualContainers":[{"config":"{}","filters":"{"}]}]}`)},
	} {
		if _, err := Parse(parts); err == nil || !strings.Contains(err.Error(), part) {
			t.Errorf("%s: %v", part, err)
		}
	}
}

func TestPagesNotInPageOrderFollowIt(t *testing.T) {
	r, err := Parse(map[string][]byte{
		"definition/pages/pages.json":  []byte(`{"pageOrder":["b","gone","b"]}`),
		"definition/pages/a/page.json": []byte(`{"displayName":"A"}`),
		"definition/pages/b/page.json": []byte(`{"displayName":"B"}`),
		"definition/pages/c/page.json": []byte(`{"displayName":"C"}`),
		"definition/elsewhere.json":    []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range r.Pages {
		got = append(got, p.Title)
	}
	if strings.Join(got, "") != "BAC" {
		t.Errorf("pages %v, want listed first then the rest by name", got)
	}
}

func TestTitlesAndLiteralsThatAreNotText(t *testing.T) {
	if got := title(json.RawMessage(`[{"properties":{"text":{"expr":{"Literal":{"Value":"12L"}}}}}]`)); got != "" {
		t.Errorf("a non-text title literal is not a title, got %q", got)
	}
	if got := title(json.RawMessage(`[{"properties":{}}]`)); got != "" {
		t.Errorf("no literal, got %q", got)
	}
	if got := title(json.RawMessage(`{`)); got != "" {
		t.Errorf("broken title, got %q", got)
	}
	for _, v := range []string{"abcL", "xD", "plain"} {
		if got := literal(v); got != v {
			t.Errorf("literal(%q) = %#v, want it back unchanged", v, got)
		}
	}
}

func TestAConnectionStringWithoutAModelIDNamesNoModel(t *testing.T) {
	r, err := Parse(map[string][]byte{
		"definition.pbir":        []byte(`{"datasetReference":{"byConnection":{"connectionString":"Data Source=powerbi://x;Initial Catalog=Retail"}}}`),
		"definition/report.json": []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.Dataset.SemanticModelID != "" || r.Dataset.ConnectionString == "" {
		t.Errorf("%+v", r.Dataset)
	}
}
