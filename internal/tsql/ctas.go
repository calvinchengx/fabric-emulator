package tsql

// CTAS → SELECT … INTO (docs/29-tsql-parity.md, T8).
//
// The last Class A gap. Fabric and Synapse spell "materialise this query as a
// table" as CREATE TABLE … AS SELECT; SQL Server has no such statement and
// spells it SELECT … INTO. Same result, different syntax, so a client that
// writes valid Fabric T-SQL is rejected by the sidecar with Msg 156 — the same
// shape of divergence T6 fixed for nested CTEs.
//
//	CREATE TABLE dst AS SELECT a, b FROM src WHERE x
//	→
//	SELECT a, b INTO dst FROM src WHERE x
//
// # Where INTO goes
//
// Immediately before the first FROM at parenthesis depth 0. Everything nested —
// a subquery in the select list, a CTE definition — sits at depth ≥ 1, so it
// cannot be mistaken for the statement's own FROM. A body with no FROM at all
// (CREATE TABLE t AS SELECT 1) takes INTO at the end, which is equally valid.
//
// A leading WITH is handled by the same rule rather than a special case: the
// CTE definitions are parenthesised, so the first depth-0 FROM is the main
// query's, and `WITH c AS (…) SELECT … INTO t FROM c` is the correct output.
//
// # What is dropped, deliberately
//
// Fabric's CTAS accepts a WITH (DISTRIBUTION = …, CLUSTERED INDEX …) clause
// describing physical layout. SQL Server has no equivalent, and the sidecar is
// a stand-in engine rather than an MPP warehouse, so the clause is dropped. It
// affects data distribution, not results — the one thing this file changes that
// is not purely syntactic, and the reason it is called out here.

import "strings"

// RewriteCTAS converts a CREATE TABLE … AS SELECT statement into the
// equivalent SELECT … INTO. It reports changed=false, and returns sql
// untouched, for anything that is not a CTAS it fully understands.
func RewriteCTAS(sql string) (out string, changed bool, err error) {
	toks, err := Tokenize(sql)
	if err != nil {
		return sql, false, err
	}
	sig := significant(toks)
	if !startsWith(sig, "create", "table") {
		return sql, false, nil
	}
	// Re-slice past a leading semicolon so `;CREATE TABLE …` is seen too.
	if sig[0].Kind == Punct && sig[0].Text == ";" {
		sig = sig[1:]
	}

	i := 2 // past CREATE TABLE
	name, i, ok := scanQualifiedName(sig, i)
	if !ok {
		return sql, false, nil
	}
	// An optional WITH (…) table-option clause, which SQL Server cannot express.
	if i < len(sig) && sig[i].Kind == Word && strings.EqualFold(sig[i].Text, "with") {
		if i+1 >= len(sig) || sig[i+1].Kind != Punct || sig[i+1].Text != "(" {
			return sql, false, nil
		}
		i = skipBalanced(sig, i+1)
		if i < 0 {
			return sql, false, nil
		}
	}
	if i >= len(sig) || sig[i].Kind != Word || !strings.EqualFold(sig[i].Text, "as") {
		return sql, false, nil // a plain CREATE TABLE (…), not a CTAS
	}
	i++
	if i >= len(sig) {
		return sql, false, nil
	}
	// The body must be a query; anything else is not a CTAS we can rewrite.
	if sig[i].Kind != Word ||
		(!strings.EqualFold(sig[i].Text, "select") && !strings.EqualFold(sig[i].Text, "with")) {
		return sql, false, nil
	}

	body := sql[sig[i].Pos:]
	insertAt := ctasIntoOffset(sig[i:], sig[i].Pos, len(body))
	// Trim only spaces and tabs before splicing, so the insertion cannot double
	// an existing separator; a newline is kept, since it is still valid there
	// and preserves the statement's shape.
	head := strings.TrimRight(body[:insertAt], " \t")
	rewritten := head + " into " + name + " " + strings.TrimLeft(body[insertAt:], " \t")
	return sql[:sig[0].Pos] + strings.TrimRight(rewritten, " \t"), true, nil
}

// scanQualifiedName consumes a possibly multi-part table name
// (`[db].[schema].[t]`) and returns it as written.
func scanQualifiedName(sig []Token, i int) (string, int, bool) {
	if i >= len(sig) || (sig[i].Kind != Word && sig[i].Kind != QuotedIdent) {
		return "", i, false
	}
	var b strings.Builder
	b.WriteString(sig[i].Text)
	i++
	for i+1 < len(sig) && sig[i].Kind == Punct && sig[i].Text == "." &&
		(sig[i+1].Kind == Word || sig[i+1].Kind == QuotedIdent) {
		b.WriteString(".")
		b.WriteString(sig[i+1].Text)
		i += 2
	}
	return b.String(), i, true
}

// skipBalanced returns the index just past the parenthesised group opening at
// i, or -1 when it never closes.
func skipBalanced(sig []Token, i int) int {
	depth := 0
	for ; i < len(sig); i++ {
		if sig[i].Kind != Punct {
			continue
		}
		switch sig[i].Text {
		case "(":
			depth++
		case ")":
			if depth--; depth == 0 {
				return i + 1
			}
		}
	}
	return -1
}

// ctasIntoOffset returns the offset *within the body* where INTO must be
// inserted: before the first FROM at parenthesis depth 0, or at the end when
// the query has none. It scans the caller's existing tokens — re-tokenizing the
// body would add a failure path that cannot fire, since a body sliced at a
// token boundary always tokenizes.
func ctasIntoOffset(bodyToks []Token, bodyPos, bodyLen int) int {
	depth := 0
	for _, t := range bodyToks {
		switch {
		case t.Kind == Punct && t.Text == "(":
			depth++
		case t.Kind == Punct && t.Text == ")":
			depth--
		case depth == 0 && t.Kind == Word && strings.EqualFold(t.Text, "from"):
			return t.Pos - bodyPos
		}
	}
	return bodyLen
}

// Adapt applies every dialect rewrite the emulator performs, in the order they
// compose: CTAS first — it produces an ordinary query — then nested-CTE
// flattening over the result, so `CREATE TABLE t AS WITH a AS (WITH b …) …`
// comes out as valid SQL Server on both counts.
//
// It reports changed=false and returns sql byte-identical when nothing applies.
func Adapt(sql string) (out string, changed bool, err error) {
	out, changed, err = adaptStatement(sql)
	if err != nil {
		return sql, false, err
	}
	// Then the same rewrites inside any EXEC('…') argument, which is how
	// dbt-fabric actually ships its DDL. Not recursive beyond one level:
	// dynamic SQL that builds more dynamic SQL is left alone.
	// Defence in depth: adaptStatement's output always re-tokenizes, so this
	// error cannot fire today. adaptDynamicSQL's own failure path is tested
	// directly (TestAdaptDynamicSQLPropagatesTokenizerError).
	dyn, rewritten, err := adaptDynamicSQL(out)
	if err != nil {
		return sql, false, err
	}
	return dyn, changed || rewritten, nil
}

// adaptStatement applies the statement-level rewrites, in the order they
// compose: CTAS first — it produces an ordinary query — then nested-CTE
// flattening over the result.
func adaptStatement(sql string) (out string, changed bool, err error) {
	out, changed, err = RewriteCTAS(sql)
	if err != nil {
		return sql, false, err
	}
	flat, flattened, err := Flatten(out)
	if err != nil {
		// A refusal (a Fabric restriction, or shadowed names) is about the
		// statement as a whole, so it is reported even if CTAS already applied.
		return sql, false, err
	}
	return flat, changed || flattened, nil
}

// --- dynamic SQL ------------------------------------------------------------
//
// dbt-fabric does not send a bare CTAS. Measured against the real adapter, its
// table materialization wraps the DDL in dynamic SQL:
//
//	EXEC('CREATE TABLE [db].[dbo].[x__dbt_temp] AS SELECT * FROM …
//	      OPTION (LABEL = ''dbt-fabric-dw'');');
//
// The statement the emulator sees is therefore an EXEC whose argument is a
// string literal, and the CTAS lives *inside* that literal. Rewriting only
// top-level statements would leave it untouched — which is exactly what the
// first attempt at T8 did, and why this is measured rather than assumed.
//
// (`OPTION (LABEL = …)` needs no handling: SQL Server 2022 accepts it, verified
// directly against the engine.)

// adaptDynamicSQL rewrites the SQL inside EXEC('…') arguments. Only the
// single-literal form is touched: EXEC(@variable) or a concatenated expression
// is left alone, since its content is not knowable here.
func adaptDynamicSQL(sql string) (string, bool, error) {
	toks, err := Tokenize(sql)
	if err != nil {
		return sql, false, err
	}
	sig := significant(toks)

	type edit struct {
		start, end int
		repl       string
	}
	var edits []edit
	for i := 0; i+3 < len(sig); i++ {
		if sig[i].Kind != Word ||
			(!strings.EqualFold(sig[i].Text, "exec") && !strings.EqualFold(sig[i].Text, "execute")) {
			continue
		}
		if sig[i+1].Kind != Punct || sig[i+1].Text != "(" ||
			sig[i+2].Kind != String ||
			sig[i+3].Kind != Punct || sig[i+3].Text != ")" {
			continue
		}
		lit := sig[i+2]
		// The tokenizer only emits a String token for a well-formed literal, so
		// this guard cannot fire today; unquoteSQLString is tested directly
		// (TestUnquoteRejectsMalformedLiterals).
		inner, unicode, ok := unquoteSQLString(lit.Text)
		if !ok {
			continue
		}
		rewritten, changed, err := adaptStatement(inner)
		if err != nil || !changed {
			continue
		}
		edits = append(edits, edit{lit.Pos, lit.Pos + len(lit.Text), quoteSQLString(rewritten, unicode)})
	}
	if len(edits) == 0 {
		return sql, false, nil
	}
	// Apply back-to-front so earlier offsets stay valid.
	out := sql
	for i := len(edits) - 1; i >= 0; i-- {
		out = out[:edits[i].start] + edits[i].repl + out[edits[i].end:]
	}
	return out, true, nil
}

// unquoteSQLString returns the content of a T-SQL string literal, undoing the
// doubled-quote escape. unicode reports the N'…' form so it can be preserved.
func unquoteSQLString(tok string) (content string, unicode bool, ok bool) {
	if len(tok) >= 2 && (tok[0] == 'N' || tok[0] == 'n') {
		unicode, tok = true, tok[1:]
	}
	if len(tok) < 2 || tok[0] != '\'' || tok[len(tok)-1] != '\'' {
		return "", false, false
	}
	return strings.ReplaceAll(tok[1:len(tok)-1], "''", "'"), unicode, true
}

// quoteSQLString is the inverse of unquoteSQLString.
func quoteSQLString(s string, unicode bool) string {
	q := "'" + strings.ReplaceAll(s, "'", "''") + "'"
	if unicode {
		return "N" + q
	}
	return q
}
