package tsql

import "testing"

func mustParse(t *testing.T, sql string) *Statement {
	t.Helper()
	st, err := Parse(sql)
	if err != nil {
		t.Fatalf("Parse(%q): %v", sql, err)
	}
	if st == nil {
		t.Fatalf("Parse(%q) = nil, want a statement", sql)
	}
	return st
}

func TestParseNonWithStatementsAreLeftAlone(t *testing.T) {
	// Anything that is not a CTE list must parse to (nil, nil) — "nothing to
	// do" — including statements that merely mention `with`.
	for _, sql := range []string{
		"select 1",
		"select 'with a as (select 1)' as literal",
		"-- with a as (select 1)\nselect 1",
		"/* with */ select 1",
		"select * from t with (nolock)",
		"",
	} {
		st, err := Parse(sql)
		if err != nil {
			t.Fatalf("Parse(%q): %v", sql, err)
		}
		if st != nil {
			t.Fatalf("Parse(%q) returned a WITH statement: %+v", sql, st.With)
		}
	}
}

func TestParseSequentialCTEs(t *testing.T) {
	st := mustParse(t, "with a as (select 1 x), b as (select x from a) select * from b")
	if len(st.With.CTEs) != 2 {
		t.Fatalf("got %d CTEs, want 2", len(st.With.CTEs))
	}
	if st.With.CTEs[0].Name != "a" || st.With.CTEs[1].Name != "b" {
		t.Fatalf("names: %q, %q", st.With.CTEs[0].Name, st.With.CTEs[1].Name)
	}
	if st.HasNestedCTE() {
		t.Fatal("sequential CTEs must not report as nested")
	}
	if st.Tail != "select * from b" {
		t.Fatalf("tail = %q", st.Tail)
	}
	if body := st.With.CTEs[0].Body; body != "select 1 x" {
		t.Fatalf("body = %q", body)
	}
}

func TestParseNestedCTE(t *testing.T) {
	st := mustParse(t, "with o as (with i as (select 1 x) select * from i) select * from o")
	if !st.HasNestedCTE() {
		t.Fatal("nested CTE not detected")
	}
	outer := st.With.CTEs[0]
	if outer.Name != "o" || outer.Inner == nil {
		t.Fatalf("outer = %+v", outer)
	}
	if len(outer.Inner.CTEs) != 1 || outer.Inner.CTEs[0].Name != "i" {
		t.Fatalf("inner = %+v", outer.Inner)
	}
	if got := outer.Inner.CTEs[0].Body; got != "select 1 x" {
		t.Fatalf("inner body = %q", got)
	}
	if got := outer.Body; got != "select * from i" {
		t.Fatalf("outer body after inner = %q", got)
	}
}

// The exact shape dbt-fabric emits, comment header and all — the statement
// this whole milestone exists to handle (docs/29-tsql-parity.md, T6a).
func TestParseDbtWrappedTest(t *testing.T) {
	sql := `/* {"app": "dbt", "dbt_version": "1.12.0"} */
with test_main_sql as (
    with all_values as (select country as value_field from dim_customer group by country)
    select * from all_values where value_field not in ('US','GB','SG')
),
dbt_internal_test as (select * from test_main_sql)
select count(*) as failures from dbt_internal_test`

	st := mustParse(t, sql)
	if !st.HasNestedCTE() {
		t.Fatal("dbt's nested CTE not detected")
	}
	if len(st.With.CTEs) != 2 {
		t.Fatalf("got %d CTEs, want 2", len(st.With.CTEs))
	}
	if st.With.CTEs[0].Inner == nil || st.With.CTEs[0].Inner.CTEs[0].Name != "all_values" {
		t.Fatalf("inner CTE not found: %+v", st.With.CTEs[0])
	}
	if st.With.CTEs[1].Inner != nil {
		t.Fatal("dbt_internal_test is not nested")
	}
	// The leading comment must be preserved for the rewrite, not swallowed.
	if st.Leading == "" || st.Leading[0] != '/' {
		t.Fatalf("leading trivia lost: %q", st.Leading)
	}
}

// A string literal containing the whole shape of a nested CTE must not be
// parsed as one — the case a regex gets wrong.
func TestParseIgnoresCTELookalikeInsideLiteral(t *testing.T) {
	st := mustParse(t, "with a as (select 'with b as (select 1) select' as s) select * from a")
	if st.HasNestedCTE() {
		t.Fatal("a nested CTE inside a string literal was parsed as real")
	}
	if len(st.With.CTEs) != 1 {
		t.Fatalf("got %d CTEs, want 1", len(st.With.CTEs))
	}
}

func TestParseLeadingSemicolonIdiom(t *testing.T) {
	// Microsoft's own examples use `;WITH`.
	st := mustParse(t, ";with a as (select 1) select * from a")
	if len(st.With.CTEs) != 1 || st.With.CTEs[0].Name != "a" {
		t.Fatalf("got %+v", st.With)
	}
}

func TestParseColumnListAndQuotedNames(t *testing.T) {
	st := mustParse(t, `with [my cte] (a, b) as (select 1, 2) select * from [my cte]`)
	c := st.With.CTEs[0]
	if c.Name != "[my cte]" {
		t.Fatalf("name = %q", c.Name)
	}
	if c.Columns != "(a, b)" {
		t.Fatalf("columns = %q", c.Columns)
	}
	if c.Ident() != "my cte" {
		t.Fatalf("Ident() = %q", c.Ident())
	}
}

func TestParseDeeplyNested(t *testing.T) {
	st := mustParse(t, "with l1 as (with l2 as (with l3 as (select 1 x) select * from l3) select * from l2) select * from l1")
	l1 := st.With.CTEs[0]
	if l1.Inner == nil || l1.Inner.CTEs[0].Inner == nil {
		t.Fatalf("three levels not parsed: %+v", l1)
	}
	if l1.Inner.CTEs[0].Inner.CTEs[0].Name != "l3" {
		t.Fatalf("level 3 = %+v", l1.Inner.CTEs[0].Inner.CTEs[0])
	}
}

// Fabric permits the same CTE name at different nesting levels; the parser must
// surface both so the flattener (T6c) can rename rather than collide them.
func TestParseShadowedNamesAreBothVisible(t *testing.T) {
	st := mustParse(t, "with cte1 as (with cte1 as (select 1 x) select * from cte1) select * from cte1")
	outer := st.With.CTEs[0]
	if outer.Ident() != "cte1" || outer.Inner.CTEs[0].Ident() != "cte1" {
		t.Fatalf("shadowing not represented: outer=%q inner=%q",
			outer.Ident(), outer.Inner.CTEs[0].Ident())
	}
}

func TestParseMalformedIsAnError(t *testing.T) {
	for _, sql := range []string{
		"with",                       // nothing after WITH
		"with a",                     // no AS
		"with a as select 1",         // no parens
		"with a as (select 1",        // unbalanced
		"with 'lit' as (select 1) s", // a literal is not a CTE name
		"with a as (select 'unterm)", // tokenizer failure propagates
	} {
		if _, err := Parse(sql); err == nil {
			t.Fatalf("Parse(%q) = nil error, want failure", sql)
		}
	}
}

func TestIdentNormalisation(t *testing.T) {
	for in, want := range map[string]string{
		"A":        "a",
		"[My CTE]": "my cte",
		`"Q"`:      "q",
		"[a]]b]":   "a]b",
		`"x""y"`:   `x"y`,
	} {
		if got := Ident(in); got != want {
			t.Fatalf("Ident(%q) = %q, want %q", in, got, want)
		}
	}
}
