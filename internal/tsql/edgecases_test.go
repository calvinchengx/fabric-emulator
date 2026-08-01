package tsql

import (
	"errors"
	"strings"
	"testing"
)

// A parenthesised or set-operation SELECT still satisfies "nested CTEs may only
// be used in a SELECT statement".
func TestSelectOnlyAcceptsParenthesisedSelect(t *testing.T) {
	sql := "with o as (with i as (select 1 x) select * from i) (select * from o)"
	if _, changed, err := Flatten(sql); err != nil || !changed {
		t.Fatalf("parenthesised SELECT rejected: changed=%v err=%v", changed, err)
	}
}

// A scope violation buried two levels down must still surface: the checker
// recurses, and an error from an inner level has to propagate out.
func TestScopeViolationPropagatesFromInnerLevels(t *testing.T) {
	sql := `
with p1 as (with q as (select 1 x) select * from q),
     p2 as (with r as (with s as (select * from q) select * from s) select * from r)
select * from p2`
	var re *RestrictionError
	if _, _, err := Flatten(sql); !errors.As(err, &re) || re.Rule != "out-of-scope-reference" {
		t.Fatalf("deep scope violation not reported: %v", err)
	}
}

// Helpers that tokenise a fragment must degrade quietly rather than mis-report,
// because a fragment is not guaranteed to be a well-formed statement.
func TestFragmentHelpersDegradeOnUnparseableInput(t *testing.T) {
	if got := firstKeyword("'unterminated"); got != "" {
		t.Fatalf("firstKeyword = %q, want empty", got)
	}
	if hasOptionHint("'unterminated") {
		t.Fatal("hasOptionHint claimed a hint in unparseable text")
	}
	sc := &scopeChecker{defined: map[string]bool{"a": true}}
	if err := sc.check("select 'unterminated", nil, "test"); err != nil {
		t.Fatalf("scope check on unparseable body: %v", err)
	}
}

// A fragment of only trivia has no keyword — and must not be mistaken for one.
func TestFirstKeywordSkipsTriviaAndHandlesEmpty(t *testing.T) {
	if got := firstKeyword("  /* just a comment */ \n -- and a line\n"); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
	if got := firstKeyword("   \t select 1"); got != "select" {
		t.Fatalf("got %q, want select", got)
	}
	if got := firstKeyword(""); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestTokenizeUnterminatedUnicodeLiteral(t *testing.T) {
	if _, err := Tokenize("select N'unterminated"); err == nil {
		t.Fatal("unterminated N'' accepted")
	}
}

// A nil CTE list has no nesting; the walk must tolerate it rather than panic.
func TestHasNestedOnNilList(t *testing.T) {
	var w *With
	if w.hasNested() {
		t.Fatal("nil With reported nesting")
	}
}

// Malformed shapes inside a CTE definition must be errors, not partial parses:
// each of these reaches a distinct failure path.
func TestParseRejectsMalformedInnerShapes(t *testing.T) {
	for name, sql := range map[string]string{
		"unbalanced column list":  "with a (x, y as (select 1) select * from a",
		"inner name is a literal": "with o as (with 'lit' as (select 1) select 1) select 1",
		"unbalanced inner parens": "with o as (with i as (select 1) select * from i select * from o",
	} {
		if _, err := Parse(sql); err == nil {
			t.Fatalf("%s: Parse accepted %q", name, sql)
		}
	}
}

// The refusal messages are user-facing: they must name the offending CTE, since
// the user's next action is to rename or restructure it.
func TestRefusalMessagesNameTheOffendingCTE(t *testing.T) {
	_, _, err := Flatten("with dup as (with dup as (select 1 x) select * from dup) select * from dup")
	if err == nil || !strings.Contains(err.Error(), "dup") {
		t.Fatalf("shadowing message does not name the CTE: %v", err)
	}
	_, _, err = Flatten("with o as (with i as (select 1 x option (recompile)) select * from i) select * from o")
	if err == nil || !strings.Contains(err.Error(), "i") {
		t.Fatalf("hint message does not name the CTE: %v", err)
	}
}
