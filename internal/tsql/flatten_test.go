package tsql

import (
	"errors"
	"strings"
	"testing"
)

func mustFlatten(t *testing.T, sql string) string {
	t.Helper()
	out, changed, err := Flatten(sql)
	if err != nil {
		t.Fatalf("Flatten(%q): %v", sql, err)
	}
	if !changed {
		t.Fatalf("Flatten(%q) reported no change", sql)
	}
	return out
}

func TestFlattenSimpleNesting(t *testing.T) {
	got := mustFlatten(t, "with o as (with i as (select 1 x) select * from i) select * from o")
	want := "with i as (select 1 x), o as (select * from i) select * from o"
	if got != want {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}
}

// Nothing to do: the statement must come back byte-identical, so a relay can
// forward the original without re-encoding.
func TestFlattenLeavesUnaffectedStatementsUntouched(t *testing.T) {
	for _, sql := range []string{
		"select 1",
		"with a as (select 1 x), b as (select x from a) select * from b",
		"select 'with x as (with y as (select 1) select 1)' as literal",
		"/* {\"app\": \"dbt\"} */ select 1",
		"",
	} {
		out, changed, err := Flatten(sql)
		if err != nil {
			t.Fatalf("Flatten(%q): %v", sql, err)
		}
		if changed {
			t.Fatalf("Flatten(%q) rewrote a statement with no nesting: %q", sql, out)
		}
		if out != sql {
			t.Fatalf("Flatten(%q) altered bytes: %q", sql, out)
		}
	}
}

// The statement this milestone exists for: dbt's wrapped accepted_values test.
func TestFlattenDbtWrappedTest(t *testing.T) {
	sql := `/* {"app": "dbt", "dbt_version": "1.12.0"} */
with test_main_sql as (
    with all_values as (select country as value_field from dim_customer group by country)
    select * from all_values where value_field not in ('US','GB','SG')
),
dbt_internal_test as (select * from test_main_sql)
select count(*) as failures from dbt_internal_test`

	got := mustFlatten(t, sql)

	// dbt's comment header survives, and it still leads the statement.
	if !strings.HasPrefix(got, `/* {"app": "dbt", "dbt_version": "1.12.0"} */`) {
		t.Fatalf("dbt comment header lost:\n%s", got)
	}
	// The inner CTE is hoisted ahead of the parent that consumes it.
	iAll := strings.Index(got, "all_values as (")
	iMain := strings.Index(got, "test_main_sql as (")
	iInternal := strings.Index(got, "dbt_internal_test as (")
	if iAll < 0 || iMain < 0 || iInternal < 0 {
		t.Fatalf("a CTE went missing:\n%s", got)
	}
	if !(iAll < iMain && iMain < iInternal) {
		t.Fatalf("order wrong (want all_values < test_main_sql < dbt_internal_test):\n%s", got)
	}
	// And no nesting remains: exactly one WITH keyword survives.
	if n := strings.Count(strings.ToLower(got), "with "); n != 1 {
		t.Fatalf("expected a single WITH, found %d:\n%s", n, got)
	}
	// Flattening must be a fixed point.
	if _, changed, err := Flatten(got); err != nil || changed {
		t.Fatalf("re-flattening changed=%v err=%v", changed, err)
	}
}

func TestFlattenDeepNestingInnermostFirst(t *testing.T) {
	got := mustFlatten(t,
		"with l1 as (with l2 as (with l3 as (select 1 x) select * from l3) select * from l2) select * from l1")
	want := "with l3 as (select 1 x), l2 as (select * from l3), l1 as (select * from l2) select * from l1"
	if got != want {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}
}

// An inner CTE may reference an earlier sibling of its parent, so sibling order
// has to be preserved as levels collapse.
func TestFlattenPreservesSiblingOrder(t *testing.T) {
	got := mustFlatten(t,
		"with a as (select 1 x), b as (with c as (select * from a) select * from c) select * from b")
	want := "with a as (select 1 x), c as (select * from a), b as (select * from c) select * from b"
	if got != want {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}
	if strings.Index(got, "a as (") > strings.Index(got, "c as (") {
		t.Fatal("inner CTE hoisted above the sibling it references")
	}
}

// The correctness trap: Fabric keeps these two `cte1`s distinct by nesting
// level. Flattening would collide them, so the statement must be refused —
// never silently rewritten into something that returns different rows.
func TestFlattenRefusesShadowedNames(t *testing.T) {
	sql := "with cte1 as (with cte1 as (select 1 x) select * from cte1) select * from cte1"
	out, changed, err := Flatten(sql)
	var shadow *ShadowedNameError
	if !errors.As(err, &shadow) {
		t.Fatalf("want ShadowedNameError, got %v", err)
	}
	if shadow.Name != "cte1" {
		t.Fatalf("error names %q", shadow.Name)
	}
	if changed || out != sql {
		t.Fatalf("refused statement must be returned untouched: changed=%v out=%q", changed, out)
	}
	if !strings.Contains(shadow.Error(), "Rename the inner CTE") {
		t.Fatalf("error is not actionable: %s", shadow.Error())
	}
}

// Shadowing is detected across quoting and case, since those name the same CTE.
func TestFlattenRefusesShadowingAcrossQuotingAndCase(t *testing.T) {
	for _, sql := range []string{
		"with A as (with [a] as (select 1 x) select * from [a]) select * from A",
		`with "b" as (with B as (select 1 x) select * from B) select * from "b"`,
	} {
		if _, _, err := Flatten(sql); err == nil {
			t.Fatalf("Flatten(%q) accepted a shadowed statement", sql)
		}
	}
}

// Distinct names at different levels are fine — only collisions are refused.
func TestFlattenAllowsDistinctNamesAcrossLevels(t *testing.T) {
	got := mustFlatten(t, "with outer1 as (with inner1 as (select 1 x) select * from inner1) select * from outer1")
	if !strings.HasPrefix(got, "with inner1 as (select 1 x), outer1 as (") {
		t.Fatalf("got %q", got)
	}
}

func TestFlattenPreservesColumnListsAndQuotedNames(t *testing.T) {
	got := mustFlatten(t,
		`with [my cte] (a, b) as (with i as (select 1, 2) select * from i) select * from [my cte]`)
	if !strings.Contains(got, "[my cte] (a, b) as (") {
		t.Fatalf("column list or quoted name lost: %q", got)
	}
	if !strings.HasPrefix(got, "with i as (select 1, 2), ") {
		t.Fatalf("inner not hoisted first: %q", got)
	}
}

func TestFlattenPreservesLeadingSemicolon(t *testing.T) {
	got := mustFlatten(t, ";with o as (with i as (select 1 x) select * from i) select * from o")
	if !strings.HasPrefix(got, ";with ") {
		t.Fatalf("leading semicolon lost: %q", got)
	}
}

func TestFlattenPropagatesParseErrors(t *testing.T) {
	for _, sql := range []string{
		"with a as (select 'unterminated",
		"with a as (with b as (select 1) select * from b",
	} {
		out, changed, err := Flatten(sql)
		if err == nil {
			t.Fatalf("Flatten(%q) = nil error", sql)
		}
		if changed || out != sql {
			t.Fatalf("failed parse must return the input untouched: %q", out)
		}
	}
}
