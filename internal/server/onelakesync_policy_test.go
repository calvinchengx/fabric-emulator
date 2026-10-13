package server

// The pure half of the OneLake-security -> SQL endpoint sync: the strings that
// become a row-level security predicate, with no endpoint behind them.
//
// WHY THESE EXIST. rowPolicy, translateRowFilters, sourceRoleName, sqlLiteral
// and splitAgg were exercised only by the suites that need a SQL Server
// (WAREHOUSE_MSSQL_DSN), which skip offline -- so per-push coverage read 0.0%
// for each while they decide who sees which rows. What they build is SQL text,
// and the text is checkable without executing it: which layers are ANDed, which
// roles OR, what a reader in no role sees, and that a quote in a role name
// cannot end the literal it sits in.
//
// These are OUR OWN assertions on generated text. They are not evidence that
// SQL Server evaluates the predicate the way Fabric does; that remains the
// mssql-backed suites' claim.

import (
	"database/sql"
	"strings"
	"testing"
)

func TestSQLQuotingCannotBeEscaped(t *testing.T) {
	for in, want := range map[string]string{
		"plain":   "N'plain'",
		"o'brien": "N'o''brien'",
		"''":      "N''''''",
		"":        "N''",
	} {
		if got := sqlLiteral(in); got != want {
			t.Errorf("sqlLiteral(%q) = %s, want %s", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"region": "[region]",
		"a]b":    "[a]]b]",
		"]":      "[]]]",
	} {
		if got := sqlIdent(in); got != want {
			t.Errorf("sqlIdent(%q) = %s, want %s", in, got, want)
		}
	}
}

// A STRING_AGG over no rows is NULL, which is "none", not one empty entry --
// the difference between zero disabled policies and a stray empty name that
// would later be spliced into an ALTER SECURITY POLICY.
func TestSplitAggReadsNullAsNone(t *testing.T) {
	if got := splitAgg(sql.NullString{}); got != nil {
		t.Errorf("NULL read as %q, want none", got)
	}
	got := splitAgg(sql.NullString{String: "a\x01b\x01c", Valid: true})
	if strings.Join(got, "|") != "a|b|c" {
		t.Errorf("split %q, want a|b|c", got)
	}
	if got := splitAgg(sql.NullString{String: "only", Valid: true}); len(got) != 1 || got[0] != "only" {
		t.Errorf("single entry read as %q", got)
	}
}

func TestContainsString(t *testing.T) {
	xs := []string{"region", "amount"}
	if !containsString(xs, "amount") || containsString(xs, "Amount") || containsString(nil, "x") {
		t.Error("containsString must be an exact, case-sensitive membership test")
	}
}

// The source's role is named from BOTH the source item and the role, because a
// role called "reader" on two different sources is two different groups of
// people. Colliding them would let one source's membership narrow, or fail to
// narrow, another's rows.
func TestSourceRoleNameSeparatesSources(t *testing.T) {
	a1, _ := sourceRoleName("item-a")("reader")
	a2, _ := sourceRoleName("item-a")("reader")
	b, _ := sourceRoleName("item-b")("reader")
	other, _ := sourceRoleName("item-a")("writer")
	if a1 != a2 {
		t.Errorf("not deterministic: %s vs %s", a1, a2)
	}
	if a1 == b || a1 == other {
		t.Errorf("collision: item-a/reader=%s item-b/reader=%s item-a/writer=%s", a1, b, other)
	}
	if !strings.HasPrefix(a1, olsPrefix+"src_") {
		t.Errorf("%s lacks the %ssrc_ prefix that keeps it inside the OLS_ role namespace the sync owns", a1, olsPrefix)
	}
	// "OLS_src_" + 16 hex digits is far inside SQL Server's 128-character limit.
	if len(a1) != len(olsPrefix+"src_")+16 {
		t.Errorf("%s is %d characters", a1, len(a1))
	}
}

// Several rules over one table arrive joined by " UNION " and become an OR, and
// the columns the OR reads are the union of each rule's, without repeats -- they
// become the predicate function's parameters, so a duplicate would be a
// duplicate parameter name.
func TestTranslateRowFiltersJoinsRulesAsOr(t *testing.T) {
	ci := " COLLATE " + rlsCollation
	expr, used, err := translateRowFilters(
		"SELECT * FROM sales WHERE region = 'west'"+
			" UNION SELECT * FROM sales WHERE amount > 5"+
			" UNION SELECT * FROM sales WHERE region = 'east'",
		"sales", salesColumns)
	if err != nil {
		t.Fatal(err)
	}
	want := "((r.[region]" + ci + " = N'west') OR (r.[amount] > 5) OR (r.[region]" + ci + " = N'east'))"
	if expr != want {
		t.Errorf("expr\n got %s\nwant %s", expr, want)
	}
	if strings.Join(used, ",") != "region,amount" {
		t.Errorf("columns %v, want region,amount with region once", used)
	}
}

// One rule that does not parse refuses the whole role's filter. The alternative,
// dropping it, would turn "rows matching A or B" into "rows matching A" --
// quietly narrower is a bug, but quietly wider is a leak, and a refusal is
// neither.
func TestTranslateRowFiltersRefusesWholeRoleOnOneBadRule(t *testing.T) {
	expr, used, err := translateRowFilters(
		"SELECT * FROM sales WHERE amount = 1 UNION UPDATE sales SET amount = 1",
		"sales", salesColumns)
	if err == nil {
		t.Fatalf("accepted %s over %v", expr, used)
	}
	if !strings.Contains(err.Error(), "expects SELECT") {
		t.Errorf("error %q does not name the bad rule", err)
	}
	if expr != "" || used != nil {
		t.Errorf("returned %q %v alongside an error", expr, used)
	}
}

// unwrap reverses the EXEC(N'...') doubling so assertions can read the DDL as
// written. Doubling is itself asserted below.
func unwrap(s string) string { return strings.ReplaceAll(s, "''", "'") }

func TestRowPolicyComposesLayersWithAnd(t *testing.T) {
	cols := map[string]endpointColumn{
		"region": {Name: "region", Type: "varchar(10)", Text: true},
		"amount": {Name: "amount", Type: "int"},
	}
	roleA, roleB, roleC := sqlLiteral("OLS_a"), sqlLiteral("OLS_b"), sqlLiteral("OLS_c")
	own := rowLayer{
		filtered:   []roleFilter{{role: roleA, expr: "(r.[region] = N'west')"}},
		unfiltered: []string{roleB},
		narrowed:   []string{roleC},
	}
	src := rowLayer{unfiltered: []string{sqlLiteral("OLS_src_1")}}

	out := rowPolicy("sales", own, src, []string{"region"}, cols)
	ddl := unwrap(out)

	for _, want := range []string{
		// The function takes one parameter per column the predicate reads, typed
		// as the endpoint's column is, and projects it back under the column's name
		// so the filter expression can say r.[region].
		"(@p0 varchar(10)) RETURNS TABLE AS RETURN SELECT 1 AS ok FROM (SELECT @p0 AS [region]) AS r WHERE ",
		// A role that filters admits a reader only while the filter holds.
		"(IS_MEMBER(N'OLS_a') = 1 AND (r.[region] = N'west'))",
		// A role that grants whole admits unconditionally.
		"IS_MEMBER(N'OLS_b') = 1",
		// A reader in none of the layer's roles is unrestricted by it.
		"(IS_MEMBER(N'OLS_a') = 0 AND IS_MEMBER(N'OLS_b') = 0)",
		// The source layer is its own parenthesised clause.
		"(IS_MEMBER(N'OLS_src_1') = 1 OR (IS_MEMBER(N'OLS_src_1') = 0))",
		// RLS in one role and CLS in another is unsupported by Fabric; the
		// predicate shows such a reader nothing rather than the union.
		"NOT (IS_MEMBER(N'OLS_a') = 1 AND IS_MEMBER(N'OLS_c') = 1)",
		"ADD FILTER PREDICATE",
		"ON [dbo].[sales] WITH (STATE = ON, SCHEMABINDING = OFF)",
		"GRANT SELECT ON [dbo].",
	} {
		if !strings.Contains(ddl, want) {
			t.Errorf("DDL lacks %s\n%s", want, ddl)
		}
	}

	// Layers AND, so a row must clear the consumer's roles AND the source's.
	if !strings.Contains(ddl, " AND (IS_MEMBER(N'OLS_src_1') = 1") {
		t.Errorf("the source layer is not ANDed onto the consumer's:\n%s", ddl)
	}

	// Every EXEC literal must survive its own quoting: an odd number of quotes
	// outside the doubled pairs would end the literal early.
	for _, stmt := range strings.Split(strings.TrimSpace(out), "\n") {
		if !strings.HasPrefix(stmt, "EXEC(N'") {
			continue
		}
		body := strings.TrimSuffix(strings.TrimPrefix(stmt, "EXEC(N'"), "');")
		if strings.Contains(strings.ReplaceAll(body, "''", ""), "'") {
			t.Errorf("an unescaped quote inside an EXEC literal: %s", stmt)
		}
	}
}

// A role name carrying a quote is a SQL-injection attempt against the sync, and
// the quote reaches rowPolicy only through sqlLiteral. That is the property:
// whatever the role is called, the generated text stays one statement.
func TestRowPolicyKeepsAHostileRoleNameInsideItsLiteral(t *testing.T) {
	cols := map[string]endpointColumn{"id": {Name: "id", Type: "int"}}
	hostile := sqlLiteral("x') OR 1=1 --")
	out := rowPolicy("t", rowLayer{unfiltered: []string{hostile}}, rowLayer{}, []string{"id"}, cols)
	if !strings.Contains(unwrap(out), "IS_MEMBER(N'x'') OR 1=1 --') = 1") {
		t.Errorf("the quote in the role name was not doubled in the literal:\n%s", unwrap(out))
	}
	// And a table name with a bracket is an identifier, not syntax.
	out = rowPolicy("a]b", rowLayer{unfiltered: []string{sqlLiteral("r")}}, rowLayer{}, []string{"id"}, cols)
	if !strings.Contains(out, "ON [dbo].[a]]b]") {
		t.Errorf("table identifier not bracket-escaped:\n%s", out)
	}
}

// A filter that reads no column (TRUE, say) still needs a parameter for the
// function to bind to; the lexically least column is chosen so the generated
// name set is stable across runs rather than following Go's map order.
func TestRowPolicyWithNoColumnsUsedPicksTheLeastColumn(t *testing.T) {
	cols := map[string]endpointColumn{
		"zed": {Name: "zed", Type: "int"},
		"abc": {Name: "abc", Type: "varchar(5)"},
		"mid": {Name: "mid", Type: "int"},
	}
	layer := rowLayer{unfiltered: []string{sqlLiteral("r")}}
	first := rowPolicy("t", layer, rowLayer{}, nil, cols)
	if !strings.Contains(first, "(@p0 varchar(5))") || !strings.Contains(first, "SELECT @p0 AS [abc]") {
		t.Errorf("did not bind the least column [abc]:\n%s", first)
	}
	for i := 0; i < 20; i++ {
		if again := rowPolicy("t", layer, rowLayer{}, nil, cols); again != first {
			t.Fatalf("output varies between runs:\n%s\n%s", first, again)
		}
	}
}

// Policy and function names are derived from the table, so re-syncing one table
// replaces its own objects and never another's.
func TestRowPolicyNamesFollowTheTable(t *testing.T) {
	cols := map[string]endpointColumn{"id": {Name: "id", Type: "int"}}
	layer := rowLayer{unfiltered: []string{sqlLiteral("r")}}
	a := rowPolicy("orders", layer, rowLayer{}, []string{"id"}, cols)
	b := rowPolicy("orders", layer, rowLayer{}, []string{"id"}, cols)
	c := rowPolicy("customers", layer, rowLayer{}, []string{"id"}, cols)
	if a != b {
		t.Error("the same table produced different DDL")
	}
	if a == c {
		t.Error("two tables produced the same DDL, so one would overwrite the other's policy")
	}
	if !strings.Contains(a, "OLS_rls_") || !strings.Contains(a, "OLS_rlsfn_") {
		t.Errorf("objects fall outside the OLS_ prefix the sync clears:\n%s", a)
	}
}
