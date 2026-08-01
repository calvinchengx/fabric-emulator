package tsql

import (
	"errors"
	"strings"
	"testing"
)

// strictCorpus is the Class B contract: what strict mode refuses, and — just as
// important — what it must leave alone. feature is "" when the statement is
// legitimate Fabric T-SQL.
var strictCorpus = []struct {
	name    string
	sql     string
	feature string
}{
	// --- refused: Fabric rejects these, the sidecar would run them ----------
	{"recursive CTE", "with r as (select 1 n union all select n+1 from r where n < 5) select * from r", "recursive-cte"},
	{"recursive CTE, quoted self-reference", `with [r] as (select 1 n union all select n+1 from [r]) select * from [r]`, "recursive-cte"},
	{"recursive CTE nested inside another", "with o as (with r as (select 1 n union all select n+1 from r) select * from r) select * from o", "recursive-cte"},
	{"trigger", "create trigger t on dbo.x after insert as select 1", "triggers"},
	{"synonym", "create synonym s for dbo.t", "synonyms"},
	{"create user", "create user u without login", "create-user"},
	{"isolation level", "set transaction isolation level read committed", "set-isolation-level"},
	{"rowcount", "set rowcount 100", "set-rowcount"},
	{"identity insert", "set identity_insert dbo.t on", "identity-insert"},
	{"for xml", "select * from t for xml auto", "for-xml"},
	{"predict", "select * from predict(model = @m, data = t)", "predict"},
	{"sp_showspaceused", "exec sp_showspaceused", "sp-showspaceused"},
	{"identity seed", "create table t (id bigint identity(1,1), v int)", "identity-seed"},
	{"enforced primary key", "create table t (id bigint primary key)", "enforced-constraint"},
	{"enforced unique", "create table t (id bigint unique)", "enforced-constraint"},
	{"enforced foreign key", "create table t (id bigint, foreign key (id) references u(id))", "enforced-constraint"},
	{"enforced references", "alter table t add constraint fk references u(id)", "enforced-constraint"},
	{"multi-column statistics", "create statistics s on t (a, b)", "multi-column-stats"},

	// --- allowed: legitimate Fabric T-SQL that must not be refused ----------
	{"plain select", "select * from t", ""},
	{"sequential CTE", "with a as (select 1 x), b as (select x from a) select * from b", ""},
	{"nested CTE (T6 rewrites, T7 must not refuse)", "with o as (with i as (select 1 x) select * from i) select * from o", ""},
	{"constraint declared NOT ENFORCED", "create table t (id bigint primary key not enforced)", ""},
	{"bare IDENTITY without a seed", "create table t (id bigint identity, v int)", ""},
	{"single-column statistics", "create statistics s on t (a)", ""},
	{"a column merely named identity", "select identity from t", ""},
	{"the word trigger in a literal", "select 'create trigger x' as s", ""},
	{"FOR XML inside a string literal", "select 'for xml auto' as s", ""},
	{"primary key outside a CREATE TABLE", "select primary_key from t", ""},
	{"unparseable input is not our call", "select 'unterminated", ""},
	{"empty", "", ""},
}

func TestCheckStrictCorpus(t *testing.T) {
	for _, tc := range strictCorpus {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckStrict(tc.sql)
			if tc.feature == "" {
				if err != nil {
					t.Fatalf("legitimate statement refused: %v", err)
				}
				return
			}
			var ue *UnsupportedError
			if !errors.As(err, &ue) {
				t.Fatalf("expected UnsupportedError(%s), got %v", tc.feature, err)
			}
			if ue.Feature != tc.feature {
				t.Fatalf("feature = %q, want %q (%s)", ue.Feature, tc.feature, ue.Detail)
			}
			if !strings.Contains(ue.Error(), "Fabric") {
				t.Fatalf("error does not say who rejects it: %s", ue.Error())
			}
		})
	}
}

// Every feature the checker can report must appear in the corpus, so a check
// cannot be added — or silently lost — without a case for it.
func TestStrictCorpusCoversEveryFeature(t *testing.T) {
	seen := map[string]bool{}
	for _, tc := range strictCorpus {
		if tc.feature != "" {
			seen[tc.feature] = true
		}
	}
	for _, want := range []string{
		"recursive-cte", "triggers", "synonyms", "create-user", "set-isolation-level",
		"set-rowcount", "identity-insert", "for-xml", "predict", "sp-showspaceused",
		"identity-seed", "enforced-constraint", "multi-column-stats",
	} {
		if !seen[want] {
			t.Errorf("strict corpus no longer covers %q", want)
		}
	}
}

// A leading semicolon must not hide a statement from the checks.
func TestCheckStrictSeesPastLeadingSemicolon(t *testing.T) {
	if err := CheckStrict("; set rowcount 100"); err == nil {
		t.Fatal("a leading semicolon hid the statement")
	}
}

// Strict mode is case-insensitive, as T-SQL keywords are.
func TestCheckStrictIsCaseInsensitive(t *testing.T) {
	for _, sql := range []string{
		"CREATE TRIGGER t ON dbo.x AFTER INSERT AS SELECT 1",
		"Set RowCount 100",
		"SELECT * FROM t FOR XML AUTO",
	} {
		if err := CheckStrict(sql); err == nil {
			t.Fatalf("not refused: %q", sql)
		}
	}
}

func TestUnsupportedErrorNamesFeatureAndCause(t *testing.T) {
	e := &UnsupportedError{Feature: "recursive-cte", Detail: "CTE r references itself"}
	msg := e.Error()
	if !strings.Contains(msg, "recursive-cte") || !strings.Contains(msg, "references itself") ||
		!strings.Contains(msg, "strict mode") {
		t.Fatalf("unhelpful message: %s", msg)
	}
}

// Statistics with an unbalanced column list must not be mistaken for
// multi-column ones.
func TestMultiColumnStatsIgnoresUnbalancedInput(t *testing.T) {
	if err := CheckStrict("create statistics s on t (a, b"); err != nil {
		t.Fatalf("unbalanced list reported as multi-column: %v", err)
	}
}

// A CTE body that fails to tokenise is skipped rather than reported.
func TestRecursiveCheckSkipsUnparseableBody(t *testing.T) {
	if err := recursiveIn(&With{CTEs: []*CTE{{Name: "a", Body: "select 'unterminated"}}}); err != nil {
		t.Fatalf("unparseable body reported: %v", err)
	}
	if err := recursiveIn(nil); err != nil {
		t.Fatalf("nil list reported: %v", err)
	}
}
