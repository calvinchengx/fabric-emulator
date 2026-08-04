package tsql

import (
	"errors"
	"strings"
	"testing"
)

var ctasCorpus = []struct {
	name string
	sql  string
	want string // "" = must be left untouched
}{
	{
		name: "the basic form",
		sql:  "create table dst as select a, b from src where x = 1",
		want: "select a, b into dst from src where x = 1",
	},
	{
		name: "no FROM at all",
		sql:  "create table dst as select 1 as x",
		want: "select 1 as x into dst",
	},
	{
		name: "qualified target name",
		sql:  "create table [db].[dbo].[dst] as select a from src",
		want: "select a into [db].[dbo].[dst] from src",
	},
	{
		name: "a subquery in the select list must not capture INTO",
		sql:  "create table dst as select a, (select max(v) from other) as m from src",
		want: "select a, (select max(v) from other) as m into dst from src",
	},
	{
		name: "UNION — INTO belongs to the first SELECT",
		sql:  "create table dst as select a from t1 union all select b from t2",
		want: "select a into dst from t1 union all select b from t2",
	},
	{
		name: "leading CTE — INTO goes in the main query, not the definition",
		sql:  "create table dst as with c as (select a from src) select a from c",
		want: "with c as (select a from src) select a into dst from c",
	},
	{
		name: "Fabric table options are dropped",
		sql:  "create table dst with (distribution = round_robin) as select a from src",
		want: "select a into dst from src",
	},
	{
		name: "leading semicolon",
		sql:  ";create table dst as select a from src",
		want: ";select a into dst from src",
	},

	// --- must be left alone -------------------------------------------------
	{name: "ordinary CREATE TABLE", sql: "create table t (a int, b varchar(10))"},
	{name: "CREATE TABLE with a computed column", sql: "create table t (a int, b as a * 2)"},
	{name: "a SELECT that merely mentions the words", sql: "select 'create table x as select' as s"},
	{name: "CREATE VIEW", sql: "create view v as select a from t"},
	{name: "plain select", sql: "select 1"},
	{name: "empty", sql: ""},
}

func TestRewriteCTASCorpus(t *testing.T) {
	for _, tc := range ctasCorpus {
		t.Run(tc.name, func(t *testing.T) {
			out, changed, err := RewriteCTAS(tc.sql)
			if err != nil {
				t.Fatalf("RewriteCTAS: %v", err)
			}
			if tc.want == "" {
				if changed || out != tc.sql {
					t.Fatalf("altered a statement it should not touch: %q", out)
				}
				return
			}
			if !changed {
				t.Fatal("CTAS not recognised")
			}
			if out != tc.want {
				t.Fatalf("\n got %q\nwant %q", out, tc.want)
			}
			// The rewrite must be a fixed point: its output is no longer a CTAS.
			if _, again, _ := RewriteCTAS(out); again {
				t.Fatal("rewriting is not idempotent")
			}
		})
	}
}

// Adapt composes the two rewrites: CTAS produces a query, and a nested CTE
// inside it is then flattened, so the sidecar can run both at once.
// TestRewriteCTASStopsAtTheStatementBoundary pins the fix for a silent
// data-corruption bug: the CTAS body used to run to the end of the BATCH, so
// when the CTAS's own SELECT had no depth-0 FROM (a constant or aggregate
// select), the INTO was spliced into a LATER statement's FROM.
//
// The batch then executed without error and filled the target from the wrong
// source table, while the client's first statement degraded to a bare
// resultset — the worst shape of bug this rewriter can have, because nothing
// fails. Each case below produced corrupted SQL before the bound was added.
func TestRewriteCTASStopsAtTheStatementBoundary(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
		want string
	}{
		{
			name: "constant select followed by a query with its own FROM",
			sql:  "CREATE TABLE t AS SELECT 1 AS x; SELECT b FROM other",
			// Was: "SELECT 1 AS x; SELECT b into t FROM other" — t filled from `other`.
			want: "SELECT 1 AS x into t; SELECT b FROM other",
		},
		{
			name: "aggregate select followed by a query with its own FROM",
			sql:  "CREATE TABLE summary AS SELECT COUNT(*) AS n; SELECT * FROM audit",
			want: "SELECT COUNT(*) AS n into summary; SELECT * FROM audit",
		},
		{
			name: "trailing statement is a write",
			sql:  "CREATE TABLE t AS SELECT 1 AS x; INSERT INTO log VALUES(1)",
			want: "SELECT 1 AS x into t; INSERT INTO log VALUES(1)",
		},
		{
			name: "the CTAS has its own FROM; the tail is untouched",
			sql:  "CREATE TABLE t AS SELECT a FROM src; SELECT b FROM other",
			want: "SELECT a into t FROM src; SELECT b FROM other",
		},
		{
			name: "a semicolon inside parens is not a terminator",
			sql:  "CREATE TABLE t AS SELECT (SELECT TOP 1 c FROM q) AS x; SELECT b FROM other",
			want: "SELECT (SELECT TOP 1 c FROM q) AS x into t; SELECT b FROM other",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, changed, err := RewriteCTAS(tc.sql)
			if err != nil || !changed {
				t.Fatalf("RewriteCTAS(%q) = changed:%v err:%v; want a rewrite", tc.sql, changed, err)
			}
			if got != tc.want {
				t.Errorf("RewriteCTAS(%q)\n got: %q\nwant: %q", tc.sql, got, tc.want)
			}
			// The tail must survive byte-for-byte: a later statement is not ours
			// to edit, and an INTO landing in it is the corruption this pins.
			if _, tail, found := strings.Cut(tc.sql, ";"); found {
				if !strings.HasSuffix(got, tail) {
					t.Errorf("tail after `;` was modified\n got: %q\nwant suffix: %q", got, tail)
				}
			}
		})
	}
}

func TestAdaptComposesCTASAndFlattening(t *testing.T) {
	sql := "create table dst as with o as (with i as (select 1 x) select * from i) select * from o"
	out, changed, err := Adapt(sql)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if strings.Count(strings.ToLower(out), "with ") != 1 {
		t.Fatalf("nesting survived: %q", out)
	}
	if !strings.Contains(out, "into dst") {
		t.Fatalf("CTAS not applied: %q", out)
	}
	if !strings.HasPrefix(out, "with i as (select 1 x), o as (") {
		t.Fatalf("got %q", out)
	}
}

func TestAdaptLeavesOrdinaryStatementsByteIdentical(t *testing.T) {
	for _, sql := range []string{
		"select 1",
		"create table t (a int)",
		"with a as (select 1 x), b as (select x from a) select * from b",
	} {
		out, changed, err := Adapt(sql)
		if err != nil || changed || out != sql {
			t.Fatalf("Adapt(%q): changed=%v err=%v out=%q", sql, changed, err, out)
		}
	}
}

// A refusal is about the statement as a whole, so it survives composition
// rather than being masked by a successful CTAS rewrite.
func TestAdaptPropagatesRefusals(t *testing.T) {
	sql := "create table dst as with c as (with c as (select 1 x) select * from c) select * from c"
	out, changed, err := Adapt(sql)
	var shadow *ShadowedNameError
	if !errors.As(err, &shadow) {
		t.Fatalf("want ShadowedNameError, got %v", err)
	}
	if changed || out != sql {
		t.Fatal("a refused statement must come back untouched")
	}
}

func TestAdaptPropagatesTokenizerErrors(t *testing.T) {
	sql := "create table dst as select 'unterminated"
	out, changed, err := Adapt(sql)
	if err == nil {
		t.Fatal("expected a tokenizer error")
	}
	if changed || out != sql {
		t.Fatal("input must be returned untouched")
	}
}

// Malformed CTAS shapes are left alone rather than half-rewritten.
func TestRewriteCTASLeavesMalformedShapesAlone(t *testing.T) {
	for _, sql := range []string{
		"create table",     // no name
		"create table dst", // no AS
		"create table dst with as select a from t",    // WITH without a paren group
		"create table dst with (a = 1 as select b",    // unbalanced options
		"create table dst as insert into t values(1)", // body is not a query
		"create table dst as",                         // nothing after AS
	} {
		out, changed, err := RewriteCTAS(sql)
		if err != nil {
			continue // a tokenizer failure is an acceptable outcome
		}
		if changed || out != sql {
			t.Fatalf("RewriteCTAS(%q) = %q", sql, out)
		}
	}
}

func TestSkipBalancedReportsUnclosedGroup(t *testing.T) {
	toks, _ := Tokenize("( a , b")
	if got := skipBalanced(significant(toks), 0); got != -1 {
		t.Fatalf("got %d, want -1", got)
	}
}

// The shape dbt-fabric actually sends: the CTAS lives inside an EXEC string
// literal, so a top-level-only rewrite misses it entirely.
func TestAdaptRewritesCTASInsideEXEC(t *testing.T) {
	sql := `EXEC('CREATE TABLE [db].[dbo].[x__dbt_temp]  AS SELECT * FROM [db].[dbo].[x_vw] 
    OPTION (LABEL = ''dbt-fabric-dw'');
');`
	out, changed, err := Adapt(sql)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if strings.Contains(strings.ToUpper(out), "CREATE TABLE") {
		t.Fatalf("CTAS survived inside the literal: %q", out)
	}
	if !strings.Contains(strings.ToUpper(out), "INTO [DB].[DBO].[X__DBT_TEMP]") {
		t.Fatalf("INTO not spliced: %q", out)
	}
	// The doubled-quote escaping of the inner literal must be preserved.
	if !strings.Contains(out, "''dbt-fabric-dw''") {
		t.Fatalf("inner escaping lost: %q", out)
	}
	// And the result must still be one well-formed EXEC call.
	if !strings.HasPrefix(out, "EXEC('") || !strings.HasSuffix(out, "');") {
		t.Fatalf("EXEC wrapper damaged: %q", out)
	}
}

func TestAdaptRewritesEveryEXECInABatch(t *testing.T) {
	sql := "EXEC('create table a as select 1 x'); EXEC('create table b as select 2 y');"
	out, changed, err := Adapt(sql)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if strings.Count(strings.ToLower(out), "into a") != 1 || strings.Count(strings.ToLower(out), "into b") != 1 {
		t.Fatalf("not every EXEC rewritten: %q", out)
	}
}

// Dynamic SQL whose content is not knowable must be left alone.
func TestAdaptLeavesNonLiteralEXECAlone(t *testing.T) {
	for _, sql := range []string{
		"EXEC(@stmt)",
		"EXEC('create table a as select 1' + @suffix)",
		"EXEC sp_who",
		"EXEC('select 1')", // a literal with nothing to rewrite
	} {
		out, changed, err := Adapt(sql)
		if err != nil {
			t.Fatalf("Adapt(%q): %v", sql, err)
		}
		if changed || out != sql {
			t.Fatalf("Adapt(%q) = %q", sql, out)
		}
	}
}

// A nested CTE inside dynamic SQL is flattened as well.
func TestAdaptFlattensNestedCTEInsideEXEC(t *testing.T) {
	sql := "EXEC('with o as (with i as (select 1 x) select * from i) select * from o')"
	out, changed, err := Adapt(sql)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if !strings.Contains(out, "with i as (select 1 x), o as (") {
		t.Fatalf("got %q", out)
	}
}

// N'…' dynamic SQL keeps its Unicode prefix through the rewrite.
func TestAdaptPreservesUnicodeLiteralPrefix(t *testing.T) {
	out, changed, err := Adapt("EXEC(N'create table a as select 1 x')")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if !strings.HasPrefix(out, "EXEC(N'") {
		t.Fatalf("N prefix lost: %q", out)
	}
}

func TestQuoteRoundTripsThroughEscaping(t *testing.T) {
	for _, s := range []string{"plain", "it's", "''already''", "a'b'c"} {
		got, unicode, ok := unquoteSQLString(quoteSQLString(s, false))
		if !ok || unicode || got != s {
			t.Fatalf("round-trip of %q: got %q ok=%v unicode=%v", s, got, ok, unicode)
		}
	}
	if _, _, ok := unquoteSQLString("notaliteral"); ok {
		t.Fatal("a non-literal was accepted")
	}
	if _, _, ok := unquoteSQLString("N"); ok {
		t.Fatal("a bare N was accepted")
	}
}

func TestAdaptDynamicSQLPropagatesTokenizerError(t *testing.T) {
	if _, _, err := adaptDynamicSQL("select 'unterminated"); err == nil {
		t.Fatal("expected a tokenizer error")
	}
}

// The remaining give-up paths, exercised directly: each returns the input
// untouched rather than a half-rewritten statement.
func TestCTASGiveUpPaths(t *testing.T) {
	// Adapt propagates a failure raised only by the dynamic-SQL pass: the outer
	// statement is fine, the EXEC argument is not.
	sql := "EXEC('create table a as select 1'); select 'unterminated"
	out, changed, err := Adapt(sql)
	if err == nil {
		t.Fatal("expected the dynamic pass to fail")
	}
	if changed || out != sql {
		t.Fatalf("input must be returned untouched: %q", out)
	}
}

// A malformed EXEC argument is skipped, not guessed at. The tokenizer only
// yields a String token for a well-formed literal, so this drives
// unquoteSQLString directly.
func TestUnquoteRejectsMalformedLiterals(t *testing.T) {
	for _, tok := range []string{"", "'", "x'y'", "N", "Nx"} {
		if _, _, ok := unquoteSQLString(tok); ok {
			t.Fatalf("accepted %q as a literal", tok)
		}
	}
}
