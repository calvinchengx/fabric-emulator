package tsql

// Strict mode — Class B truthfulness (docs/29-tsql-parity.md, T7).
//
// T6 fixed Class A: statements Fabric accepts that the SQL Server sidecar
// rejects. This file addresses the opposite and more dangerous direction —
// constructs Fabric *rejects* that the sidecar happily runs. Those pass
// locally and fail in production, which makes a green local build a lie, and a
// green local build is the entire signal this emulator exists to provide.
//
// It is off by default and enabled with -tsql-strict (FABRIC_TSQL_STRICT),
// because unlike everything else in T6 it *removes* capability: SQL that works
// today would begin to fail. The default flips only once the checks have been
// exercised widely enough to trust.
//
// Enforced (each rejected with the Fabric restriction that forbids it):
//
//	recursive CTEs, triggers, synonyms, CREATE USER, SET TRANSACTION
//	ISOLATION LEVEL, SET ROWCOUNT, SET IDENTITY_INSERT, SELECT … FOR XML,
//	IDENTITY(seed, increment), enforced PRIMARY KEY / UNIQUE / FOREIGN KEY,
//	multi-column statistics, PREDICT, sp_showspaceused, FOR JSON inside a
//	subquery, and a `/` or `\` in the name of a schema or table being created.
//
// Deliberately not enforced, with the reason, because a lexer cannot see them:
//
//   - indexed ("materialized") views — needs correlating CREATE INDEX with the
//     view it targets, across statements;
//   - queries against system tables, and the vector data type — the sidecar
//     (SQL Server 2022) has no vector type either, so that row is not actually
//     a divergence here.

import (
	"fmt"
	"strings"
)

// UnsupportedError reports a construct that real Fabric does not support. It
// is raised only in strict mode: outside it, the sidecar's broader T-SQL is
// left alone.
type UnsupportedError struct {
	Feature string // short identifier, e.g. "recursive-cte"
	Detail  string
}

func (e *UnsupportedError) Error() string {
	return fmt.Sprintf("tsql: %s — unsupported in Fabric Data Warehouse (strict mode: %s)",
		e.Detail, e.Feature)
}

// leadingForms are statements identified by their opening keywords.
var leadingForms = []struct {
	words   []string
	feature string
	detail  string
}{
	{[]string{"create", "trigger"}, "triggers", "triggers are not supported"},
	{[]string{"create", "synonym"}, "synonyms", "synonyms are not supported"},
	{[]string{"create", "user"}, "create-user", "CREATE USER is not supported"},
	{[]string{"set", "transaction", "isolation", "level"}, "set-isolation-level",
		"SET TRANSACTION ISOLATION LEVEL is not supported"},
	{[]string{"set", "rowcount"}, "set-rowcount", "SET ROWCOUNT is not supported"},
	{[]string{"set", "identity_insert"}, "identity-insert", "IDENTITY_INSERT is not supported"},
}

// CheckStrict reports the first construct in sql that real Fabric rejects.
// It returns nil for anything it cannot read with confidence: an unparseable
// statement is the engine's business, not this checker's.
func CheckStrict(sql string) error {
	toks, err := Tokenize(sql)
	if err != nil {
		return nil
	}
	sig := significant(toks)
	if len(sig) == 0 {
		return nil
	}

	for _, f := range leadingForms {
		if startsWith(sig, f.words...) {
			return &UnsupportedError{f.feature, f.detail}
		}
	}
	if containsSeq(sig, "for", "xml") {
		return &UnsupportedError{"for-xml", "SELECT … FOR XML is not supported"}
	}
	if containsSeq(sig, "predict") {
		return &UnsupportedError{"predict", "PREDICT is not supported"}
	}
	if containsSeq(sig, "sp_showspaceused") {
		return &UnsupportedError{"sp-showspaceused", "sp_showspaceused is not supported"}
	}
	if err := checkIdentitySeed(sig); err != nil {
		return err
	}
	if err := checkEnforcedConstraints(sig); err != nil {
		return err
	}
	if err := checkMultiColumnStats(sig); err != nil {
		return err
	}
	if err := checkForJSONSubquery(sig); err != nil {
		return err
	}
	if err := checkObjectNames(sig); err != nil {
		return err
	}
	return checkRecursiveCTE(sql)
}

func significant(toks []Token) []Token {
	out := make([]Token, 0, len(toks))
	for _, t := range toks {
		if !t.Trivia() {
			out = append(out, t)
		}
	}
	return out
}

// startsWith matches leading keywords, skipping a leading semicolon so the
// `;WITH`-style idiom does not hide a statement from the checks.
func startsWith(sig []Token, words ...string) bool {
	if len(sig) > 0 && sig[0].Kind == Punct && sig[0].Text == ";" {
		sig = sig[1:]
	}
	return matchAt(sig, 0, words...)
}

func containsSeq(sig []Token, words ...string) bool {
	for i := range sig {
		if matchAt(sig, i, words...) {
			return true
		}
	}
	return false
}

func matchAt(sig []Token, i int, words ...string) bool {
	if i+len(words) > len(sig) {
		return false
	}
	for j, w := range words {
		t := sig[i+j]
		if t.Kind != Word || !strings.EqualFold(t.Text, w) {
			return false
		}
	}
	return true
}

// checkIdentitySeed rejects IDENTITY(seed, increment): Fabric supports the
// IDENTITY property but not a user-defined seed or increment. Bare IDENTITY is
// fine, so only the parenthesised form is refused.
func checkIdentitySeed(sig []Token) error {
	for i, t := range sig {
		if t.Kind != Word || !strings.EqualFold(t.Text, "identity") {
			continue
		}
		if i+1 < len(sig) && sig[i+1].Kind == Punct && sig[i+1].Text == "(" {
			return &UnsupportedError{"identity-seed",
				"defining an IDENTITY seed and increment is not supported"}
		}
	}
	return nil
}

// checkEnforcedConstraints rejects key constraints declared without NOT
// ENFORCED, which Fabric requires. Scoped to CREATE/ALTER TABLE so the
// keywords cannot fire inside an ordinary query.
func checkEnforcedConstraints(sig []Token) error {
	if !startsWith(sig, "create", "table") && !startsWith(sig, "alter", "table") {
		return nil
	}
	if containsSeq(sig, "not", "enforced") {
		return nil
	}
	for _, c := range []struct {
		words []string
		what  string
	}{
		{[]string{"primary", "key"}, "PRIMARY KEY"},
		{[]string{"foreign", "key"}, "FOREIGN KEY"},
		{[]string{"references"}, "FOREIGN KEY"},
		{[]string{"unique"}, "UNIQUE"},
	} {
		if containsSeq(sig, c.words...) {
			return &UnsupportedError{"enforced-constraint",
				fmt.Sprintf("%s constraints are supported only with NOT ENFORCED", c.what)}
		}
	}
	return nil
}

// checkMultiColumnStats rejects manually created multi-column statistics.
func checkMultiColumnStats(sig []Token) error {
	if !startsWith(sig, "create", "statistics") {
		return nil
	}
	// The column list is the first parenthesised group; more than one column in
	// it means multi-column statistics.
	depth, commas := 0, 0
	for _, t := range sig {
		if t.Kind != Punct {
			continue
		}
		switch t.Text {
		case "(":
			depth++
		case ")":
			if depth--; depth == 0 && commas > 0 {
				return &UnsupportedError{"multi-column-stats",
					"manually created multi-column statistics are not supported"}
			}
		case ",":
			if depth == 1 {
				commas++
			}
		}
	}
	return nil
}

// checkRecursiveCTE rejects a CTE that references itself. Fabric supports
// standard, sequential and nested CTEs but not recursive ones, while SQL
// Server runs them happily — so without this a hierarchy query passes locally
// and fails on Fabric.
//
// A CTE's own name appearing in its body is the signal. As with T6d's scope
// check, a column or alias sharing the CTE's name is a false positive; in
// strict mode — which the operator opted into precisely to be told about
// divergence — a loud refusal is the intended trade.
func checkRecursiveCTE(sql string) error {
	st, err := Parse(sql)
	if err != nil || st == nil {
		return nil
	}
	return recursiveIn(st.With)
}

func recursiveIn(w *With) error {
	if w == nil {
		return nil
	}
	for _, c := range w.CTEs {
		if err := recursiveIn(c.Inner); err != nil {
			return err
		}
		toks, err := Tokenize(c.Body)
		if err != nil {
			continue
		}
		self := c.Ident()
		for _, t := range toks {
			if (t.Kind == Word || t.Kind == QuotedIdent) && Ident(t.Text) == self {
				return &UnsupportedError{"recursive-cte",
					fmt.Sprintf("CTE %s references itself; recursive CTEs are not supported", c.Name)}
			}
		}
	}
	return nil
}

// checkForJSONSubquery rejects FOR JSON inside parentheses. Microsoft: "`FOR
// JSON` must be the last operator in the query, so you can't use it inside
// subqueries." A query's last operator is at the top level of its statement, so
// FOR JSON at any parenthesis depth above zero is in a subquery, a derived
// table, a CTE or a call — every form the sentence excludes.
//
// This was once written off as needing a real parse. It does not, for the
// sentence's own reason: the legal form is the one that is not nested, and nesting
// is what parentheses are. A parse is still the way to be sure of that, so
// TestForJSONSubqueryAgreesWithSqlglotGo reads the same statements with
// sqlglot-go's T-SQL parser and holds the two to the same verdict.
func checkForJSONSubquery(sig []Token) error {
	depth := 0
	for i, t := range sig {
		if t.Kind == Punct {
			switch t.Text {
			case "(":
				depth++
			case ")":
				if depth > 0 {
					depth--
				}
			}
			continue
		}
		if depth > 0 && matchAt(sig, i, "for", "json") {
			return &UnsupportedError{"for-json-subquery",
				"FOR JSON must be the last operator in the query, so it cannot be used inside a subquery"}
		}
	}
	return nil
}

// checkObjectNames rejects a `/` or `\` in the name of a schema or table being
// created: "Schema and table names can't contain `/` or `\`." Only a creation
// is looked at — CREATE or ALTER of a TABLE, VIEW or SCHEMA, and SELECT … INTO —
// and only the name itself: a column called [a/b], or a slash in a literal, is
// not what the sentence says. A quoted identifier is one token, so the name is
// read whole, however it is bracketed or split by dots.
func checkObjectNames(sig []Token) error {
	i := 0
	if len(sig) > 0 && sig[0].Kind == Punct && sig[0].Text == ";" {
		i = 1
	}
	var name int // index of the first token of the created object's name
	switch {
	case matchAt(sig, i, "create", "or", "alter"):
		name = i + 3
	case matchAt(sig, i, "create") || matchAt(sig, i, "alter"):
		name = i + 1
	case matchAt(sig, i, "select"):
		// SELECT … INTO <name>: the first INTO outside any parentheses.
		depth := 0
		for j := i; j < len(sig); j++ {
			switch {
			case sig[j].Kind == Punct && sig[j].Text == "(":
				depth++
			case sig[j].Kind == Punct && sig[j].Text == ")":
				depth--
			case depth == 0 && matchAt(sig, j, "into"):
				return checkNameAt(sig, j+1)
			}
		}
		return nil
	default:
		return nil
	}
	if !matchAt(sig, name, "table") && !matchAt(sig, name, "view") && !matchAt(sig, name, "schema") {
		return nil
	}
	return checkNameAt(sig, name+1)
}

// checkNameAt reads a (possibly dotted) name starting at sig[i] and refuses a part
// with a `/` or `\` in it.
func checkNameAt(sig []Token, i int) error {
	for i < len(sig) && (sig[i].Kind == Word || sig[i].Kind == QuotedIdent) {
		if strings.ContainsAny(Ident(sig[i].Text), `/\`) {
			return &UnsupportedError{"object-name-character",
				"schema and table names cannot contain / or \\"}
		}
		i++
		if i < len(sig) && sig[i].Kind == Punct && sig[i].Text == "." {
			i++
			continue
		}
		break
	}
	return nil
}
