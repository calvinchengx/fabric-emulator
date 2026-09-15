package server

import (
	"strings"
	"testing"
)

var salesColumns = map[string]endpointColumn{
	"region":    {Name: "region", Type: "varchar(10)", Text: true},
	"amount":    {Name: "amount", Type: "int"},
	"open":      {Name: "open", Type: "bit"},
	"ship date": {Name: "ship date", Type: "date"},
}

func TestTranslateRowFilter(t *testing.T) {
	ci := " COLLATE " + rlsCollation
	for filter, want := range map[string]struct {
		expr string
		cols string
	}{
		"SELECT * FROM sales WHERE region = 'west'":                    {"(r.[region]" + ci + " = N'west')", "region"},
		"select * from dbo.sales where Amount > 50000 AND region='CA'": {"((r.[amount] > 50000) AND (r.[region]" + ci + " = N'CA'))", "amount,region"},
		"SELECT * FROM [dbo].[sales] WHERE sales.amount >= -5.5;":      {"(r.[amount] >= -5.5)", "amount"},
		"SELECT * FROM sales WHERE amount <> 1 OR amount <= 2 AND amount < 3": {
			"((r.[amount] <> 1) OR ((r.[amount] <= 2) AND (r.[amount] < 3)))", "amount"},
		"SELECT * FROM sales WHERE region IN ('a', N'b') AND amount NOT IN (1)": {
			"((r.[region]" + ci + " IN (N'a', N'b')) AND (r.[amount] NOT IN (1)))", "region,amount"},
		"SELECT * FROM sales WHERE NOT (region = 'x' OR TRUE) AND FALSE": {
			"((NOT ((r.[region]" + ci + " = N'x') OR (1 = 1))) AND (1 = 0))", "region"},
		"SELECT * FROM sales WHERE [ship date] IS NULL OR region IS NOT BLANK": {
			"((r.[ship date] IS NULL) OR (NOT (r.[region]" + ci + " IS NULL OR CAST(r.[region]" + ci + " AS nvarchar(max)) = N'')))", "ship date,region"},
		"SELECT * FROM sales WHERE open = TRUE AND open <> FALSE": {"((r.[open] = 1) AND (r.[open] <> 0))", "open"},
		"SELECT * FROM sales WHERE amount IS NOT NULL":            {"(NOT r.[amount] IS NULL)", "amount"},
		`SELECT * FROM "sales" WHERE "region" = 'x'`:              {"(r.[region]" + ci + " = N'x')", "region"},
	} {
		got, err := translateRowFilter(filter, "sales", salesColumns)
		if err != nil {
			t.Errorf("%s: %v", filter, err)
			continue
		}
		if got.Expr != want.expr || strings.Join(got.Columns, ",") != want.cols {
			t.Errorf("%s:\n got %s [%s]\nwant %s [%s]", filter, got.Expr, strings.Join(got.Columns, ","), want.expr, want.cols)
		}
	}
}

// Outside Microsoft's grammar, or not matching the table, a filter is refused —
// and a refused filter grants no rows.
func TestTranslateRowFilterRefusals(t *testing.T) {
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
		"SELECT * FROM sales WHERE secret = 1":                                       `column "secret"`,
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
		if _, err := translateRowFilter(filter, "sales", salesColumns); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", filter, err, want)
		}
	}
}
