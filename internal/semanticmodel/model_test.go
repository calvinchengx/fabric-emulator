package semanticmodel

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixturesDir is the single source of truth for the golden model + oracle — the
// same files the e2e uses (Go tests run with the package dir as cwd).
func fixturesDir() string {
	return filepath.Join("..", "..", "e2e", "semantic-model", "fixtures")
}

func loadModel(t *testing.T) *Model {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixturesDir(), "retail.bim"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := ParseTMSL(b)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestParseRetailModel(t *testing.T) {
	m := loadModel(t)
	if m.Name != "RetailAnalysis" {
		t.Fatalf("model name = %q", m.Name)
	}
	if len(m.Tables) != 3 {
		t.Fatalf("tables = %d, want 3", len(m.Tables))
	}

	// Store columns.
	store := m.Table("Store")
	if store == nil || len(store.Columns) != 4 {
		t.Fatalf("Store = %+v", store)
	}
	if c := store.Column("PostalCode"); c == nil || c.DataType != "string" {
		t.Fatalf("Store.PostalCode = %+v", c)
	}

	// Sales measures — resolvable model-wide by name.
	for _, name := range []string{"TotalUnits", "Total Units This Year", "Total Units Last Year", "Total Units Ratio"} {
		if m.Measure(name) == nil {
			t.Errorf("measure %q not found", name)
		}
	}
	if got := m.Measure("TotalUnits").Expression; got != "SUM(Sales[Units])" {
		t.Errorf("TotalUnits expr = %q", got)
	}
	if got := m.Measure("Total Units Ratio").Expression; got != "DIVIDE([Total Units This Year], [Total Units Last Year])" {
		t.Errorf("ratio expr = %q", got)
	}

	// Quoted table name resolves, and single-quotes are tolerated.
	if m.Table("'Time'") == nil {
		t.Error("quoted 'Time' should resolve")
	}

	// Relationships in both directions.
	if len(m.Relationships) != 2 {
		t.Fatalf("relationships = %d, want 2", len(m.Relationships))
	}
	if m.RelationshipBetween("Sales", "Time") == nil || m.RelationshipBetween("Time", "Sales") == nil {
		t.Error("Sales<->Time relationship should resolve either direction")
	}
	if r := m.RelationshipBetween("Sales", "Store"); r == nil || r.FromColumn != "StoreId" || r.ToColumn != "StoreId" {
		t.Errorf("Sales->Store relationship = %+v", r)
	}
}

func TestParseTMSLErrors(t *testing.T) {
	if _, err := ParseTMSL([]byte("{not json")); err == nil {
		t.Error("expected parse error")
	}
	if _, err := ParseTMSL([]byte(`{"name":"x","model":{"tables":[]}}`)); err == nil {
		t.Error("expected 'no tables' error")
	}
	for name, raw := range map[string]string{
		"old compatibility":   `{"compatibilityLevel":1603,"model":{"tables":[{"name":"T","partitions":[{"mode":"directLake","source":{"type":"entity","entityName":"t","expressionSource":"DL"}}]}]}}`,
		"bad entity source":   `{"compatibilityLevel":1604,"model":{"tables":[{"name":"T","partitions":[{"mode":"directLake","source":{"type":"m"}}]}]}}`,
		"multiple partitions": `{"compatibilityLevel":1604,"model":{"tables":[{"name":"T","partitions":[{"mode":"directLake","source":{"type":"entity","entityName":"t","expressionSource":"DL"}},{"mode":"directLake","source":{"type":"entity","entityName":"t","expressionSource":"DL"}}]}]}}`,
		"bad expression":      `{"compatibilityLevel":1604,"model":{"expressions":[{"name":"DL","expression":42}],"tables":[{"name":"T"}]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseTMSL([]byte(raw)); err == nil {
				t.Fatal("expected parse error")
			}
		})
	}
}

func TestParseDirectLakeModel(t *testing.T) {
	raw := []byte(`{
  "name":"Direct","compatibilityLevel":1604,
  "model":{
    "expressions":[{"name":"DL","expression":["let"," Source = AzureStorage.DataLake(\"https://onelake.dfs.fabric.microsoft.com/ws/lake\")","in Source"]}],
    "tables":[{"name":"Sales","columns":[{"name":"Amount","dataType":"int64","sourceColumn":"amount"}],
      "partitions":[{"name":"Sales","mode":"directLake","source":{"type":"entity","entityName":"sales_delta","schemaName":"dbo","expressionSource":"DL"}}]}]
  }
}`)
	m, err := ParseTMSL(raw)
	if err != nil {
		t.Fatal(err)
	}
	table := m.Table("Sales")
	if m.CompatibilityLevel != 1604 || !strings.Contains(m.Expressions["DL"], "AzureStorage.DataLake") {
		t.Fatalf("model=%+v", m)
	}
	if table == nil || table.DirectLake == nil || table.DirectLake.EntityName != "sales_delta" || table.DirectLake.ExpressionSource != "DL" {
		t.Fatalf("table=%+v", table)
	}
	if table.Columns[0].SourceColumn != "amount" {
		t.Fatalf("column=%+v", table.Columns[0])
	}
}
