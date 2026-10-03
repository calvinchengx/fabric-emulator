package tsql

// Tests for Phase 3's wiring (docs/35-warehouse-time-travel.md): finding a
// statement's table references, materialising each one a fake resolver hands
// back as a #temp table, and refusing a reference to a column that did not
// exist as of the hint's timestamp. The resolver itself (reading real Delta
// history) is package warehouse's job and is tested there
// (internal/warehouse/timetravel_test.go); everything here is a fake, so a
// failure points at the statement rewrite, never at Delta.

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

var fixedAsOf = time.Date(2024, 3, 13, 19, 39, 35, 280_000_000, time.UTC)

// snap is a small helper for building a fake TimeTravelSnapshot in tests.
func snap(cols, types []string, rows [][]string, current []string) *TimeTravelSnapshot {
	if current == nil {
		current = cols
	}
	return &TimeTravelSnapshot{Columns: cols, SQLTypes: types, RowLiterals: rows, CurrentColumns: current}
}

// resolverOf builds a TimeTravelResolver from a table-name -> snapshot map
// (case-insensitive), recording every table name it was asked about in calls
// so a test can assert a #temp table, or a table already resolved via a
// self-join, was never offered to it.
func resolverOf(t *testing.T, tables map[string]*TimeTravelSnapshot, calls *[]string) TimeTravelResolver {
	t.Helper()
	return func(table string, asOf time.Time) (*TimeTravelSnapshot, bool, error) {
		if calls != nil {
			*calls = append(*calls, table)
		}
		if !asOf.Equal(fixedAsOf) {
			t.Errorf("resolver called with asOf = %s, want %s", asOf, fixedAsOf)
		}
		for name, s := range tables {
			if strings.EqualFold(name, table) {
				return s, true, nil
			}
		}
		return nil, false, nil
	}
}

const hint = ` OPTION (FOR TIMESTAMP AS OF '2024-03-13T19:39:35.280')`

func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// TestAdaptWithTimeTravelNilResolverIsAdapt: a nil resolver — every connection
// except a lakehouse analytics endpoint's — must behave byte-for-byte like
// plain Adapt, hint or no hint. This is the regression guarantee for every
// other surface: Phase 3 must change nothing about them.
func TestAdaptWithTimeTravelNilResolverIsAdapt(t *testing.T) {
	cases := []string{
		"SELECT * FROM dbo.T1",
		"SELECT * FROM dbo.T1" + hint,
		"CREATE TABLE t AS SELECT a FROM src",
		"WITH a AS (WITH b AS (SELECT 1 AS x) SELECT * FROM b) SELECT * FROM a",
	}
	for _, sql := range cases {
		wantOut, wantChanged, wantErr := Adapt(sql)
		gotOut, gotChanged, gotErr := AdaptWithTimeTravel(sql, nil)
		if gotOut != wantOut || gotChanged != wantChanged || errString(gotErr) != errString(wantErr) {
			t.Errorf("AdaptWithTimeTravel(%q, nil) = (%q, %v, %v), want (%q, %v, %v)",
				sql, gotOut, gotChanged, gotErr, wantOut, wantChanged, wantErr)
		}
	}
}

// TestAdaptWithTimeTravelNoHintIsAdapt: a resolver is wired up (an analytics
// endpoint) but the statement carries no hint at all — still plain Adapt, so
// the ordinary CTAS/flatten pipeline every other statement gets is untouched.
func TestAdaptWithTimeTravelNoHintIsAdapt(t *testing.T) {
	sql := "WITH a AS (WITH b AS (SELECT 1 AS x) SELECT * FROM b) SELECT * FROM a"
	resolve := resolverOf(t, nil, nil)
	wantOut, wantChanged, wantErr := Adapt(sql)
	gotOut, gotChanged, gotErr := AdaptWithTimeTravel(sql, resolve)
	if gotOut != wantOut || gotChanged != wantChanged || errString(gotErr) != errString(wantErr) {
		t.Errorf("got (%q, %v, %v), want (%q, %v, %v)", gotOut, gotChanged, gotErr, wantOut, wantChanged, wantErr)
	}
}

// TestAdaptWithTimeTravelMaterializesSingleTable is the paradigm case from
// the design doc: `SELECT * FROM t OPTION (FOR TIMESTAMP AS OF …)`.
func TestAdaptWithTimeTravelMaterializesSingleTable(t *testing.T) {
	s := snap([]string{"id", "name"}, []string{"INT", "VARCHAR(8000)"},
		[][]string{{"1", "N'Alice'"}, {"2", "N'Bob'"}}, nil)
	var calls []string
	resolve := resolverOf(t, map[string]*TimeTravelSnapshot{"customer": s}, &calls)

	out, changed, err := AdaptWithTimeTravel("SELECT id, name FROM dbo.Customer"+hint, resolve)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !changed {
		t.Fatal("want changed = true")
	}
	if len(calls) != 1 || calls[0] != "Customer" {
		t.Errorf("resolver calls = %v, want [Customer]", calls)
	}
	if strings.Contains(out, "OPTION") {
		t.Errorf("hint should be stripped: %s", out)
	}
	if strings.Contains(out, "dbo.Customer") {
		t.Errorf("the qualified table reference should not remain: %s", out)
	}
	for _, want := range []string{
		"CREATE TABLE #tt0 ([id] INT, [name] VARCHAR(8000));",
		"INSERT INTO #tt0 VALUES (1, N'Alice');",
		"INSERT INTO #tt0 VALUES (2, N'Bob');",
		"FROM #tt0 AS Customer",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// TestAdaptWithTimeTravelPreservesExplicitAlias: when the client already
// wrote an alias, the rewrite must not duplicate it — only the table name is
// swapped, since `AS a` already follows it in the source untouched.
func TestAdaptWithTimeTravelPreservesExplicitAlias(t *testing.T) {
	s := snap([]string{"id"}, []string{"INT"}, [][]string{{"1"}}, nil)
	resolve := resolverOf(t, map[string]*TimeTravelSnapshot{"t1": s}, nil)

	out, _, err := AdaptWithTimeTravel("SELECT a.id FROM dbo.T1 AS a"+hint, resolve)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Count(out, " AS a") != 1 {
		t.Errorf("want exactly one alias clause, got:\n%s", out)
	}
	if !strings.Contains(out, "FROM #tt0 AS a") {
		t.Errorf("want FROM #tt0 AS a, got:\n%s", out)
	}
}

// TestAdaptWithTimeTravelPreservesImplicitNameQualifier: with no alias at
// all, a later qualifier using the table's own name (`A.id`) must still
// resolve — which only works if the rewritten FROM clause keeps that name as
// an alias on the temp table rather than dropping it.
func TestAdaptWithTimeTravelPreservesImplicitNameQualifier(t *testing.T) {
	a := snap([]string{"id"}, []string{"INT"}, [][]string{{"1"}}, nil)
	b := snap([]string{"id"}, []string{"INT"}, [][]string{{"1"}}, nil)
	resolve := resolverOf(t, map[string]*TimeTravelSnapshot{"a": a, "b": b}, nil)

	out, _, err := AdaptWithTimeTravel("SELECT * FROM dbo.A, dbo.B WHERE A.id = B.id"+hint, resolve)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "FROM #tt0 AS A, #tt1 AS B") {
		t.Errorf("want both comma-joined tables aliased back to their own names, got:\n%s", out)
	}
	if !strings.Contains(out, "WHERE A.id = B.id") {
		t.Errorf("the WHERE clause's qualifiers must survive untouched:\n%s", out)
	}
}

// TestAdaptWithTimeTravelSelfJoinResolvesOnce: a self-join references the
// same table twice under two aliases. It must be resolved (and materialised)
// exactly once — the design doc's statement-wide scope means both sides agree
// on one snapshot, and asking the resolver twice would risk two different
// answers if the clock ticked between calls.
func TestAdaptWithTimeTravelSelfJoinResolvesOnce(t *testing.T) {
	s := snap([]string{"id", "parent_id"}, []string{"INT", "INT"}, [][]string{{"1", "0"}}, nil)
	var calls []string
	resolve := resolverOf(t, map[string]*TimeTravelSnapshot{"node": s}, &calls)

	out, _, err := AdaptWithTimeTravel(
		"SELECT a.id, b.id FROM dbo.Node AS a JOIN dbo.Node AS b ON a.parent_id = b.id"+hint, resolve)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(calls) != 1 {
		t.Errorf("resolver calls = %v, want exactly one", calls)
	}
	if strings.Count(out, "CREATE TABLE #tt0") != 1 {
		t.Errorf("want exactly one materialisation, got:\n%s", out)
	}
	if !strings.Contains(out, "FROM #tt0 AS a JOIN #tt0 AS b") {
		t.Errorf("want both aliases pointed at the same temp table, got:\n%s", out)
	}
}

// TestAdaptWithTimeTravelLeavesTempTablesAlone: a #temp table is never
// Delta-backed, so it must never even reach the resolver — docs/35 calls this
// out as "the one place the emulator is likely to agree for free... worth an
// assertion rather than an assumption".
func TestAdaptWithTimeTravelLeavesTempTablesAlone(t *testing.T) {
	var calls []string
	resolve := resolverOf(t, map[string]*TimeTravelSnapshot{}, &calls)

	out, changed, err := AdaptWithTimeTravel("SELECT * FROM #staging"+hint, resolve)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !changed {
		t.Fatal("want changed = true: the hint itself must still be stripped")
	}
	if len(calls) != 0 {
		t.Errorf("resolver should never be asked about a #temp table, got calls = %v", calls)
	}
	if !strings.Contains(out, "FROM #staging") || strings.Contains(out, "OPTION") {
		t.Errorf("want #staging left alone and the hint stripped, got:\n%s", out)
	}
}

// TestAdaptWithTimeTravelLeavesUnresolvedReferencesAlone covers a reference
// the resolver does not recognise at all (not a #temp table, just not a Delta
// table it knows about — a system view, say). It must be left exactly as
// written rather than guessed at.
func TestAdaptWithTimeTravelLeavesUnresolvedReferencesAlone(t *testing.T) {
	resolve := resolverOf(t, map[string]*TimeTravelSnapshot{}, nil)
	out, changed, err := AdaptWithTimeTravel("SELECT * FROM sys.tables"+hint, resolve)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !changed {
		t.Fatal("want changed = true: the hint itself must still be stripped")
	}
	if !strings.Contains(out, "FROM sys.tables") {
		t.Errorf("want sys.tables left alone, got:\n%s", out)
	}
}

// TestAdaptWithTimeTravelResolverErrorRejects: a genuine resolution failure
// (a timestamp predating the table's first commit, say) must reject the
// statement, never forward it — forwarding would run the un-time-travelled
// statement against today's data, the exact silent-wrong-answer docs/35
// exists to prevent.
func TestAdaptWithTimeTravelResolverErrorRejects(t *testing.T) {
	resolve := func(table string, asOf time.Time) (*TimeTravelSnapshot, bool, error) {
		return nil, false, fmt.Errorf("boom")
	}
	_, _, err := AdaptWithTimeTravel("SELECT * FROM dbo.T1"+hint, resolve)
	if err == nil {
		t.Fatal("want an error")
	}
	var tterr *TimeTravelError
	if !errors.As(err, &tterr) {
		t.Fatalf("want a *TimeTravelError, got %T: %v", err, err)
	}
	if tterr.Rule != "unavailable" {
		t.Errorf("Rule = %q, want %q", tterr.Rule, "unavailable")
	}
	if !strings.Contains(tterr.Detail, "T1") {
		t.Errorf("Detail should name the table: %q", tterr.Detail)
	}
}

// --- Class B: the schema-as-of check ---------------------------------------

// tableWithNewColumn is "today" has id, name, email — but email was added
// after the hint's timestamp, so Columns (as of) omits it.
func tableWithNewColumn() *TimeTravelSnapshot {
	return snap(
		[]string{"id", "name"}, []string{"INT", "VARCHAR(8000)"},
		[][]string{{"1", "N'Alice'"}},
		[]string{"id", "name", "email"}, // current: email exists today
	)
}

func TestCheckColumnsExistedAsOf(t *testing.T) {
	cases := []struct {
		name    string
		sql     string
		wantErr bool
	}{
		{"select old columns only", "SELECT id, name FROM dbo.Customer", false},
		{"select star", "SELECT * FROM dbo.Customer", true},
		{"select the too-new column explicitly", "SELECT email FROM dbo.Customer", true},
		{"select the too-new column qualified", "SELECT c.email FROM dbo.Customer AS c", true},
		{"qualified star", "SELECT c.* FROM dbo.Customer AS c", true},
		{"too-new column referenced only in WHERE", "SELECT id FROM dbo.Customer WHERE email = N'x'", true},
		{"unrelated column named like arithmetic is not a star",
			"SELECT id * 2 AS doubled FROM dbo.Customer", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resolve := resolverOf(t, map[string]*TimeTravelSnapshot{"customer": tableWithNewColumn()}, nil)
			_, _, err := AdaptWithTimeTravel(tc.sql+hint, resolve)
			if tc.wantErr {
				var tterr *TimeTravelError
				if !errors.As(err, &tterr) {
					t.Fatalf("want a *TimeTravelError, got %T: %v", err, err)
				}
				if tterr.Rule != "schema-as-of" {
					t.Errorf("Rule = %q, want %q", tterr.Rule, "schema-as-of")
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestCheckColumnsExistedAsOfAcrossJoin: the too-new column belongs to ONE
// joined table; a reference qualified to the OTHER table must not be
// penalised for it.
func TestCheckColumnsExistedAsOfAcrossJoin(t *testing.T) {
	stable := snap([]string{"id"}, []string{"INT"}, [][]string{{"1"}}, nil) // same schema then and now
	tables := map[string]*TimeTravelSnapshot{"customer": tableWithNewColumn(), "orders": stable}

	t.Run("qualified to the unaffected table is fine", func(t *testing.T) {
		resolve := resolverOf(t, tables, nil)
		_, _, err := AdaptWithTimeTravel(
			"SELECT o.id FROM dbo.Customer AS c JOIN dbo.Orders AS o ON c.id = o.id"+hint, resolve)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("unqualified star still catches the affected table", func(t *testing.T) {
		resolve := resolverOf(t, tables, nil)
		_, _, err := AdaptWithTimeTravel(
			"SELECT * FROM dbo.Customer AS c JOIN dbo.Orders AS o ON c.id = o.id"+hint, resolve)
		var tterr *TimeTravelError
		if !errors.As(err, &tterr) || tterr.Rule != "schema-as-of" {
			t.Fatalf("want a schema-as-of TimeTravelError, got %v", err)
		}
	})
}

// A plain batch's #temp table outlives the batch, so the materialisation has to
// be safe to run twice on one session: the second hint used to fail with
// "There is already an object named '#tt0'".
func TestAdaptWithTimeTravelDropsAStaleTempTableFirst(t *testing.T) {
	s := snap([]string{"id"}, []string{"INT"}, [][]string{{"1"}}, nil)
	out, changed, err := AdaptWithTimeTravel("SELECT id FROM dbo.customer"+hint,
		resolverOf(t, map[string]*TimeTravelSnapshot{"customer": s}, nil))
	if err != nil || !changed {
		t.Fatalf("= (%q, %v, %v)", out, changed, err)
	}
	drop := strings.Index(out, "IF OBJECT_ID('tempdb..#tt0') IS NOT NULL DROP TABLE #tt0;")
	create := strings.Index(out, "CREATE TABLE #tt0")
	if drop < 0 || create < 0 || drop > create {
		t.Fatalf("the drop must precede the create:\n%s", out)
	}
}
