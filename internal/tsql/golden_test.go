package tsql

import (
	"errors"
	"strings"
	"testing"
)

// The golden corpus (docs/29-tsql-parity.md, T6f): every statement shape T6 is
// claimed to handle, with its expected outcome, in one place. The per-file
// tests prove the mechanisms; this proves the *contract*, and is where a new
// case belongs when one is discovered in the wild.
//
// outcome is exactly one of:
//
//	flatten  — rewritten, and want is the expected output
//	same     — forwarded byte-identical (nothing to do)
//	rule:X   — refused as a Fabric restriction with rule X
//	shadow   — refused because flattening would collide two CTE names
//	parse    — refused as unparseable, and therefore forwarded untouched
var goldenCorpus = []struct {
	name    string
	sql     string
	outcome string
	want    string // expected SQL for "flatten"
}{
	// --- the statements this milestone chain exists for -------------------
	{
		name:    "dbt accepted_values wrapper",
		sql:     `/* {"app": "dbt"} */ with test_main_sql as (with all_values as (select country as v from dim_customer group by country) select * from all_values where v not in ('US')), dbt_internal_test as (select * from test_main_sql) select count(*) as failures from dbt_internal_test`,
		outcome: "flatten",
		want:    `/* {"app": "dbt"} */ with all_values as (select country as v from dim_customer group by country), test_main_sql as (select * from all_values where v not in ('US')), dbt_internal_test as (select * from test_main_sql) select count(*) as failures from dbt_internal_test`,
	},
	{
		name:    "simple nesting",
		sql:     "with o as (with i as (select 1 x) select * from i) select * from o",
		outcome: "flatten",
		want:    "with i as (select 1 x), o as (select * from i) select * from o",
	},
	{
		name:    "three levels, innermost first",
		sql:     "with l1 as (with l2 as (with l3 as (select 1 x) select * from l3) select * from l2) select * from l1",
		outcome: "flatten",
		want:    "with l3 as (select 1 x), l2 as (select * from l3), l1 as (select * from l2) select * from l1",
	},
	{
		name:    "inner references an earlier sibling of its parent",
		sql:     "with a as (select 1 x), b as (with c as (select * from a) select * from c) select * from b",
		outcome: "flatten",
		want:    "with a as (select 1 x), c as (select * from a), b as (select * from c) select * from b",
	},
	{
		name:    "leading semicolon idiom",
		sql:     ";with o as (with i as (select 1 x) select * from i) select * from o",
		outcome: "flatten",
		want:    ";with i as (select 1 x), o as (select * from i) select * from o",
	},
	{
		name:    "quoted name with a column list",
		sql:     `with [my cte] (a, b) as (with i as (select 1, 2) select * from i) select * from [my cte]`,
		outcome: "flatten",
		want:    `with i as (select 1, 2), [my cte] (a, b) as (select * from i) select * from [my cte]`,
	},

	// --- the traps a regex would fall into ---------------------------------
	{
		name:    "CTE look-alike inside a string literal",
		sql:     "with a as (select 'with b as (select 1) select' as s) select * from a",
		outcome: "same",
	},
	{
		name:    "nested CTE spelled out inside a comment",
		sql:     "with a as (select 1 x /* with b as (select 2) select */) select * from a",
		outcome: "same",
	},
	{
		name:    "the word with inside a quoted identifier",
		sql:     `with a as (select 1 as [with b as]) select * from a`,
		outcome: "same",
	},
	{
		name:    "table hint, not a CTE",
		sql:     "select * from t with (nolock)",
		outcome: "same",
	},
	{
		name:    "sequential CTEs need no rewriting",
		sql:     "with a as (select 1 x), b as (select x from a) select * from b",
		outcome: "same",
	},
	{
		name:    "no CTE at all",
		sql:     "select 1",
		outcome: "same",
	},

	// --- every T6d refusal --------------------------------------------------
	{
		name:    "nested CTE feeding an INSERT",
		sql:     "with o as (with i as (select 1 x) select * from i) insert into t select * from o",
		outcome: "rule:select-only",
	},
	{
		name:    "DML inside a nested definition",
		sql:     "with o as (with i as (insert into t values (1)) select * from i) select * from o",
		outcome: "rule:no-dml-in-definition",
	},
	{
		name:    "query hint inside a nested definition",
		sql:     "with o as (with i as (select 1 x option (recompile)) select * from i) select * from o",
		outcome: "rule:no-query-hint",
	},
	{
		name:    "duplicate name at the same level",
		sql:     "with o as (with i as (select 1 x), i as (select 2 x) select * from i) select * from o",
		outcome: "rule:duplicate-name",
	},
	{
		name:    "reference escaping its nesting scope",
		sql:     "with p1 as (with q as (select 1 x) select * from q), p2 as (select * from q) select * from p2",
		outcome: "rule:out-of-scope-reference",
	},
	{
		name:    "statement body reaching into a nested scope",
		sql:     "with o as (with i as (select 1 x) select * from i) select * from i",
		outcome: "rule:out-of-scope-reference",
	},

	// --- the shadowing refusal ----------------------------------------------
	{
		name:    "same name at two nesting levels",
		sql:     "with c as (with c as (select 1 x) select * from c) select * from c",
		outcome: "shadow",
	},
	{
		name:    "shadowing across quoting and case",
		sql:     "with A as (with [a] as (select 1 x) select * from [a]) select * from A",
		outcome: "shadow",
	},

	// --- malformed input is forwarded, never guessed at ----------------------
	{
		name:    "unterminated string literal",
		sql:     "with a as (select 'unterminated",
		outcome: "parse",
	},
	{
		name:    "unbalanced parentheses",
		sql:     "with o as (with i as (select 1 x) select * from i select * from o",
		outcome: "parse",
	},
}

func TestGoldenCorpus(t *testing.T) {
	for _, tc := range goldenCorpus {
		t.Run(tc.name, func(t *testing.T) {
			out, changed, err := Flatten(tc.sql)

			switch {
			case tc.outcome == "flatten":
				if err != nil || !changed {
					t.Fatalf("expected a rewrite: changed=%v err=%v", changed, err)
				}
				if out != tc.want {
					t.Fatalf("\n got %q\nwant %q", out, tc.want)
				}
				// A rewrite must be idempotent — re-flattening changes nothing.
				if _, again, err := Flatten(out); err != nil || again {
					t.Fatalf("not a fixed point: changed=%v err=%v", again, err)
				}

			case tc.outcome == "same":
				if err != nil || changed {
					t.Fatalf("expected no rewrite: changed=%v err=%v", changed, err)
				}
				if out != tc.sql {
					t.Fatalf("bytes altered:\n got %q\nwant %q", out, tc.sql)
				}

			case strings.HasPrefix(tc.outcome, "rule:"):
				var re *RestrictionError
				if !errors.As(err, &re) {
					t.Fatalf("expected a Fabric restriction, got err=%v changed=%v", err, changed)
				}
				if want := strings.TrimPrefix(tc.outcome, "rule:"); re.Rule != want {
					t.Fatalf("rule = %q, want %q (%s)", re.Rule, want, re.Detail)
				}
				if changed || out != tc.sql {
					t.Fatal("a refused statement must be returned untouched")
				}

			case tc.outcome == "shadow":
				var se *ShadowedNameError
				if !errors.As(err, &se) {
					t.Fatalf("expected ShadowedNameError, got %v", err)
				}
				if changed || out != tc.sql {
					t.Fatal("a refused statement must be returned untouched")
				}

			case tc.outcome == "parse":
				if err == nil {
					t.Fatal("expected a parse failure")
				}
				var re *RestrictionError
				var se *ShadowedNameError
				if errors.As(err, &re) || errors.As(err, &se) {
					t.Fatalf("parse failure misreported as a refusal: %v", err)
				}
				if changed || out != tc.sql {
					t.Fatal("an unparseable statement must be returned untouched")
				}

			default:
				t.Fatalf("unknown outcome %q", tc.outcome)
			}
		})
	}
}

// Every outcome the contract defines must actually be exercised, so a corpus
// that quietly loses its only refusal case fails rather than passes.
func TestGoldenCorpusCoversEveryOutcome(t *testing.T) {
	seen := map[string]bool{}
	for _, tc := range goldenCorpus {
		seen[tc.outcome] = true
	}
	for _, want := range []string{
		"flatten", "same", "shadow", "parse",
		"rule:select-only", "rule:no-dml-in-definition", "rule:no-query-hint",
		"rule:duplicate-name", "rule:out-of-scope-reference",
	} {
		if !seen[want] {
			t.Errorf("golden corpus no longer covers outcome %q", want)
		}
	}
}
