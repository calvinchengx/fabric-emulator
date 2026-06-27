package warehouse

import (
	"strings"
	"testing"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/clock"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// asOfIDs reads the table as of ts and returns its id column, so an assertion
// can name the rows rather than only count them — a version with the right
// number of the wrong rows is the failure worth catching.
func asOfIDs(t *testing.T, st *store.Store, itemID, name string, ts time.Time) []int64 {
	t.Helper()
	tbl, err := ReadDeltaTableAsOf(st, itemID, name, ts)
	if err != nil {
		t.Fatalf("as of %s: %v", ts.UTC().Format(time.RFC3339), err)
	}
	return ids(t, tbl)
}

// ids pulls the single "id" column out of a Table.
func ids(t *testing.T, tbl *Table) []int64 {
	t.Helper()
	if len(tbl.Columns) != 1 || tbl.Columns[0] != "id" {
		t.Fatalf("columns = %v, want [id]", tbl.Columns)
	}
	out := make([]int64, 0, len(tbl.Rows))
	for _, row := range tbl.Rows {
		n, ok := row[0].(int64)
		if !ok {
			t.Fatalf("id = %#v (%T), want int64", row[0], row[0])
		}
		out = append(out, n)
	}
	return out
}

func sameIDs(got, want []int64) bool {
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

// TestReadDeltaTableAsOf: replaying the log through the last commit at or
// before an instant returns the table as it stood then (docs/35 Phase 1).
//
// Three commits an emulator hour apart — create, append, overwrite — and the
// interesting instants are the MIDPOINTS as much as the commit times: a reader
// that stopped one commit too late or too early would still pass at the exact
// commit times under an off-by-one comparison, and the overwrite is what proves
// the removed file is genuinely excluded rather than merely absent.
func TestReadDeltaTableAsOf(t *testing.T) {
	clk := clock.New()
	clk.Freeze()
	clk.Advance(3600 * 24 * 400) // well clear of real "now"
	st, err := store.Open("", clk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ws := &store.Workspace{ID: "ws-asof", DisplayName: "w", Type: "Workspace"}
	if err := st.CreateWorkspace(ws, store.Principal{ID: "p", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	lh := &store.Item{ID: "it-asof", WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lake"}
	if err := st.CreateItem(lh, nil); err != nil {
		t.Fatal(err)
	}

	rows := func(vals ...int64) *Table {
		tbl := &Table{Columns: []string{"id"}}
		for _, v := range vals {
			tbl.Rows = append(tbl.Rows, []any{v})
		}
		return tbl
	}
	write := func(mode string, tbl *Table) {
		t.Helper()
		if err := WriteDeltaTableAs(store.Attribution{}, st, ws.ID, lh.ID, "orders", mode, tbl); err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
	}
	at := func(sec int64) time.Time { return time.Unix(sec, 0) }

	write(WriteOverwrite, rows(1, 2))
	v0 := clk.Now()
	clk.Advance(3600)
	write(WriteAppend, rows(3))
	v1 := clk.Now()
	clk.Advance(3600)
	write(WriteOverwrite, rows(9))
	v2 := clk.Now()

	cases := []struct {
		name string
		when time.Time
		want []int64
	}{
		{"at v0", at(v0), []int64{1, 2}},
		{"between v0 and v1", at(v0 + 1800), []int64{1, 2}},
		{"one millisecond before v1", time.UnixMilli(v1*1000 - 1), []int64{1, 2}},
		{"at v1", at(v1), []int64{1, 2, 3}},
		{"between v1 and v2", at(v1 + 1800), []int64{1, 2, 3}},
		{"at v2", at(v2), []int64{9}},
		{"long after v2", at(v2 + 86400), []int64{9}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := asOfIDs(t, st, lh.ID, "orders", tc.when); !sameIDs(got, tc.want) {
				t.Errorf("ids = %v, want %v", got, tc.want)
			}
		})
	}

	// "As of now" is the ordinary read: time travel to the present must not be
	// a different answer from no time travel at all.
	live, err := ReadDeltaTable(st, lh.ID, "orders")
	if err != nil {
		t.Fatalf("live read: %v", err)
	}
	if got, want := asOfIDs(t, st, lh.ID, "orders", at(clk.Now())), ids(t, live); !sameIDs(got, want) {
		t.Errorf("as of now = %v, want ReadDeltaTable's %v", got, want)
	}

	// Before the table existed: an error, not an empty table. Answering "no
	// rows" here is a plausible wrong answer, which is the failure mode time
	// travel is supposed to prevent.
	before := at(v0 - 1)
	if tbl, err := ReadDeltaTableAsOf(st, lh.ID, "orders", before); err == nil {
		t.Fatalf("read before the first commit returned %d rows, want an error", len(tbl.Rows))
	} else if !strings.Contains(err.Error(), "orders") {
		t.Errorf("error %q should name the table", err)
	}
}
