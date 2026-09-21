package server

import (
	"fmt"
	"strings"
	"testing"

	sg "github.com/calvinchengx/sqlglot-go/sqlglot"
)

// The row-filter translator is checked twice. rowfilter_test.go compares its
// output to a string this repository wrote, which proves it does what it was
// written to do. This file parses that output with a second reader — sqlglot-go's
// T-SQL parser, written and verified against sqlglot rather than against this
// translator — and holds the tree to what Microsoft says a OneLake row filter is:
// "RLS roles don't support dynamic and multitable queries", each rule a column
// compared with "a static value or set of values", joined by AND and OR.
//
// A parser is not a Fabric oracle: it says the emitted predicate is T-SQL of the
// permitted shape, not that Fabric emits this predicate. The tsql dialect stands
// in for Fabric's, whose dialect the pinned sqlglot-go release does not carry.

// predicateNodes is every node class a translated filter may contain. A
// subquery, a function call, a join or a second table is not in it.
var predicateNodes = map[string]bool{
	"And": true, "Or": true, "Not": true, "Paren": true,
	"EQ": true, "NEQ": true, "GT": true, "GTE": true, "LT": true, "LTE": true,
	"In": true, "Is": true, "Null": true,
	"Literal": true, "National": true, "Neg": true, "Column": true, "Identifier": true,
	"Collate": true, "Cast": true, "DataType": true, "DataTypeParam": true, "Var": true,
}

// predicateShape parses `expr` as the WHERE of a query over one aliased table `r`
// and returns what is wrong with it, if anything.
func predicateShape(expr string) error {
	tree, err := sg.ParseOne("SELECT 1 FROM dbo.sales AS r WHERE "+expr, "tsql")
	if err != nil {
		return fmt.Errorf("does not parse as T-SQL: %w", err)
	}
	where := tree.FindAll("Where")
	if len(where) != 1 {
		return fmt.Errorf("has %d WHERE clauses, want 1", len(where))
	}
	var bad []string
	where[0].Walk(func(n *sg.Expression) bool {
		switch {
		case n.Class == "Where":
		case !predicateNodes[n.Class]:
			bad = append(bad, n.Class)
		}
		return true
	})
	for _, c := range where[0].FindAll("Column") {
		if q, _ := c.Args["table"].(*sg.Expression); q == nil || q.Name() != "r" {
			bad = append(bad, "a column not read from the one table")
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("contains %s", strings.Join(bad, ", "))
	}
	return nil
}

func TestATranslatedRowFilterIsAStaticPredicateOverOneTable(t *testing.T) {
	for _, filter := range []string{
		"SELECT * FROM sales WHERE region = 'west'",
		"select * from dbo.sales where Amount > 50000 AND region='CA'",
		"SELECT * FROM [dbo].[sales] WHERE sales.amount >= -5.5;",
		"SELECT * FROM sales WHERE amount <> 1 OR amount <= 2 AND amount < 3",
		"SELECT * FROM sales WHERE region IN ('a', N'b') AND amount NOT IN (1)",
		"SELECT * FROM sales WHERE NOT (region = 'x' OR TRUE) AND FALSE",
		"SELECT * FROM sales WHERE [ship date] IS NULL OR region IS NOT BLANK",
		"SELECT * FROM sales WHERE open = TRUE AND open <> FALSE",
		"SELECT * FROM sales WHERE amount IS NOT NULL",
		`SELECT * FROM "sales" WHERE "region" = 'x'`,
	} {
		rf, err := translateRowFilter(filter, "sales", salesColumns)
		if err != nil {
			t.Errorf("%s: %v", filter, err)
			continue
		}
		if err := predicateShape(rf.Expr); err != nil {
			t.Errorf("%s\n  -> %s\n  %v", filter, rf.Expr, err)
		}
	}
}

// The check has to be able to fail: predicates a translator bug could emit, each
// of which must be named.
func TestThePredicateShapeCheckCatchesWhatItIsFor(t *testing.T) {
	for expr, want := range map[string]string{
		"r.[a] IN (SELECT x FROM hr)":    "Select",
		"r.[a] = USER_NAME()":            "Anonymous",
		"hr.[a] = 1":                     "a column not read from the one table",
		"r.[a] = 1; DROP TABLE dbo.t":    "does not parse",
		"EXISTS (SELECT 1 FROM hr)":      "Exists",
		"r.[a] = (SELECT MAX(x) FROM z)": "Subquery",
	} {
		err := predicateShape(expr)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want it to mention %q", expr, err, want)
		}
	}
}
