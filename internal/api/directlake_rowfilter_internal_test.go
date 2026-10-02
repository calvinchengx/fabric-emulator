package api

import (
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/warehouse"
)

// Delta's dates, timestamps and decimals reach the row filter as the values it
// compares, and a schema-enabled lakehouse's table is matched with its schema.
func TestFilterDirectLakeRowsOverDeltaTypes(t *testing.T) {
	day := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)
	tbl := &warehouse.Table{Columns: []string{"day", "at", "price", "note"}, Rows: [][]any{
		{warehouse.Date{T: day}, warehouse.Timestamp{T: day.Add(time.Hour)}, warehouse.Decimal{Unscaled: big.NewInt(150), Scale: 2}, "a"},
		{warehouse.Date{T: day.AddDate(0, 0, -1)}, warehouse.Timestamp{T: day.Add(-time.Hour)}, warehouse.Decimal{Unscaled: big.NewInt(151), Scale: 2}, "b"},
		{nil, nil, warehouse.Decimal{}, "c"},
	}}
	for filter, want := range map[string]string{
		"SELECT * FROM gold.sales WHERE day >= '2024-03-01'":                    "a",
		"SELECT * FROM gold.sales WHERE at > '2024-03-01T00:00:00Z'":            "a",
		"SELECT * FROM gold.sales WHERE price = 1.50":                           "a",
		"SELECT * FROM gold.sales WHERE price IS NULL":                          "c",
		"SELECT * FROM [gold].[sales] WHERE note IN ('B') OR at < '2024-03-01'": "b",
	} {
		got, err := filterDirectLakeRows(filter, "gold/sales", tbl)
		if err != nil {
			t.Errorf("%s: %v", filter, err)
			continue
		}
		var notes []string
		for _, r := range got.Rows {
			notes = append(notes, r[3].(string))
		}
		if strings.Join(notes, ",") != want {
			t.Errorf("%s = %v, want %s", filter, notes, want)
		}
	}
	// The schema is part of the match.
	if _, err := filterDirectLakeRows("SELECT * FROM dbo.sales WHERE note = 'a'", "gold/sales", tbl); err == nil {
		t.Error("a filter written for dbo applied to gold")
	}
}
