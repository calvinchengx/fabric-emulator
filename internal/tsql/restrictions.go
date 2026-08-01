package tsql

// Fabric's own nested-CTE restrictions (docs/29-tsql-parity.md, T6d).
//
// T6c fixed a Class A gap: statements Fabric accepts and SQL Server rejects.
// This file exists to stop that fix opening a Class B one — statements *Fabric*
// rejects that the emulator would now quietly accept. A local build that goes
// green on SQL a real warehouse refuses is the failure this whole document is
// about, so every rule Microsoft documents is either enforced here or recorded
// below as already agreeing.
//
// Enforced:
//
//   - a nested CTE may appear only in a SELECT statement;
//   - no INSERT/UPDATE/DELETE/MERGE inside a nested CTE definition;
//   - no query hints (OPTION) inside a nested CTE definition;
//   - CTE names must be unique within a nesting level;
//   - a nested CTE is visible only to its own level and its parent's body —
//     the scope rule flattening would otherwise widen;
//   - nesting depth is capped (Microsoft documents 64).
//
// Already agreeing, so deliberately not enforced:
//
//   - a nested CTE in CREATE VIEW, and a nested CTE in a *general* subquery
//     (`SELECT * FROM (WITH …) x`), are both rejected by Fabric with Msg 156 —
//     and neither parses as a leading WITH statement here, so it is forwarded
//     untouched and SQL Server rejects it with the same Msg 156. Two engines,
//     one error: nothing to add.
//
// Not enforced, and why:
//
//   - `AS OF` in a nested definition (Fabric rejects) needs temporal-clause
//     parsing this lexer does not do; the construct cannot arise from the
//     tooling T6 targets.
//   - recursive CTEs are unsupported by Fabric but supported by SQL Server —
//     a pre-existing Class B entry belonging to T7, not something T6 introduced.

import (
	"fmt"
	"strings"
)

// maxNestingDepth mirrors the limit Microsoft documents for nested CTEs. It
// also bounds recursion over attacker-supplied SQL.
const maxNestingDepth = 64

// RestrictionError reports a statement that Fabric itself would reject. It is
// raised *instead of* rewriting, so the emulator refuses what the real
// warehouse refuses rather than silently being more permissive.
type RestrictionError struct {
	Rule   string // short identifier, e.g. "select-only"
	Detail string
}

func (e *RestrictionError) Error() string {
	return fmt.Sprintf("tsql: %s (Fabric nested-CTE restriction: %s)", e.Detail, e.Rule)
}

// checkNestedRestrictions runs every enforceable rule over a statement that
// contains nesting. Order is deliberate: structural rules first, so a
// malformed statement is reported by its clearest cause.
func checkNestedRestrictions(st *Statement) error {
	if err := checkDepth(st.With, 1); err != nil {
		return err
	}
	if err := checkSelectOnly(st.Tail); err != nil {
		return err
	}
	if err := checkLevelDuplicates(st.With); err != nil {
		return err
	}
	if err := checkNestedDefinitions(st.With, 0); err != nil {
		return err
	}
	return checkScope(st)
}

func checkDepth(w *With, depth int) error {
	if depth > maxNestingDepth {
		return &RestrictionError{"max-depth",
			fmt.Sprintf("nested CTEs are more than %d levels deep", maxNestingDepth)}
	}
	for _, c := range w.CTEs {
		if c.Inner != nil {
			if err := checkDepth(c.Inner, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkSelectOnly enforces that a nested CTE feeds a SELECT. `WITH … INSERT
// INTO t SELECT …` is ordinary T-SQL for a *non*-nested CTE, so this rule only
// applies on the nesting path.
func checkSelectOnly(tail string) error {
	kw := firstKeyword(tail)
	switch kw {
	case "select", "":
		return nil // "" = an empty tail; the parser already rejects malformed input
	case "(":
		return nil // a parenthesised or set-operation SELECT
	}
	return &RestrictionError{"select-only",
		fmt.Sprintf("a nested CTE may only be used in a SELECT statement, not %s", strings.ToUpper(kw))}
}

// checkLevelDuplicates enforces Fabric's "CTE names at the same nesting level
// can't be duplicated". Distinguishing this from cross-level shadowing matters:
// a same-level duplicate is invalid on Fabric too, whereas shadowing across
// levels is valid there and merely unflattenable here (ShadowedNameError).
func checkLevelDuplicates(w *With) error {
	seen := make(map[string]bool, len(w.CTEs))
	for _, c := range w.CTEs {
		id := c.Ident()
		if seen[id] {
			return &RestrictionError{"duplicate-name",
				fmt.Sprintf("CTE %s is declared twice at the same nesting level", c.Name)}
		}
		seen[id] = true
		if c.Inner != nil {
			if err := checkLevelDuplicates(c.Inner); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkNestedDefinitions applies the rules Microsoft scopes to a *nested* CTE's
// definition — no DML, no query hints — to CTEs at depth ≥ 1 only, so an outer
// CTE keeps whatever T-SQL it was always allowed.
func checkNestedDefinitions(w *With, depth int) error {
	for _, c := range w.CTEs {
		if depth > 0 {
			if kw := firstKeyword(c.Body); isDML(kw) {
				return &RestrictionError{"no-dml-in-definition",
					fmt.Sprintf("CTE %s: %s is not allowed in a nested CTE definition",
						c.Name, strings.ToUpper(kw))}
			}
			if hasOptionHint(c.Body) {
				return &RestrictionError{"no-query-hint",
					fmt.Sprintf("CTE %s: query hints (OPTION) are not allowed in a nested CTE definition", c.Name)}
			}
		}
		if c.Inner != nil {
			if err := checkNestedDefinitions(c.Inner, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkScope enforces the rule flattening would otherwise silently widen: a
// nested CTE is visible to its own level and to its parent's body, and nowhere
// else. Microsoft's own example of the violation fails on Fabric with Msg 208
// — and would succeed here once every level shares one namespace.
//
// Only identifiers that name a CTE *somewhere in this statement* are
// considered, so ordinary table and column names cannot trip it. A column that
// happens to share a name with a CTE in an unrelated branch is a false
// positive; that costs a refused statement (loud, Class A) rather than a
// wrong answer (silent, Class B), which is the trade this document asks for.
func checkScope(st *Statement) error {
	defined := map[string]bool{}
	collectNames(st.With, defined)

	sc := &scopeChecker{defined: defined}
	if err := sc.walk(st.With, nil); err != nil {
		return err
	}
	// The statement body sees only the outermost level.
	return sc.check(st.Tail, namesOf(st.With), "the statement body")
}

type scopeChecker struct{ defined map[string]bool }

// walk validates each definition against the names legally visible to it.
// visible carries the CTEs of enclosing levels that were declared earlier;
// siblings become visible as the list is traversed, matching sequential CTE
// semantics.
func (sc *scopeChecker) walk(w *With, visible []string) error {
	inScope := append([]string{}, visible...)
	for _, c := range w.CTEs {
		// A CTE's own nested list sees the enclosing scope plus earlier siblings.
		if c.Inner != nil {
			if err := sc.walk(c.Inner, inScope); err != nil {
				return err
			}
		}
		// The definition body additionally sees its own nested CTEs — the
		// "immediate higher level" the rule permits — and itself, so a
		// self-reference is reported by T7's recursive-CTE rule, not as a
		// misleading scope error.
		bodyScope := append(append([]string{}, inScope...), c.Ident())
		if c.Inner != nil {
			bodyScope = append(bodyScope, namesOf(c.Inner)...)
		}
		if err := sc.check(c.Body, bodyScope, "CTE "+c.Name); err != nil {
			return err
		}
		inScope = append(inScope, c.Ident())
	}
	return nil
}

// check rejects a region that references a CTE defined outside its scope.
func (sc *scopeChecker) check(body string, visible []string, where string) error {
	vis := make(map[string]bool, len(visible))
	for _, v := range visible {
		vis[v] = true
	}
	toks, err := Tokenize(body)
	if err != nil {
		return nil // unparseable bodies are the caller's problem, not this rule's
	}
	for _, t := range toks {
		if t.Kind != Word && t.Kind != QuotedIdent {
			continue
		}
		id := Ident(t.Text)
		if sc.defined[id] && !vis[id] {
			return &RestrictionError{"out-of-scope-reference",
				fmt.Sprintf("%s references CTE %s, which is declared in another nesting scope",
					where, t.Text)}
		}
	}
	return nil
}

func collectNames(w *With, out map[string]bool) {
	for _, c := range w.CTEs {
		out[c.Ident()] = true
		if c.Inner != nil {
			collectNames(c.Inner, out)
		}
	}
}

func namesOf(w *With) []string {
	out := make([]string, 0, len(w.CTEs))
	for _, c := range w.CTEs {
		out = append(out, c.Ident())
	}
	return out
}

// firstKeyword returns the first significant token of s, lowercased — skipping
// comments and whitespace, which is what makes it usable on dbt's output.
func firstKeyword(s string) string {
	toks, err := Tokenize(s)
	if err != nil {
		return ""
	}
	for _, t := range toks {
		if t.Trivia() {
			continue
		}
		return strings.ToLower(t.Text)
	}
	return ""
}

func isDML(kw string) bool {
	switch kw {
	case "insert", "update", "delete", "merge":
		return true
	}
	return false
}

// hasOptionHint reports an OPTION(...) query hint: the keyword followed by an
// opening paren, so a column or alias merely named "option" does not trip it.
func hasOptionHint(body string) bool {
	toks, err := Tokenize(body)
	if err != nil {
		return false
	}
	for i, t := range toks {
		if t.Kind != Word || !strings.EqualFold(t.Text, "option") {
			continue
		}
		for _, next := range toks[i+1:] {
			if next.Trivia() {
				continue
			}
			return next.Kind == Punct && next.Text == "("
		}
	}
	return false
}
