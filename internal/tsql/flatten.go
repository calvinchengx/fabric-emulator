package tsql

// Flattening nested CTEs into the sequential form SQL Server accepts
// (docs/29-tsql-parity.md, T6c).
//
//	WITH outer AS (WITH inner AS (…) SELECT * FROM inner) SELECT * FROM outer
//	→
//	WITH inner AS (…), outer AS (SELECT * FROM inner) SELECT * FROM outer
//
// # Why this refuses to rename
//
// Fabric permits the same CTE name at different nesting levels, and its own
// documentation demonstrates it. Flattening collapses every level into one
// namespace, so such a statement cannot be flattened without renaming one of
// the collisions — and renaming a CTE means rewriting every *reference* to it.
//
// Finding those references needs a real SQL parser, not a lexer. `cte1` in a
// body may be a table reference, a column, or an alias:
//
//	WITH cte1 AS (SELECT 1 AS cte1) SELECT cte1 FROM cte1
//
// A token-level substitution cannot tell the three apart, and rewriting the
// wrong one produces a statement that still executes and returns *different
// rows* — the precise failure mode docs/29's "rewrite or reject, never
// silently approximate" exists to forbid. So a shadowed statement is refused
// with ShadowedNameError, and the caller reports the limitation by name rather
// than guessing.
//
// # A known widening
//
// After flattening, a formerly-nested CTE is visible to the whole statement,
// whereas Fabric scopes it to its immediate higher level. A statement that
// Fabric rejects for referencing out of scope (Msg 208) may therefore succeed
// here. That is a Class B divergence — tracked as a T6d obligation, not solved
// at this layer.

import (
	"fmt"
	"strings"
)

// ShadowedNameError reports that flattening would collide two CTEs that Fabric
// keeps distinct by nesting level. It is a refusal, not a failure to parse:
// the statement is well-formed, and simply cannot be expressed sequentially
// without renaming.
type ShadowedNameError struct{ Name string }

func (e *ShadowedNameError) Error() string {
	return fmt.Sprintf("tsql: CTE %s is declared at more than one nesting level; "+
		"flattening would collide the two. Rename the inner CTE.", e.Name)
}

// Flatten rewrites nested CTEs in sql into an equivalent sequential CTE list.
//
// It reports changed=false and returns sql untouched when there is nothing to
// do — no WITH prefix, or a WITH prefix with no nesting — which is the common
// case and must stay allocation-cheap and byte-exact.
//
// An error means the statement was left alone deliberately: either it could
// not be parsed with confidence, or it is shadowed (ShadowedNameError).
func Flatten(sql string) (out string, changed bool, err error) {
	st, err := Parse(sql)
	if err != nil {
		return sql, false, err
	}
	if st == nil || !st.HasNestedCTE() {
		return sql, false, nil
	}

	var flat []*CTE
	hoist(st.With, &flat)

	// Every level now shares one namespace, so any repeated name is a
	// collision — including one that was legal in the nested original.
	seen := make(map[string]bool, len(flat))
	for _, c := range flat {
		id := c.Ident()
		if seen[id] {
			return sql, false, &ShadowedNameError{Name: c.Name}
		}
		seen[id] = true
	}

	var b strings.Builder
	b.Grow(len(sql) + 16)
	b.WriteString(st.Leading)
	b.WriteString("with ")
	for i, c := range flat {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(c.Name)
		if c.Columns != "" {
			b.WriteString(" ")
			b.WriteString(c.Columns)
		}
		b.WriteString(" as (")
		b.WriteString(c.Body)
		b.WriteString(")")
	}
	b.WriteString(" ")
	b.WriteString(st.Tail)
	return b.String(), true, nil
}

// hoist appends CTEs depth-first, innermost first — the order that keeps every
// definition ahead of its first use once the levels are collapsed. A CTE's own
// nested definitions are emitted before it; sibling order is preserved, so an
// inner CTE can still reference an earlier sibling of its parent.
func hoist(w *With, out *[]*CTE) {
	for _, c := range w.CTEs {
		if c.Inner != nil {
			hoist(c.Inner, out)
		}
		*out = append(*out, c)
	}
}
