package warehouse

import (
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/clock"
	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/calvinchengx/fabric-emulator/internal/tsql"
)

// setupTimeTravelStore builds a frozen-clock store with one lakehouse, ready
// for WriteDeltaTableAs — the same rig delta_asof_test.go uses, since this
// file tests the layer built directly on top of it.
func setupTimeTravelStore(t *testing.T) (*store.Store, *clock.Clock, *store.Item) {
	t.Helper()
	clk := clock.New()
	clk.Freeze()
	clk.Advance(3600 * 24 * 400)
	st, err := store.Open("", clk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ws := &store.Workspace{ID: "ws-tt", DisplayName: "w", Type: "Workspace"}
	if err := st.CreateWorkspace(ws, store.Principal{ID: "p", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	lh := &store.Item{ID: "it-tt", WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lake"}
	if err := st.CreateItem(lh, nil); err != nil {
		t.Fatal(err)
	}
	return st, clk, lh
}

func writeTT(t *testing.T, st *store.Store, lh *store.Item, name, mode string, tbl *Table) {
	t.Helper()
	if err := WriteDeltaTableAs(store.Attribution{}, st, lh.WorkspaceID, lh.ID, name, mode, tbl); err != nil {
		t.Fatalf("writing %q: %v", name, err)
	}
}

// TestTimeTravelResolverTracksSchemaEvolution: a table written once, then
// rewritten with an extra column, must answer an AS-OF request from before
// the column existed with the OLD schema, and CurrentColumns with today's —
// exactly the gap checkColumnsExistedAsOf (package tsql) compares against
// (docs/35, Class B).
//
// The table is deliberately named with mixed case ("Customer") so a
// resolver that lower-cased it internally would fail to find it — OneLake
// paths are case-sensitive, and this pinned a real bug caught while building
// Phase 3's wiring in package tsql.
func TestTimeTravelResolverTracksSchemaEvolution(t *testing.T) {
	st, clk, lh := setupTimeTravelStore(t)

	v0 := &Table{Columns: []string{"id", "name"}, Rows: [][]any{
		{int64(1), "Alice"}, {int64(2), "Bob"},
	}}
	writeTT(t, st, lh, "Customer", WriteOverwrite, v0)
	tsV0 := time.Unix(clk.Now(), 0)
	clk.Advance(3600)

	v1 := &Table{Columns: []string{"id", "name", "email"}, Rows: [][]any{
		{int64(1), "Alice", "alice@example.com"},
	}}
	writeTT(t, st, lh, "Customer", WriteOverwrite, v1)
	tsV1 := time.Unix(clk.Now(), 0)

	resolve := TimeTravelResolver(st, lh.ID)

	t.Run("as of v0: the old schema, but today's CurrentColumns", func(t *testing.T) {
		snap, ok, err := resolve("Customer", tsV0)
		if err != nil || !ok {
			t.Fatalf("resolve = (ok=%v, err=%v), want ok=true, err=nil", ok, err)
		}
		if got, want := snap.Columns, []string{"id", "name"}; !equalStrings(got, want) {
			t.Errorf("Columns = %v, want %v", got, want)
		}
		if got, want := snap.CurrentColumns, []string{"id", "name", "email"}; !equalStrings(got, want) {
			t.Errorf("CurrentColumns = %v, want %v", got, want)
		}
		if len(snap.SQLTypes) != len(snap.Columns) {
			t.Fatalf("SQLTypes = %v, want one per column in %v", snap.SQLTypes, snap.Columns)
		}
		if snap.SQLTypes[0] != "BIGINT" || snap.SQLTypes[1] != varcharType {
			t.Errorf("SQLTypes = %v, want [BIGINT %s]", snap.SQLTypes, varcharType)
		}
		if len(snap.RowLiterals) != 2 {
			t.Fatalf("RowLiterals has %d rows, want 2", len(snap.RowLiterals))
		}
		if got, want := snap.RowLiterals[0], []string{"1", "N'Alice'"}; !equalStrings(got, want) {
			t.Errorf("row 0 = %v, want %v", got, want)
		}
		if got, want := snap.RowLiterals[1], []string{"2", "N'Bob'"}; !equalStrings(got, want) {
			t.Errorf("row 1 = %v, want %v", got, want)
		}
	})

	t.Run("as of v1: email now exists, and matches CurrentColumns", func(t *testing.T) {
		snap, ok, err := resolve("Customer", tsV1)
		if err != nil || !ok {
			t.Fatalf("resolve = (ok=%v, err=%v), want ok=true, err=nil", ok, err)
		}
		if got, want := snap.Columns, []string{"id", "name", "email"}; !equalStrings(got, want) {
			t.Errorf("Columns = %v, want %v", got, want)
		}
		if got, want := snap.Columns, snap.CurrentColumns; !equalStrings(got, want) {
			t.Errorf("Columns = %v, CurrentColumns = %v, want equal (no hint beats no hint)", got, want)
		}
	})

	t.Run("case preserved: lower-casing the name must not hide the table", func(t *testing.T) {
		if _, ok, err := resolve("customer", tsV0); err != nil || !ok {
			t.Fatalf("resolve(%q) = (ok=%v, err=%v), want ok=true (SQL Server identifiers are "+
				"case-insensitive even though the OneLake path beneath them is not — "+
				"the resolver is handed whatever case the statement wrote)", "customer", ok, err)
		}
	})
}

// TestTimeTravelResolverUnknownTable: a name that is not a Delta table in
// this item at all — a system view, an unrelated object — answers ok=false,
// not an error, so Adapt leaves the reference untouched (docs/35).
func TestTimeTravelResolverUnknownTable(t *testing.T) {
	st, _, lh := setupTimeTravelStore(t)
	resolve := TimeTravelResolver(st, lh.ID)
	snap, ok, err := resolve("NoSuchTable", time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatalf("ok = true, want false (snap = %+v)", snap)
	}
}

// TestTimeTravelResolverBeforeFirstCommit: a real Delta table, but a
// timestamp before it existed — a genuine failure, not "nothing to resolve",
// the same distinction ReadDeltaTableAsOf itself draws (Phase 1).
func TestTimeTravelResolverBeforeFirstCommit(t *testing.T) {
	st, clk, lh := setupTimeTravelStore(t)
	writeTT(t, st, lh, "Customer", WriteOverwrite, &Table{
		Columns: []string{"id"}, Rows: [][]any{{int64(1)}},
	})
	before := time.Unix(clk.Now()-10, 0)

	resolve := TimeTravelResolver(st, lh.ID)
	if _, ok, err := resolve("Customer", before); err == nil {
		t.Fatal("want an error for a timestamp before the table's first commit")
	} else if ok {
		t.Error("ok = true alongside a non-nil error")
	} else if !strings.Contains(err.Error(), "Customer") {
		t.Errorf("error should name the table: %v", err)
	}
}

// TestTimeTravelResolverComposesWithAdaptWithTimeTravel is the integration
// proof: a resolver built from REAL Delta history, handed to package tsql's
// AdaptWithTimeTravel, produces a statement that materialises the right rows
// under the right #temp table — the full Phase 3 path minus the TDS wire and
// the engine actually running the result.
func TestTimeTravelResolverComposesWithAdaptWithTimeTravel(t *testing.T) {
	st, clk, lh := setupTimeTravelStore(t)
	writeTT(t, st, lh, "Orders", WriteOverwrite, &Table{
		Columns: []string{"id", "amount"},
		Rows:    [][]any{{int64(1), float64(9.5)}},
	})
	asOf := time.Unix(clk.Now(), 0)

	resolve := TimeTravelResolver(st, lh.ID)
	sql := "SELECT id, amount FROM dbo.Orders OPTION (FOR TIMESTAMP AS OF '" +
		asOf.UTC().Format("2006-01-02T15:04:05.000") + "')"
	out, changed, err := tsql.AdaptWithTimeTravel(sql, resolve)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !changed {
		t.Fatal("want changed = true")
	}
	for _, want := range []string{
		"CREATE TABLE #tt0", "INSERT INTO #tt0 VALUES (1, 9.5);", "FROM #tt0 AS Orders",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "OPTION") {
		t.Errorf("hint should be stripped:\n%s", out)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// --- sqlLiteral ---------------------------------------------------------

func TestSQLLiteral(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"nil", nil, "NULL"},
		{"true", true, "1"},
		{"false", false, "0"},
		{"int16", int16(5), "5"},
		{"int32", int32(-7), "-7"},
		{"int64", int64(9), "9"},
		{"float32", float32(1.5), "1.5"},
		{"float64", float64(2.25), "2.25"},
		{"decimal", Decimal{Unscaled: big.NewInt(150), Precision: 10, Scale: 2}, "1.50"},
		{"date", Date{T: time.Date(2024, 3, 13, 0, 0, 0, 0, time.UTC)}, "'2024-03-13'"},
		{"bytes", []byte{0xAB, 0xCD}, "0xabcd"},
		{"string", "O'Brien", "N'O''Brien'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sqlLiteral(tc.in); got != tc.want {
				t.Errorf("sqlLiteral(%#v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
