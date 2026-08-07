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

	cases := []struct {
		name, def, wantCode string
	}{
		{"CDC mode", `{"properties":{"jobMode":"CDC"},"activities":[]}`, "CopyJobCDCNotImplemented"},
		{"external connection", external, "CopyJobExternalConnectionNotSupported"},
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

func TestCopyJobPublishesItsTerminalEventAtStart(t *testing.T) {
	// Pins the SECOND dispatch site. startJob publishes a job's terminal event
	// immediately only when terminalStatusOf says the type executes now —
	// remove CopyJob from executesNow and every other test here still passes,
	// because status converges later through the clock and the failure record.
	// What breaks is the flow view's contract: a job that really finished
	// stays unsettled on the event stream. Measured before writing this test:
	// that mutation failed zero of the six existing cases.
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	src := seedLakehouse(t, st, ws.ID, "src")
	dst := seedLakehouse(t, st, ws.ID, "dst")
	seedFile(t, st, ws.ID, src.ID, "Tables/orders/part-0.parquet", []byte("x"))

	terminal := func(jid string, evs []store.Event) string {
		t.Helper()
		for _, ev := range evs {
			if ev.Kind == store.KindJob && ev.JobID == jid &&
				(ev.Status == store.JobCompleted || ev.Status == store.JobFailed) {
				return ev.Status
			}
		}
		return ""
	}

	sub := st.Subscribe()
	defer sub.Close()
	cj := createCopyJob(t, st, ws.ID, lakehouseBatchDef(ws.ID, src.ID, dst.ID, ""))
	_, jid := runJob(t, a, ws.ID, cj.ID, "jobType=Execute", "{}")
	if got := terminal(jid, drainEvents(t, sub.C)); got != store.JobCompleted {
		t.Fatalf("no terminal event at start for a completed copy; got %q", got)
	}

	sub2 := st.Subscribe()
	defer sub2.Close()
	bad := createCopyJob(t, st, ws.ID, `{"properties":{"jobMode":"CDC"},"activities":[]}`)
	_, jid = runJob(t, a, ws.ID, bad.ID, "jobType=Execute", "{}")
	if got := terminal(jid, drainEvents(t, sub2.C)); got != store.JobFailed {
		t.Fatalf("no terminal event at start for a refused copy; got %q", got)
	}
}
