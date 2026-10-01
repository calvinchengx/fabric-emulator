package onelakesec

import (
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"
)

var salesCols = map[string]FilterColumn{
	"region":  {Name: "region", Text: true},
	"amount":  {Name: "amount"},
	"price":   {Name: "price"},
	"open":    {Name: "open"},
	"shipped": {Name: "shipped"},
	"code":    {Name: "code", Text: true},
}

func row(m map[string]any) func(string) any { return func(c string) any { return m[c] } }

func admits(t *testing.T, filter string, r map[string]any) (bool, error) {
	t.Helper()
	f, err := ParseRowFilter(filter, "dbo", "sales", salesCols)
	if err != nil {
		t.Fatalf("%s: parse: %v", filter, err)
	}
	return f.Admits(row(r))
}

// Every admission is paired with a row the same filter must refuse: an
// evaluator that admitted everything would pass the first half alone.
func TestRowFilterAdmits(t *testing.T) {
	day := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		filter  string
		yes, no map[string]any
	}{
		{"SELECT * FROM sales WHERE region = 'west'", map[string]any{"region": "West  "}, map[string]any{"region": "east"}},
		{"SELECT * FROM sales WHERE region <> 'west'", map[string]any{"region": "east"}, map[string]any{"region": "WEST"}},
		{"SELECT * FROM sales WHERE region > 'b'", map[string]any{"region": "Cobalt"}, map[string]any{"region": "Apple"}},
		{"SELECT * FROM dbo.sales WHERE amount >= 10", map[string]any{"amount": int64(10)}, map[string]any{"amount": int32(9)}},
		{"SELECT * FROM sales WHERE amount < -1.5", map[string]any{"amount": float64(-2)}, map[string]any{"amount": int16(-1)}},
		{"SELECT * FROM sales WHERE amount <= 3", map[string]any{"amount": int8(3)}, map[string]any{"amount": float32(3.5)}},
		{"SELECT * FROM sales WHERE price = 1.50", map[string]any{"price": big.NewRat(3, 2)}, map[string]any{"price": big.NewRat(151, 100)}},
		{"SELECT * FROM sales WHERE amount = '7'", map[string]any{"amount": 7}, map[string]any{"amount": 8}},
		{"SELECT * FROM sales WHERE code = 42", map[string]any{"code": " 42 "}, map[string]any{"code": "41"}},
		{"SELECT * FROM sales WHERE open = TRUE", map[string]any{"open": true}, map[string]any{"open": false}},
		{"SELECT * FROM sales WHERE open = FALSE", map[string]any{"open": false}, map[string]any{"open": true}},
		{"SELECT * FROM sales WHERE shipped >= '2024-03-01'", map[string]any{"shipped": day}, map[string]any{"shipped": day.Add(-time.Second)}},
		{"SELECT * FROM sales WHERE shipped < '2024-03-01T00:00:01Z'", map[string]any{"shipped": day}, map[string]any{"shipped": day.Add(time.Second)}},
		{"SELECT * FROM sales WHERE region IN ('a', 'West')", map[string]any{"region": "west"}, map[string]any{"region": "east"}},
		{"SELECT * FROM sales WHERE region NOT IN ('a', 'b')", map[string]any{"region": "c"}, map[string]any{"region": "B"}},
		{"SELECT * FROM sales WHERE region IS NULL", map[string]any{"region": nil}, map[string]any{"region": ""}},
		{"SELECT * FROM sales WHERE region IS NOT NULL", map[string]any{"region": ""}, map[string]any{"region": nil}},
		{"SELECT * FROM sales WHERE region IS BLANK", map[string]any{"region": ""}, map[string]any{"region": "x"}},
		{"SELECT * FROM sales WHERE amount IS BLANK", map[string]any{"amount": nil}, map[string]any{"amount": 0}},
		{"SELECT * FROM sales WHERE region IS NOT BLANK", map[string]any{"region": "x"}, map[string]any{"region": nil}},
		{"SELECT * FROM sales WHERE region = 'a' OR amount = 1 AND open = TRUE", map[string]any{"region": "a"}, map[string]any{"region": "b", "amount": 1, "open": false}},
		{"SELECT * FROM sales WHERE (region = 'a' OR amount = 1) AND open = TRUE", map[string]any{"amount": 1, "open": true}, map[string]any{"region": "a", "open": false}},
		{"SELECT * FROM sales WHERE NOT region = 'a'", map[string]any{"region": "b"}, map[string]any{"region": "a"}},
		{"SELECT * FROM sales WHERE TRUE", map[string]any{}, nil},
		{"SELECT * FROM sales WHERE FALSE OR region = 'a'", map[string]any{"region": "a"}, map[string]any{"region": "b"}},
	} {
		if ok, err := admits(t, tc.filter, tc.yes); err != nil || !ok {
			t.Errorf("%s: %v admitted=%v err=%v, want admitted", tc.filter, tc.yes, ok, err)
		}
		if tc.no == nil {
			continue
		}
		if ok, err := admits(t, tc.filter, tc.no); err != nil || ok {
			t.Errorf("%s: %v admitted=%v err=%v, want refused", tc.filter, tc.no, ok, err)
		}
	}
}

// SQL's three-valued logic: a comparison with NULL is unknown, so neither it
// nor its NOT admits the row — while OR with a true side, and IS NULL, do.
func TestRowFilterThreeValuedLogic(t *testing.T) {
	null := map[string]any{"region": nil, "amount": nil}
	for filter, want := range map[string]bool{
		"SELECT * FROM sales WHERE region = 'a'":                      false,
		"SELECT * FROM sales WHERE region <> 'a'":                     false,
		"SELECT * FROM sales WHERE NOT region = 'a'":                  false,
		"SELECT * FROM sales WHERE NOT (region = 'a' AND amount = 1)": false,
		"SELECT * FROM sales WHERE region IN ('a')":                   false,
		"SELECT * FROM sales WHERE region NOT IN ('a')":               false,
		"SELECT * FROM sales WHERE region = 'a' OR TRUE":              true,
		"SELECT * FROM sales WHERE region = 'a' OR FALSE":             false,
		"SELECT * FROM sales WHERE NOT (region = 'a' OR FALSE)":       false,
		"SELECT * FROM sales WHERE region = 'a' AND FALSE":            false,
		"SELECT * FROM sales WHERE NOT (region = 'a' AND FALSE)":      true,
		"SELECT * FROM sales WHERE NOT (region = 'a' OR TRUE)":        false,
		"SELECT * FROM sales WHERE region IS NULL AND amount IS NULL": true,
	} {
		if ok, err := admits(t, filter, null); err != nil || ok != want {
			t.Errorf("%s over NULLs: admitted=%v err=%v, want %v", filter, ok, err, want)
		}
	}
}

// A value the filter cannot be compared with is an error, as SQL Server's
// conversion failure is — never a quiet answer either way.
func TestRowFilterRefusesWhatItCannotCompare(t *testing.T) {
	for filter, r := range map[string]map[string]any{
		"SELECT * FROM sales WHERE code = 42":                {"code": "forty-two"},
		"SELECT * FROM sales WHERE amount = 'many'":          {"amount": 3},
		"SELECT * FROM sales WHERE shipped = 5":              {"shipped": time.Now()},
		"SELECT * FROM sales WHERE shipped = 'next tuesday'": {"shipped": time.Now()},
		"SELECT * FROM sales WHERE amount = 1":               {"amount": []byte("x")},
		"SELECT * FROM sales WHERE price = 1":                {"price": float64Inf()},
		"SELECT * FROM sales WHERE region IN ('a', 1)":       {"region": "z"},
		"SELECT * FROM sales WHERE region = 'a' OR code = 1": {"region": "a", "code": "x"},
		"SELECT * FROM sales WHERE code = 1 OR region = 'a'": {"region": "a", "code": "x"},
		"SELECT * FROM sales WHERE NOT code = 1":             {"code": "x"},
	} {
		if ok, err := admits(t, filter, r); err == nil {
			t.Errorf("%s over %v: admitted=%v with no error", filter, r, ok)
		}
	}
}

func float64Inf() float64 { var z float64; return 1 / z }

func TestParseRowFilters(t *testing.T) {
	fs, err := ParseRowFilters("SELECT * FROM sales WHERE region = 'a' UNION SELECT * FROM sales WHERE amount = 1",
		"dbo", "sales", salesCols)
	if err != nil || len(fs) != 2 {
		t.Fatalf("%v, %v", fs, err)
	}
	if strings.Join(fs[1].Columns, ",") != "amount" {
		t.Errorf("columns = %v", fs[1].Columns)
	}
	if _, err := ParseRowFilters("SELECT * FROM sales WHERE region = 'a' UNION nonsense", "dbo", "sales", salesCols); err == nil {
		t.Error("a union with an invalid half parsed")
	}
	// A schema other than dbo is named by the caller.
	if _, err := ParseRowFilter("SELECT * FROM gold.sales WHERE amount = 1", "gold", "sales", salesCols); err != nil {
		t.Errorf("schema gold: %v", err)
	}
	_, err = ParseRowFilter("SELECT * FROM sales WHERE missing = 1", "dbo", "sales", salesCols)
	var uce *UnknownColumnError
	if !errors.As(err, &uce) {
		t.Errorf("an unknown column = %v, want UnknownColumnError", err)
	}
}

// The SQL rendering covers every node; the endpoint's own tests pin its exact
// text, so this checks only what they do not reach.
func TestRowFilterSQL(t *testing.T) {
	f, err := ParseRowFilter("SELECT * FROM sales WHERE region IS BLANK AND NOT amount IS NULL", "dbo", "sales", salesCols)
	if err != nil {
		t.Fatal(err)
	}
	got := f.SQL(func(c FilterColumn) string { return "[" + c.Name + "]" })
	want := "((([region] IS NULL OR CAST([region] AS nvarchar(max)) = N'')) AND (NOT ([amount] IS NULL)))"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// Outside Microsoft's grammar, or not matching the table, a filter is refused.
func TestParseRowFilterRefusals(t *testing.T) {
	for filter, want := range map[string]string{
		"SELECT * FROM sales WHERE " + strings.Repeat("amount = 1 OR ", 80) + "TRUE": "longer than 1000",
		"SELECT * FROM sales WHERE region = 'unterminated":                           "does not parse",
		"SELECT region FROM sales WHERE amount = 1":                                  `expects *`,
		"UPDATE sales SET amount = 1":                                                "expects SELECT",
		"SELECT * sales WHERE amount = 1":                                            "expects FROM",
		"SELECT * FROM WHERE amount = 1":                                             `written for table "WHERE"`,
		"SELECT * FROM":                                                              "ends where it expects a table name",
		"SELECT * FROM gold.sales WHERE amount = 1":                                  `names schema "gold"`,
		"SELECT * FROM dbo. WHERE amount = 1":                                        `written for table "WHERE"`,
		"SELECT * FROM dbo.":                                                         "ends where it expects a table name",
		"SELECT * FROM Sales WHERE amount = 1":                                       `written for table "Sales"`,
		"SELECT * FROM sales amount = 1":                                             "expects WHERE",
		"SELECT * FROM sales WHERE secretcol = 1":                                    `column "secretcol"`,
		"SELECT * FROM sales WHERE hr.amount = 1":                                    "multitable filters are not supported",
		"SELECT * FROM sales WHERE sales. = 1":                                       "expects a column",
		"SELECT * FROM sales WHERE 1 = amount":                                       `column "1"`,
		"SELECT * FROM sales WHERE ":                                                 "ends where it expects a column",
		"SELECT * FROM sales WHERE amount = 1.":                                      `unsupported SQL from "."`,
		"SELECT * FROM sales WHERE amount IS 1":                                      "NULL or BLANK",
		"SELECT * FROM sales WHERE amount NOT 1":                                     "expects IN",
		"SELECT * FROM sales WHERE amount NOT IN (x)":                                "a static value",
		"SELECT * FROM sales WHERE amount IN 1":                                      `expects "("`,
		"SELECT * FROM sales WHERE amount IN (1, 2":                                  `ends where it expects ")"`,
		"SELECT * FROM sales WHERE amount LIKE 1":                                    "a comparison operator",
		"SELECT * FROM sales WHERE amount = region":                                  "a static value",
		"SELECT * FROM sales WHERE amount = - x":                                     "a number",
		"SELECT * FROM sales WHERE amount = USER_NAME()":                             "a static value",
		"SELECT * FROM sales WHERE (amount = 1":                                      `ends where it expects ")"`,
		"SELECT * FROM sales WHERE (amount = x)":                                     "a static value",
		"SELECT * FROM sales WHERE NOT":                                              "ends where it expects a column",
		"SELECT * FROM sales WHERE amount = 1 AND":                                   "ends where it expects a column",
		"SELECT * FROM sales WHERE amount = 1 OR":                                    "ends where it expects a column",
		"SELECT * FROM sales WHERE amount = 1 UNION SELECT * FROM hr":                `unsupported SQL from "UNION"`,
	} {
		if _, err := ParseRowFilter(filter, "dbo", "sales", salesCols); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", filter, err, want)
		}
	}
}

// Every node renders, in the shape the endpoint's predicate functions use.
func TestRowFilterSQLRendersEveryNode(t *testing.T) {
	f, err := ParseRowFilter(`SELECT * FROM sales WHERE (region IN ('a', N'b') OR amount NOT IN (1, -2.5)) AND NOT open = TRUE AND shipped <> FALSE OR FALSE`,
		"dbo", "sales", salesCols)
	if err != nil {
		t.Fatal(err)
	}
	got := f.SQL(func(c FilterColumn) string { return c.Name })
	want := "(((((region IN (N'a', N'b')) OR (amount NOT IN (1, -2.5))) AND (NOT (open = 1))) AND (shipped <> 0)) OR (1 = 0))"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if strings.Join(f.Columns, ",") != "region,amount,open,shipped" {
		t.Errorf("columns = %v", f.Columns)
	}
}

// Quoted names, a trailing semicolon, TRUE and IS NOT NULL render as the
// endpoint's tests pin them.
func TestRowFilterSQLQuotedNamesAndConstants(t *testing.T) {
	f, err := ParseRowFilter(`SELECT * FROM [dbo].[sales] WHERE "region" IS NOT NULL OR TRUE;`, "dbo", "sales", salesCols)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := f.SQL(func(c FilterColumn) string { return c.Name }), "((NOT region IS NULL) OR (1 = 1))"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}
