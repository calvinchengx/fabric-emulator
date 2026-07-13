package semanticmodel

import (
	"os"
	"path/filepath"
	"testing"
)

func loadData(t *testing.T) Data {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixturesDir(), "seed_data.json"))
	if err != nil {
		t.Fatal(err)
	}
	d, err := ParseData(b)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestParseSeedData(t *testing.T) {
	d := loadData(t)
	// The _comment key is skipped; three tables load.
	if _, ok := d["_comment"]; ok {
		t.Error("_comment should be skipped, not a table")
	}
	if len(d.Rows("Store")) != 4 || len(d.Rows("Time")) != 4 || len(d.Rows("Sales")) != 8 {
		t.Fatalf("row counts: Store=%d Time=%d Sales=%d", len(d.Rows("Store")), len(d.Rows("Time")), len(d.Rows("Sales")))
	}
	if d.Rows("Store")[0]["PostalCode"] != "98052" {
		t.Errorf("Store[0].PostalCode = %v", d.Rows("Store")[0]["PostalCode"])
	}
	// Numbers decode as float64.
	if d.Rows("Store")[0]["StoreId"] != float64(1) {
		t.Errorf("StoreId type/value = %v (%T)", d.Rows("Store")[0]["StoreId"], d.Rows("Store")[0]["StoreId"])
	}

	// Sanity-check the fixture's own arithmetic: Units for MonthKey 201301 sum to 60000.
	var sum float64
	for _, r := range d.Rows("Sales") {
		if r["MonthKey"] == float64(201301) {
			sum += r["Units"].(float64)
		}
	}
	if sum != 60000 {
		t.Errorf("Σ Units for 201301 = %v, want 60000 (fixture drift)", sum)
	}
}
