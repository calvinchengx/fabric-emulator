package api

// A Copy Job must COPY — and refuse by name what it cannot copy. These tests
// drive the documented surface end to end: the copyjob-content.json shapes are
// Microsoft's own examples (copyjob-definition Examples 1 and 2, the
// capabilities article's Batch sample), and run-on-demand uses the documented
// jobType=Execute. The negative cases matter as much as the positive one: a
// CopyJob that "completed" while silently skipping a CDC mode or an external
// leg is the false-green shape this repo keeps paying for.

import (
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/clock"
	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/calvinchengx/fabric-emulator/internal/warehouse"
)

func createCopyJob(t *testing.T, st *store.Store, wid, contentJSON string) *store.Item {
	t.Helper()
	payload := base64.StdEncoding.EncodeToString([]byte(contentJSON))
	it := &store.Item{WorkspaceID: wid, Type: "CopyJob",
		DisplayName: fmt.Sprintf("cj-%d", pipelineSeq.Add(1))}
	parts := []store.DefinitionPart{{Path: "copyjob-content.json", Payload: payload, PayloadType: "InlineBase64"}}
	if err := st.CreateItem(it, parts); err != nil {
		t.Fatal(err)
	}
	return it
}

// lakehouseBatchDef is Example 2's shape: LakehouseTable → LakehouseTable,
// one activity, table orders → bronze_orders.
func lakehouseBatchDef(wsID, srcID, dstID, writeBehavior string) string {
	wb := ""
	if writeBehavior != "" {
		wb = `"writeBehavior":"` + writeBehavior + `",`
	}
	return `{
	  "properties": {
	    "jobMode": "Batch",
	    "source": {"type": "LakehouseTable", "connectionSettings": {"type": "Lakehouse",
	      "typeProperties": {"workspaceId": "` + wsID + `", "artifactId": "` + srcID + `", "rootFolder": "Tables"}}},
	    "destination": {"type": "LakehouseTable", "connectionSettings": {"type": "Lakehouse",
	      "typeProperties": {"workspaceId": "` + wsID + `", "artifactId": "` + dstID + `", "rootFolder": "Tables"}}},
	    "policy": {"timeout": "0.12:00:00"}
	  },
	  "activities": [{"id": "aaaaaaaa-1111-2222-3333-bbbbbbbbbbbb", "properties": {
	    "source": {"datasetSettings": {"table": "orders"}},
	    "destination": {` + wb + `"datasetSettings": {"table": "bronze_orders"}},
	    "translator": {"type": "TabularTranslator"},
	    "typeConversionSettings": {"typeConversion": {"allowDataTruncation": true}}
	  }}]
	}`
}

func TestCopyJobExecuteReallyCopies(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	src := seedLakehouse(t, st, ws.ID, "src")
	dst := seedLakehouse(t, st, ws.ID, "dst")
	payload := []byte("orders bytes")
	seedFile(t, st, ws.ID, src.ID, "Tables/orders/part-0.parquet", payload)

	cj := createCopyJob(t, st, ws.ID, lakehouseBatchDef(ws.ID, src.ID, dst.ID, ""))
	_, jid := runJob(t, a, ws.ID, cj.ID, "jobType=Execute", "{}")
	if s := awaitJob(t, a, ws.ID, cj.ID, jid); s != "Completed" {
		t.Fatalf("job status = %s", s)
	}
	got, err := st.GetOneLakePath(dst.ID, "Tables/bronze_orders/part-0.parquet")
	if err != nil {
		t.Fatalf("destination table missing — the Copy Job did not copy: %v", err)
	}
	if string(got.Content) != string(payload) {
		t.Fatalf("destination content = %q", got.Content)
	}
	// The edge must exist AND say Copy: the emulator watched these bytes move.
	edges, err := st.ListLineageEdges(ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range edges {
		if e.JobID == jid && e.Producer == store.ProducerCopy &&
			e.SourceItemID == src.ID && e.TargetItemID == dst.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("no Copy lineage edge for the job; edges = %+v", edges)
	}
}

func TestCopyJobDispatchesOnBothDocumentedJobTypeSpellings(t *testing.T) {
	// The capabilities article POSTs jobType=Execute and reads back
	// jobType "CopyJob" — Microsoft's asymmetry, so both must dispatch.
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	src := seedLakehouse(t, st, ws.ID, "src")
	dst := seedLakehouse(t, st, ws.ID, "dst")
	seedFile(t, st, ws.ID, src.ID, "Tables/orders/part-0.parquet", []byte("x"))
	cj := createCopyJob(t, st, ws.ID, lakehouseBatchDef(ws.ID, src.ID, dst.ID, ""))
	_, jid := runJob(t, a, ws.ID, cj.ID, "jobType=CopyJob", "{}")
	if s := awaitJob(t, a, ws.ID, cj.ID, jid); s != "Completed" {
		t.Fatalf("jobType=CopyJob status = %s", s)
	}
}

func TestCopyJobEmptyDefinitionCompletesEmpty(t *testing.T) {
	// Microsoft's minimal Example 1: {"jobMode":"Batch","activities":[]} —
	// nothing to copy is a successful copy of nothing, not an error.
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	cj := createCopyJob(t, st, ws.ID, `{"properties":{"jobMode":"Batch"},"activities":[]}`)
	_, jid := runJob(t, a, ws.ID, cj.ID, "jobType=Execute", "{}")
	if s := awaitJob(t, a, ws.ID, cj.ID, jid); s != "Completed" {
		t.Fatalf("empty definition status = %s", s)
	}
}

func TestCopyJobRefusesWhatItCannotHonour(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	src := seedLakehouse(t, st, ws.ID, "src")
	dst := seedLakehouse(t, st, ws.ID, "dst")
	seedFile(t, st, ws.ID, src.ID, "Tables/orders/part-0.parquet", []byte("x"))

	external := `{
	  "properties": {"jobMode": "Batch",
	    "source": {"type": "AzureSqlTable", "connectionSettings": {"type": "AzureSqlDatabase",
	      "typeProperties": {"database": "salesdb"}, "externalReferences": {"connection": "00000000-0000-0000-0000-000000000000"}}},
	    "destination": {"type": "LakehouseTable", "connectionSettings": {"type": "Lakehouse",
	      "typeProperties": {"workspaceId": "` + ws.ID + `", "artifactId": "` + dst.ID + `"}}}},
	  "activities": [{"properties": {
	    "source": {"datasetSettings": {"schema": "dbo", "table": "Customers"}},
	    "destination": {"datasetSettings": {"table": "Customers"}}}}]
	}`

	externalDst := `{
	  "properties": {"jobMode": "Batch",
	    "source": {"type": "LakehouseTable", "connectionSettings": {"type": "Lakehouse",
	      "typeProperties": {"workspaceId": "` + ws.ID + `", "artifactId": "` + src.ID + `"}}},
	    "destination": {"type": "AzureSqlTable", "connectionSettings": {"type": "AzureSqlDatabase",
	      "typeProperties": {"database": "salesdb"}, "externalReferences": {"connection": "00000000-0000-0000-0000-000000000000"}}}},
	  "activities": [{"properties": {
	    "source": {"datasetSettings": {"table": "orders"}},
	    "destination": {"datasetSettings": {"schema": "dbo", "table": "orders"}}}}]
	}`
	// Both sides external: source must be named, EVERY run. The first version
	// ranged a map here, so which side got reported was up to Go's iteration
	// order — a test asserting either side would have been a genuine flake.
	bothExternal := `{
	  "properties": {"jobMode": "Batch",
	    "source": {"type": "AzureSqlTable", "connectionSettings": {"type": "AzureSqlDatabase",
	      "typeProperties": {"database": "a"}, "externalReferences": {"connection": "00000000-0000-0000-0000-000000000000"}}},
	    "destination": {"type": "AzureSqlTable", "connectionSettings": {"type": "AzureSqlDatabase",
	      "typeProperties": {"database": "b"}, "externalReferences": {"connection": "00000000-0000-0000-0000-000000000000"}}}},
	  "activities": [{"properties": {
	    "source": {"datasetSettings": {"table": "t"}},
	    "destination": {"datasetSettings": {"table": "t"}}}}]
	}`

	cases := []struct {
		name, def, wantCode string
	}{
		{"CDC mode", `{"properties":{"jobMode":"CDC"},"activities":[]}`, "CopyJobCDCNotImplemented"},
		{"external source", external, "CopyJobExternalSourceNotSupported"},
		{"external destination", externalDst, "CopyJobExternalDestinationNotSupported"},
		{"both external names source first", bothExternal, "CopyJobExternalSourceNotSupported"},
		{"merge writeBehavior", lakehouseBatchDef(ws.ID, src.ID, dst.ID, "Merge"), "CopyJobWriteBehaviorNotSupported"},
		{"garbage content", `{not json`, "CopyJobDefinitionInvalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cj := createCopyJob(t, st, ws.ID, tc.def)
			_, jid := runJob(t, a, ws.ID, cj.ID, "jobType=Execute", "{}")
			if s := awaitJob(t, a, ws.ID, cj.ID, jid); s != "Failed" {
				t.Fatalf("status = %s, want Failed", s)
			}
			j, err := a.Store.GetJobInstance(cj.ID, jid)
			if err != nil {
				t.Fatal(err)
			}
			if j.FailWith != tc.wantCode {
				t.Fatalf("failure code = %q, want %q", j.FailWith, tc.wantCode)
			}
			// A refusal must not half-copy: nothing may have landed.
			if tc.name == "merge writeBehavior" {
				if _, err := st.GetOneLakePath(dst.ID, "Tables/bronze_orders/part-0.parquet"); err == nil {
					t.Fatal("a refused writeBehavior still copied bytes")
				}
			}
		})
	}
}

func TestCopyJobDeltaSourceMakesRealCommits(t *testing.T) {
	// The parity row claims real Delta semantics for Tables/ sinks — Append
	// appends, Overwrite replaces — so the witness must drive the Delta path,
	// not only the byte-copy fallback the bare-parquet test exercises. A
	// CopyJob that parsed, dispatched and returned Completed while copying
	// nothing would pass every status assertion; the row counts are the claim.
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	src := seedLakehouse(t, st, ws.ID, "src")
	dst := seedLakehouse(t, st, ws.ID, "dst")
	seedDeltaTable(t, st, ws.ID, src.ID, "orders", []lookupRow{{"us", 80}, {"eu", 60}})

	rowsAt := func(table string) int {
		t.Helper()
		tbl, err := warehouse.ReadDeltaTable(st, dst.ID, table)
		if err != nil {
			t.Fatalf("destination %s not a readable Delta table: %v", table, err)
		}
		return len(tbl.Rows)
	}

	// Overwrite (the default): destination holds exactly the source's rows.
	cj := createCopyJob(t, st, ws.ID, lakehouseBatchDef(ws.ID, src.ID, dst.ID, "Overwrite"))
	_, jid := runJob(t, a, ws.ID, cj.ID, "jobType=Execute", "{}")
	if s := awaitJob(t, a, ws.ID, cj.ID, jid); s != "Completed" {
		t.Fatalf("overwrite status = %s", s)
	}
	if n := rowsAt("bronze_orders"); n != 2 {
		t.Fatalf("after overwrite: %d rows, want 2", n)
	}

	// Append, twice: each run adds the source's rows — 2, then 4, then 6.
	// Re-running an Overwrite job would also read 2; only growth proves the
	// Append leg is honoured through the CopyJob door.
	app := createCopyJob(t, st, ws.ID, lakehouseBatchDef(ws.ID, src.ID, dst.ID, "Append"))
	for i, want := range []int{4, 6} {
		_, jid := runJob(t, a, ws.ID, app.ID, "jobType=Execute", "{}")
		if s := awaitJob(t, a, ws.ID, app.ID, jid); s != "Completed" {
			t.Fatalf("append run %d status = %s", i, s)
		}
		if n := rowsAt("bronze_orders"); n != want {
			t.Fatalf("after append run %d: %d rows, want %d", i, n, want)
		}
	}
}

// TestCopyJobTerminalEventArrivesExactlyOnceAfterTheCopy: the terminal event
// must describe a copy that HAPPENED.
//
// This test was `…PublishesItsTerminalEventAtStart` and passed — accurately,
// while dispatch was synchronous. Async makes settle-at-start the bug rather
// than the contract, so a passing test whose NAME asserts the old behaviour
// would be a comment stating an invariant the code no longer holds: the exact
// defect class this file's neighbours were written to catch. Renamed, not
// deleted, because the site it pins (terminalStatusOf, the quiet second switch)
// still needs pinning — a mutation restoring CopyJob to executesNow fails the
// COUNT below with [Completed Completed], and would pass a presence check.
func TestCopyJobTerminalEventArrivesExactlyOnceAfterTheCopy(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	src := seedLakehouse(t, st, ws.ID, "src")
	dst := seedLakehouse(t, st, ws.ID, "dst")
	seedFile(t, st, ws.ID, src.ID, "Tables/orders/part-0.parquet", []byte("x"))

	terminals := func(jid string, evs []store.Event) []string {
		t.Helper()
		var out []string
		for _, ev := range evs {
			if ev.Kind == store.KindJob && ev.JobID == jid &&
				(ev.Status == store.JobCompleted || ev.Status == store.JobFailed) {
				out = append(out, ev.Status)
			}
		}
		return out
	}

	// Subscribe BEFORE the POST and drain after: Replay() races the async
	// dispatcher, which is measured rather than theoretical.
	sub := st.Subscribe()
	defer sub.Close()
	cj := createCopyJob(t, st, ws.ID, lakehouseBatchDef(ws.ID, src.ID, dst.ID, ""))
	_, jid := runJob(t, a, ws.ID, cj.ID, "jobType=Execute", "{}")
	if s := awaitJob(t, a, ws.ID, cj.ID, jid); s != "Completed" {
		t.Fatalf("job = %s", s)
	}
	if got := terminals(jid, drainEvents(t, sub.C)); len(got) != 1 || got[0] != store.JobCompleted {
		t.Fatalf("terminal events = %v, want exactly one Completed", got)
	}

	// A refusal is terminal too, and must also arrive exactly once — the
	// goroutine finalises with the refusal code rather than the copy's.
	sub2 := st.Subscribe()
	defer sub2.Close()
	bad := createCopyJob(t, st, ws.ID, `{"properties":{"jobMode":"CDC"},"activities":[]}`)
	_, jid = runJob(t, a, ws.ID, bad.ID, "jobType=Execute", "{}")
	if s := awaitJob(t, a, ws.ID, bad.ID, jid); s != "Failed" {
		t.Fatalf("refused job = %s", s)
	}
	if got := terminals(jid, drainEvents(t, sub2.C)); len(got) != 1 || got[0] != store.JobFailed {
		t.Fatalf("terminal events = %v, want exactly one Failed", got)
	}
}

// TestCopyJobStatusReflectsTheCopyNotTheClock: a copy that has DEMONSTRABLY
// finished must not report InProgress.
//
// This is the witness for the async migration, and it exists because the
// obvious assertions cannot see the change: parked clock, exactly-once
// terminal, correct verdict — all of those already hold under the synchronous
// code, so they witness what worked before rather than what changed.
//
// The seam is the virtual clock. `StatusAt` derives a job's wire status purely
// from `now < CompleteAt`, and synchronous dispatch leaves CompleteAt at the
// clock-derived `Now()+lroDelay`. So with a delay configured, the emulator ran
// the copy INLINE during the POST — the bytes are at the destination, provably
// — and then reported InProgress for the next hour of virtual time. Status that
// contradicts the filesystem is the same class as a notebook reporting
// Completed with cells outstanding, inverted: there, green with nothing done;
// here, pending with everything done.
//
// Async dispatch fixes it because the goroutine calls FinalizeJob, which sets
// complete_at = Now() — the status follows the work instead of the clock. The
// margin is deliberate: a 1-hour delay against awaitJob's 5-second deadline, so
// this fails on the old behaviour by a factor of 720 rather than by a race.
func TestCopyJobStatusReflectsTheCopyNotTheClock(t *testing.T) {
	st, err := store.Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	a := New(st, nil, 1, 3600) // an hour of LRO delay: the clock cannot finish this job

	ws := seedWorkspace(t, st)
	src := seedLakehouse(t, st, ws.ID, "src")
	dst := seedLakehouse(t, st, ws.ID, "dst")
	payload := []byte("orders bytes")
	seedFile(t, st, ws.ID, src.ID, "Tables/orders/part-0.parquet", payload)

	cj := createCopyJob(t, st, ws.ID, lakehouseBatchDef(ws.ID, src.ID, dst.ID, ""))
	_, jid := runJob(t, a, ws.ID, cj.ID, "jobType=Execute", "{}")

	if s := awaitJob(t, a, ws.ID, cj.ID, jid); s != "Completed" {
		t.Fatalf("job = %s; a finished copy must not wait on the clock", s)
	}
	// And the copy really did happen — otherwise "Completed" would be the
	// other failure this repo keeps finding.
	got, err := st.GetOneLakePath(dst.ID, "Tables/bronze_orders/part-0.parquet")
	if err != nil {
		t.Fatalf("job reported Completed but the destination is empty: %v", err)
	}
	if string(got.Content) != string(payload) {
		t.Fatalf("destination content = %q", got.Content)
	}
}
