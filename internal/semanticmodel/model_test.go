package semanticmodel

import (
	"os"
	"path/filepath"
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
}
