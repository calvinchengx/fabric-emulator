package tsql

import (
	"errors"
	"testing"

	sg "github.com/calvinchengx/sqlglot-go/sqlglot"
)

// The FOR JSON rule (checkForJSONSubquery) reads parentheses; it was once written
// off as needing a parse. This holds it to one: the same statements are read with
// sqlglot-go's T-SQL parser, and FOR JSON is nested when its clause sits in a
// SELECT that is inside another. Two readers built differently agreeing on every
// statement is the evidence that the parenthesis rule is the sentence Microsoft
// wrote — "you can't use it inside subqueries" — and not an approximation of it.
//
// sqlglot is a community parser, not Fabric; this says the rule means what the
// documentation says, not that Fabric behaves so. It also cannot read every
// statement: at v0.4.0 it stops at a FOR after ORDER BY, after a JOIN, and after
// a subquery in the select list ("range operator for"). Those are in strictCorpus,
// where the lexer rule is checked directly, and are left out here rather than
// skipped, so this test cannot pass on statements only one reader read.

// forJSONNested reports whether a FOR clause in the tree belongs to a SELECT
// nested inside another.
func forJSONNested(e *sg.Expression) bool {
	var walk func(n *sg.Expression, selects int) bool
	walk = func(n *sg.Expression, selects int) bool {
		if n.Class == "Select" {
			selects++
		}
		if n.Class == "ForClause" && selects > 1 {
			return true
		}
		for _, k := range n.Keys {
			switch v := n.Args[k].(type) {
			case *sg.Expression:
				if walk(v, selects) {
					return true
				}
			case []*sg.Expression:
				for _, c := range v {
					if walk(c, selects) {
						return true
					}
				}
			}
		}
		return false
	}
	return walk(e, 0)
}

func TestForJSONSubqueryAgreesWithSqlglotGo(t *testing.T) {
	for _, q := range []string{
		"select a from t for json path",
		"select a from t for json path, root('r'), include_null_values",
		"select a from (select a from t) x for json path",
		"select a from t union all select a from u for json path",
		"select (select a from t for json path) as j",
		"with c as (select a from t for json path) select * from c",
		"select * from (select a from t for json auto) x",
		"select a from t where a in (select b from u for json auto)",
		"select json_query((select a from t for json path))",
		"select a from t where exists (select 1 from u for json path)",
		"with c as (select a from t) select a from c for json path",
	} {
		tree, err := sg.ParseOne(q, "tsql")
		if err != nil {
			t.Errorf("sqlglot-go does not read %q: %v — the differential is only as good as the statements both can read", q, err)
			continue
		}
		var ue *UnsupportedError
		ours := errors.As(CheckStrict(q), &ue) && ue.Feature == "for-json-subquery"
		if theirs := forJSONNested(tree); ours != theirs {
			t.Errorf("%q: strict mode refuses=%v, the parse tree says nested=%v", q, ours, theirs)
		}
	}
}
