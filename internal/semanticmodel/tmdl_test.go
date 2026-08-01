package semanticmodel

import (
	"testing"
)

// The same model in both serialisations. If TMDL and TMSL disagree, one of the
// two parsers is wrong, and which one is a coin toss — so the test asserts they
// agree rather than asserting either in isolation.
const equivalentTMSL = `{
  "name": "ContosoRevenue",
  "compatibilityLevel": 1550,
  "model": {
    "culture": "en-US",
    "tables": [
      {"name": "Customer", "columns": [
        {"name": "CustomerId", "dataType": "string", "sourceColumn": "CustomerId"},
        {"name": "Country", "dataType": "string", "sourceColumn": "Country"}]},
      {"name": "Revenue",
       "columns": [
         {"name": "Country", "dataType": "string", "sourceColumn": "Country"},
         {"name": "Revenue", "dataType": "double", "sourceColumn": "Revenue"},
         {"name": "Units", "dataType": "int64", "sourceColumn": "Units"}],
       "measures": [
         {"name": "Total Revenue", "expression": "SUM(Revenue[Revenue])"},
         {"name": "Revenue per Unit", "expression": "DIVIDE([Total Revenue], [Total Units])"}]}
    ],
    "relationships": [
      {"name": "Revenue_Customer", "fromTable": "Revenue", "fromColumn": "Country",
       "toTable": "Customer", "toColumn": "Country"}]
  }
}`

var equivalentTMDL = map[string][]byte{
	"definition/model.tmdl": []byte(
		"model ContosoRevenue\n\tculture: en-US\n\tcompatibilityLevel: 1550\n"),
	"definition/tables/Customer.tmdl": []byte(
		"table Customer\n" +
			"\n\tcolumn CustomerId\n\t\tdataType: string\n\t\tsourceColumn: CustomerId\n" +
			"\n\tcolumn Country\n\t\tdataType: string\n\t\tsourceColumn: Country\n"),
	"definition/tables/Revenue.tmdl": []byte(
		"table Revenue\n" +
			"\n\tcolumn Country\n\t\tdataType: string\n\t\tsourceColumn: Country\n" +
			"\n\tcolumn Revenue\n\t\tdataType: double\n\t\tsourceColumn: Revenue\n" +
			"\n\tcolumn Units\n\t\tdataType: int64\n\t\tsourceColumn: Units\n" +
			"\n\tmeasure 'Total Revenue' = SUM(Revenue[Revenue])\n" +
			"\t\tformatString: #,0.00\n" +
			"\n\tmeasure 'Revenue per Unit' = DIVIDE([Total Revenue], [Total Units])\n"),
	"definition/relationships.tmdl": []byte(
		"relationship Revenue_Customer\n" +
			"\tfromColumn: Revenue.Country\n\ttoColumn: Customer.Country\n"),
}

func TestParseTMDLMatchesTMSL(t *testing.T) {
	want, err := ParseTMSL([]byte(equivalentTMSL))
	if err != nil {
		t.Fatalf("TMSL: %v", err)
	}
	got, err := ParseTMDL(equivalentTMDL)
	if err != nil {
		t.Fatalf("TMDL: %v", err)
	}

	if got.CompatibilityLevel != want.CompatibilityLevel {
		t.Errorf("compatibilityLevel = %d, want %d",
			got.CompatibilityLevel, want.CompatibilityLevel)
	}
	if len(got.Tables) != len(want.Tables) {
		t.Fatalf("tables = %d, want %d", len(got.Tables), len(want.Tables))
	}
	for i, wt := range want.Tables {
		gt := got.Tables[i]
		if gt.Name != wt.Name {
			t.Errorf("table[%d] = %q, want %q", i, gt.Name, wt.Name)
		}
		if len(gt.Columns) != len(wt.Columns) {
			t.Fatalf("table %s: %d columns, want %d", wt.Name, len(gt.Columns), len(wt.Columns))
		}
		for j, wc := range wt.Columns {
			if gt.Columns[j] != wc {
				t.Errorf("table %s column[%d] = %+v, want %+v", wt.Name, j, gt.Columns[j], wc)
			}
		}
		if len(gt.Measures) != len(wt.Measures) {
			t.Fatalf("table %s: %d measures, want %d", wt.Name, len(gt.Measures), len(wt.Measures))
		}
		for j, wm := range wt.Measures {
			if gt.Measures[j] != wm {
				t.Errorf("table %s measure[%d] = %+v, want %+v", wt.Name, j, gt.Measures[j], wm)
			}
		}
	}
	if len(got.Relationships) != 1 {
		t.Fatalf("relationships = %d, want 1", len(got.Relationships))
	}
	if got.Relationships[0] != want.Relationships[0] {
		t.Errorf("relationship = %+v, want %+v", got.Relationships[0], want.Relationships[0])
	}
}

func TestParseTMDLQuotingAndContinuation(t *testing.T) {
	parts := map[string][]byte{
		"definition/tables/Daily.tmdl": []byte(
			"table 'Daily Revenue'\n" +
				"\n\tcolumn 'Order Date'\n\t\tdataType: string\n" +
				"\n\tmeasure 'Rolling' =\n" +
				"\t\t\tCALCULATE(\n" +
				"\t\t\t\tSUM('Daily Revenue'[Revenue])\n" +
				"\t\t\t)\n"),
		// A quoted table name on a relationship endpoint must not be split on
		// the dot inside the quotes.
		"definition/relationships.tmdl": []byte(
			"relationship R1\n" +
				"\tfromColumn: 'Daily Revenue'.'Order Date'\n" +
				"\ttoColumn: Calendar.Date\n"),
	}
	m, err := ParseTMDL(parts)
	if err != nil {
		t.Fatalf("ParseTMDL: %v", err)
	}
	if m.Tables[0].Name != "Daily Revenue" {
		t.Errorf("table name = %q, want %q", m.Tables[0].Name, "Daily Revenue")
	}
	// sourceColumn defaults to the column name when the file omits it.
	if c := m.Tables[0].Columns[0]; c.Name != "Order Date" || c.SourceColumn != "Order Date" {
		t.Errorf("column = %+v", c)
	}
	got := m.Tables[0].Measures[0]
	want := "CALCULATE( SUM('Daily Revenue'[Revenue]) )"
	if got.Name != "Rolling" || got.Expression != want {
		t.Errorf("measure = %+v, want name=Rolling expression=%q", got, want)
	}
	r := m.Relationships[0]
	if r.FromTable != "Daily Revenue" || r.FromColumn != "Order Date" ||
		r.ToTable != "Calendar" || r.ToColumn != "Date" {
		t.Errorf("relationship = %+v", r)
	}
}

func TestParseTMDLDirectLakePartition(t *testing.T) {
	parts := map[string][]byte{
		"definition/tables/Sales.tmdl": []byte(
			"table Sales\n" +
				"\n\tcolumn Amount\n\t\tdataType: double\n" +
				"\n\tpartition Sales = entity\n" +
				"\t\tmode: directLake\n" +
				"\t\tentityName: gold_sales\n" +
				"\t\tschemaName: dbo\n" +
				"\t\texpressionSource: DatabaseQuery\n"),
	}
	m, err := ParseTMDL(parts)
	if err != nil {
		t.Fatalf("ParseTMDL: %v", err)
	}
	dl := m.Tables[0].DirectLake
	if dl == nil {
		t.Fatal("DirectLake partition not parsed")
	}
	if dl.EntityName != "gold_sales" || dl.SchemaName != "dbo" ||
		dl.ExpressionSource != "DatabaseQuery" {
		t.Errorf("partition = %+v", dl)
	}
}

func TestParseTMDLIgnoresUnsupportedBlocksAndRejectsEmpty(t *testing.T) {
	// A real .pbip carries blocks the evaluator will never consult. Loading
	// must not fail over them.
	parts := map[string][]byte{
		"definition/database.tmdl": []byte("database\n\tcompatibilityLevel: 1550\n"),
		"definition/tables/T.tmdl": []byte(
			"table T\n\tcolumn C\n\t\tdataType: string\n"),
		"definition/culture.tmdl": []byte(
			"cultureInfo en-US\n\tlinguisticMetadata\n\t\tcontent: {}\n"),
	}
	m, err := ParseTMDL(parts)
	if err != nil {
		t.Fatalf("ParseTMDL: %v", err)
	}
	if len(m.Tables) != 1 || m.Tables[0].Name != "T" {
		t.Errorf("tables = %+v", m.Tables)
	}

	if _, err := ParseTMDL(map[string][]byte{"definition/model.bim": []byte("{}")}); err == nil {
		t.Error("expected an error when no .tmdl parts are present")
	}
	// A model with no tables is not a model the evaluator can serve.
	if _, err := ParseTMDL(map[string][]byte{
		"definition/model.tmdl": []byte("model M\n\tculture: en-US\n"),
	}); err == nil {
		t.Error("expected an error for a definition with no tables")
	}
}
