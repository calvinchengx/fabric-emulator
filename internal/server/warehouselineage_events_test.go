package server

import (
	"testing"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/calvinchengx/fabric-emulator/internal/tsql"
)

// A warehouse write must ANNOUNCE ITSELF, not only be recorded.
//
// The flow view lights a node on a `table` event, and until this the only
// emitter was a Delta commit in OneLake. A warehouse is written over TDS, so a
// dbt-built gold layer appeared in the graph — correctly connected, by edges
// this same observer records — and stayed grey for an entire run. Grey then
// meant two different things: "nothing was written here" and "this path is not
// instrumented", with nothing on screen to tell them apart.

// tableEvents drains the subscription and keeps the table events.
func tableEvents(t *testing.T, sub *store.Subscription) []store.Event {
	t.Helper()
	var out []store.Event
	for {
		select {
		case ev := <-sub.C:
			if ev.Kind == store.KindTable {
				out = append(out, ev)
			}
		case <-time.After(200 * time.Millisecond):
			return out
		}
	}
}

func TestWarehouseWriteAnnouncesTheTable(t *testing.T) {
	f := newLineageFixture(t)
	sub := f.st.Subscribe()
	defer sub.Close()

	f.observe(`CREATE TABLE dbo.fct_sales AS SELECT * FROM dbo.silver_orders`)

	got := tableEvents(t, sub)
	if len(got) != 1 {
		t.Fatalf("got %d table events, want 1: %+v", len(got), got)
	}
	if got[0].Table != "Tables/fct_sales" {
		t.Errorf("table = %q, want Tables/fct_sales", got[0].Table)
	}
	if got[0].ItemID != f.wh.ID {
		t.Errorf("itemId = %q, want the warehouse", got[0].ItemID)
	}
	// The statement kind travels so the log can say what happened, matching
	// what the lineage edge for the same write records.
	if got[0].ActivityName == "" {
		t.Error("no activity name: the log cannot say what kind of write this was")
	}
	// A warehouse has no Delta version. Inventing one would make a T-SQL write
	// indistinguishable from a commit that really carried a version number.
	if got[0].Version != nil {
		t.Errorf("version = %v, want nil for a warehouse write", *got[0].Version)
	}
}

func TestWarehouseWriteWithSeveralSourcesAnnouncesOnce(t *testing.T) {
	// One statement is one write. Publishing per edge would report a table
	// rebuilt from three sources three times, and the log is read by people.
	f := newLineageFixture(t)
	sub := f.st.Subscribe()
	defer sub.Close()

	f.observe(`CREATE TABLE dbo.fct_sales AS
	           SELECT * FROM dbo.silver_orders
	           JOIN dbo.silver_customers ON 1=1
	           JOIN dbo.silver_party ON 1=1`)

	if got := tableEvents(t, sub); len(got) != 1 {
		t.Fatalf("got %d table events for one statement, want 1: %+v", len(got), got)
	}
}

func TestStatementsThatWriteNoTableAnnounceNothing(t *testing.T) {
	// A view holds no bytes and a drop removes them; neither is a write, and a
	// node lighting up for either would be a lie in the direction that matters
	// most — the graph claiming data moved when none did.
	f := newLineageFixture(t)
	sub := f.st.Subscribe()
	defer sub.Close()

	f.observe(`CREATE VIEW dbo.v AS SELECT * FROM dbo.silver_orders`)
	f.observe(`DROP TABLE dbo.fct_sales`)

	if got := tableEvents(t, sub); len(got) != 0 {
		t.Fatalf("got %d table events, want none: %+v", len(got), got)
	}
}

func TestAnUnresolvableTargetAnnouncesNothing(t *testing.T) {
	// `resolve` refuses to guess which item a name belongs to. When it cannot,
	// there is no node to light, and announcing anyway would put an event on the
	// bus that names a table the graph does not have.
	f := newLineageFixture(t)
	sub := f.st.Subscribe()
	defer sub.Close()

	f.wl.observe("no-such-database", tsql.DataFlows(
		`CREATE TABLE dbo.fct_sales AS SELECT * FROM dbo.silver_orders`))

	if got := tableEvents(t, sub); len(got) != 0 {
		t.Fatalf("got %d table events for an unresolvable target, want none: %+v", len(got), got)
	}
}

// TestDbtBuildAndSwapAnnouncesTheRealTable replays dbt-fabric's actual shape —
// build into `<model>__dbt_temp`, then sp_rename into place.
//
// THIS IS THE CASE THE FIRST VERSION MISSED. Announcing only the CTAS was green
// in unit tests that wrote straight to the final name, and lit nothing at all
// against a real dbt build: every gold table emitted an event named
// `..._dbt_temp`, which is not a node the graph draws, so all nine stayed grey.
func TestDbtBuildAndSwapAnnouncesTheRealTable(t *testing.T) {
	f := newLineageFixture(t)
	sub := f.st.Subscribe()
	defer sub.Close()

	f.observe(`CREATE TABLE dbo.fct_sales__dbt_temp AS SELECT * FROM dbo.silver_orders`)
	f.observe(`EXEC sp_rename 'dbo.fct_sales__dbt_temp', 'fct_sales'`)

	var names []string
	for _, ev := range tableEvents(t, sub) {
		names = append(names, ev.Table)
	}
	found := false
	for _, n := range names {
		if n == "Tables/fct_sales" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no event named the real table; got %v", names)
	}
}
