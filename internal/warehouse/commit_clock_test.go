package warehouse

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/clock"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// commitActions decodes a _delta_log commit into its NDJSON actions, keyed by
// the single action name each line carries.
func commitActions(t *testing.T, st *store.Store, itemID, name string, version string) map[string]map[string]any {
	t.Helper()
	p, err := st.GetOneLakePath(itemID, "Tables/"+name+"/_delta_log/"+version+".json")
	if err != nil {
		t.Fatalf("commit %s of %q: %v", version, name, err)
	}
	out := map[string]map[string]any{}
	for _, line := range bytes.Split(p.Content, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var action map[string]map[string]any
		if err := json.Unmarshal(line, &action); err != nil {
			t.Fatalf("commit %s line %q: %v", version, line, err)
		}
		for k, v := range action {
			out[k] = v
		}
	}
	return out
}

// millis reads a JSON number field as epoch milliseconds. json.Unmarshal into
// `any` gives a float64, which is exact well past any timestamp we stamp.
func millis(t *testing.T, action map[string]any, field string) int64 {
	t.Helper()
	v, ok := action[field]
	if !ok {
		t.Fatalf("action %v has no %q", action, field)
	}
	n, ok := v.(float64)
	if !ok {
		t.Fatalf("%q = %#v, want a number", field, v)
	}
	return int64(n)
}

// TestDeltaCommitsStampedFromEmulatorClock: every timestamp in a Delta commit
// must come from the emulator clock, not the wall clock.
//
// This is the prerequisite for FOR TIMESTAMP AS OF (docs/35): time travel
// replays the log against the clock a test controls, so a commit stamped from
// real time sits outside every window that test can name — on a frozen clock,
// in the future forever. It is also what makes a commit's own time assertable
// at all, which is what this test does.
func TestDeltaCommitsStampedFromEmulatorClock(t *testing.T) {
	clk := clock.New()
	clk.Freeze()
	clk.Advance(3600 * 24 * 400) // well clear of real "now", so a wall-clock stamp cannot pass by luck
	st, err := store.Open("", clk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ws := &store.Workspace{ID: "ws-clk", DisplayName: "w", Type: "Workspace"}
	if err := st.CreateWorkspace(ws, store.Principal{ID: "p", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	lh := &store.Item{ID: "it-clk", WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lake"}
	if err := st.CreateItem(lh, nil); err != nil {
		t.Fatal(err)
	}

	tbl := &Table{Columns: []string{"id"}, Rows: [][]any{{int64(1)}}}
	if err := WriteDeltaTableAs(store.Attribution{}, st, ws.ID, lh.ID, "orders", WriteOverwrite, tbl); err != nil {
		t.Fatalf("create: %v", err)
	}

	wantFirst := clk.Now() * 1000
	first := commitActions(t, st, lh.ID, "orders", "00000000000000000000")
	info, ok := first["commitInfo"]
	if !ok {
		t.Fatalf("commit 0 has no commitInfo action: %v", first)
	}
	if got := millis(t, info, "timestamp"); got != wantFirst {
		t.Errorf("commitInfo.timestamp = %d, want the emulator clock's %d", got, wantFirst)
	}
	if got, want := info["operation"], "WRITE"; got != want {
		t.Errorf("commitInfo.operation = %v, want %q", got, want)
	}
	if got := millis(t, first["add"], "modificationTime"); got != wantFirst {
		t.Errorf("add.modificationTime = %d, want %d", got, wantFirst)
	}
	if got := millis(t, first["metaData"], "createdTime"); got != wantFirst {
		t.Errorf("metaData.createdTime = %d, want %d", got, wantFirst)
	}

	// An hour of emulator time, no real time at all: the next commit must move
	// by exactly that, which only a clock-derived stamp can do.
	clk.Advance(3600)
	if err := WriteDeltaTableAs(store.Attribution{}, st, ws.ID, lh.ID, "orders", WriteOverwrite, tbl); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	wantSecond := wantFirst + 3600*1000
	second := commitActions(t, st, lh.ID, "orders", "00000000000000000001")
	if got := millis(t, second["commitInfo"], "timestamp"); got != wantSecond {
		t.Errorf("second commitInfo.timestamp = %d, want %d (one emulator hour later)", got, wantSecond)
	}
	if got := millis(t, second["add"], "modificationTime"); got != wantSecond {
		t.Errorf("second add.modificationTime = %d, want %d", got, wantSecond)
	}
	// The overwrite retires commit 0's file, and the tombstone is stamped from
	// the same clock — a reader replaying to an instant between the two commits
	// depends on it.
	rm, ok := second["remove"]
	if !ok {
		t.Fatalf("an overwrite must remove the previous file: %v", second)
	}
	if got := millis(t, rm, "deletionTimestamp"); got != wantSecond {
		t.Errorf("remove.deletionTimestamp = %d, want %d", got, wantSecond)
	}
}
