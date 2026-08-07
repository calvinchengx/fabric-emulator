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
	if s := jobStatus(t, a, ws.ID, cj.ID, jid); s != "Completed" {
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
	if s := jobStatus(t, a, ws.ID, cj.ID, jid); s != "Completed" {
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
	if s := jobStatus(t, a, ws.ID, cj.ID, jid); s != "Completed" {
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
			if s := jobStatus(t, a, ws.ID, cj.ID, jid); s != "Failed" {
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
