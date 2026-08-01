package tsql

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// rejects asserts the statement is refused with a RestrictionError of the given
// rule, and that the input is returned untouched.
func rejects(t *testing.T, rule, sql string) {
	t.Helper()
	out, changed, err := Flatten(sql)
	var re *RestrictionError
	if !errors.As(err, &re) {
		t.Fatalf("Flatten(%q):\n got err %v\nwant RestrictionError(%s)", sql, err, rule)
	}
	if re.Rule != rule {
		t.Fatalf("Flatten(%q): rule = %q, want %q (%s)", sql, re.Rule, rule, re.Detail)
	}
	if changed || out != sql {
		t.Fatalf("a refused statement must come back untouched: changed=%v", changed)
	}
}

// The scope rule flattening would otherwise widen — Microsoft's own example of
// a statement Fabric rejects with Msg 208. Without this check the emulator
// would happily run it, which is a Class B divergence introduced by the fix.
func TestRejectsOutOfScopeReference(t *testing.T) {
	rejects(t, "out-of-scope-reference", `
with outer_1 as (
    with inner_1_1 as (select 1 x)
    select * from inner_1_1
),
outer_2 as (
    with inner_2_1 as (select 2 x)
    select * from inner_1_1, inner_2_1
)
select * from outer_2`)
}

// The statement body cannot see a nested CTE either.
func TestRejectsTailReferencingNestedCTE(t *testing.T) {
	rejects(t, "out-of-scope-reference",
		"with o as (with i as (select 1 x) select * from i) select * from i")
}

// Legal visibility must still pass: own level, earlier siblings, enclosing
// levels, and a parent seeing its own nested CTEs.
func TestScopeAllowsLegalVisibility(t *testing.T) {
	for _, sql := range []string{
		// parent sees its own nested CTE
		"with o as (with i as (select 1 x) select * from i) select * from o",
		// nested CTE sees an earlier sibling of its parent
		"with a as (select 1 x), b as (with c as (select * from a) select * from c) select * from b",
		// nested siblings see each other
		"with o as (with i1 as (select 1 x), i2 as (select * from i1) select * from i2) select * from o",
		// deep chain
		"with l1 as (with l2 as (with l3 as (select 1 x) select * from l3) select * from l2) select * from l1",
	} {
		if _, changed, err := Flatten(sql); err != nil || !changed {
			t.Fatalf("Flatten(%q) rejected a legal statement: changed=%v err=%v", sql, changed, err)
		}
	}
}

func TestRejectsNonSelectStatements(t *testing.T) {
	for _, sql := range []string{
		"with o as (with i as (select 1 x) select * from i) insert into t select * from o",
		"with o as (with i as (select 1 x) select * from i) delete from t where x in (select x from o)",
		"with o as (with i as (select 1 x) select * from i) update t set a = 1 from o",
	} {
		rejects(t, "select-only", sql)
	}
}

// A non-nested CTE feeding an INSERT is ordinary T-SQL and must not be touched.
func TestNonNestedInsertIsLeftAlone(t *testing.T) {
	sql := "with a as (select 1 x) insert into t select * from a"
	out, changed, err := Flatten(sql)
	if err != nil || changed || out != sql {
		t.Fatalf("changed=%v err=%v out=%q", changed, err, out)
	}
}

func TestRejectsDMLInsideNestedDefinition(t *testing.T) {
	rejects(t, "no-dml-in-definition",
		"with o as (with i as (insert into t values (1)) select * from i) select * from o")
}

func TestRejectsQueryHintInsideNestedDefinition(t *testing.T) {
	rejects(t, "no-query-hint",
		"with o as (with i as (select 1 x option (recompile)) select * from i) select * from o")
}

// "option" as a mere identifier is not a hint.
func TestOptionAsIdentifierIsNotAHint(t *testing.T) {
	sql := "with o as (with i as (select 1 as option_id, 2 as option) select * from i) select * from o"
	if _, changed, err := Flatten(sql); err != nil || !changed {
		t.Fatalf("false positive on an identifier named option: changed=%v err=%v", changed, err)
	}
}

// A same-level duplicate is invalid on Fabric too, so it is reported as a
// Fabric restriction rather than as this rewriter's shadowing limitation.
func TestRejectsSameLevelDuplicateAsFabricViolation(t *testing.T) {
	rejects(t, "duplicate-name",
		"with o as (with i as (select 1 x), i as (select 2 x) select * from i) select * from o")
}

// Whereas cross-level shadowing is legal on Fabric and merely unflattenable —
// a different error, because it means something different to the user.
func TestCrossLevelShadowingStaysAShadowError(t *testing.T) {
	sql := "with cte1 as (with cte1 as (select 1 x) select * from cte1) select * from cte1"
	_, _, err := Flatten(sql)
	var shadow *ShadowedNameError
	if !errors.As(err, &shadow) {
		t.Fatalf("want ShadowedNameError, got %v", err)
	}
	var re *RestrictionError
	if errors.As(err, &re) {
		t.Fatalf("cross-level shadowing must not be reported as a Fabric restriction: %v", re)
	}
}

func TestRejectsExcessiveNestingDepth(t *testing.T) {
	sql := "select 1 x"
	for i := 0; i <= maxNestingDepth+1; i++ {
		sql = fmt.Sprintf("with c%d as (%s) select * from c%d", i, sql, i)
	}
	rejects(t, "max-depth", sql)
}

func TestDepthJustUnderTheCapIsAccepted(t *testing.T) {
	sql := "select 1 x"
	for i := 0; i < maxNestingDepth-2; i++ {
		sql = fmt.Sprintf("with c%d as (%s) select * from c%d", i, sql, i)
	}
	if _, changed, err := Flatten(sql); err != nil || !changed {
		t.Fatalf("depth under the cap rejected: changed=%v err=%v", changed, err)
	}
}

func TestRestrictionErrorNamesTheRule(t *testing.T) {
	e := &RestrictionError{Rule: "select-only", Detail: "a nested CTE may only be used in a SELECT statement"}
	if !strings.Contains(e.Error(), "select-only") || !strings.Contains(e.Error(), "SELECT statement") {
		t.Fatalf("unhelpful error: %s", e.Error())
	}
}

// Restrictions apply only where nesting exists; ordinary statements are never
// examined, let alone refused.
func TestRestrictionsDoNotApplyWithoutNesting(t *testing.T) {
	for _, sql := range []string{
		"with a as (select 1 x option (recompile)) select * from a",
		"with a as (select 1 x), a2 as (select 2 x) insert into t select * from a",
		"select * from some_table",
	} {
		if _, changed, err := Flatten(sql); err != nil || changed {
			t.Fatalf("Flatten(%q): changed=%v err=%v", sql, changed, err)
		}
	}
}
