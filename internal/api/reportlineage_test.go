package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// TestReportLineageRecordsAStepsMovement: an interactive engine (no job)
// reports what it read and wrote, and the edges appear in the workspace graph
// marked as reported rather than observed.
func TestReportLineageRecordsAStepsMovement(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lake := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lake"}
	if err := st.CreateItem(lake, nil); err != nil {
		t.Fatal(err)
	}

	body := `{"step":"silver",
	  "reads":[{"itemId":"` + lake.ID + `","path":"Tables/bronze_orders"},
	           {"itemId":"` + lake.ID + `","path":"Tables/bronze_customers"}],
	  "writes":[{"itemId":"` + lake.ID + `","path":"Tables/silver_orders"}]}`

	w := do(a.reportLineage, admin, "POST", body, map[string]string{"wid": ws.ID})
	if w.Code != http.StatusOK {
		t.Fatalf("report = %d %s", w.Code, w.Body)
	}
	var out struct {
		Step          string `json:"step"`
		EdgesRecorded int    `json:"edgesRecorded"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	// One edge per (read × write) pair: a step joining two tables into one
	// records both inputs.
	if out.EdgesRecorded != 2 || out.Step != "silver" {
		t.Fatalf("recorded = %+v", out)
	}

	edges, err := st.ListLineageEdges(ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 2 {
		t.Fatalf("graph has %d edges", len(edges))
	}
	for _, e := range edges {
		// Reported, not observed — a consumer must be able to tell a claim from
		// something the emulator watched happen.
		if e.Producer != store.ProducerReported || e.ActivityName != "silver" {
			t.Fatalf("edge = %+v", e)
		}
		// No job: a step outside any run is the whole point of this endpoint.
		if e.JobID != "" {
			t.Fatalf("edge claimed a job it never had: %+v", e)
		}
		if e.TargetPath != "Tables/silver_orders" {
			t.Fatalf("target = %q", e.TargetPath)
		}
	}

	// Re-reporting is idempotent: a re-run must not double the graph.
	if w := do(a.reportLineage, admin, "POST", body, map[string]string{"wid": ws.ID}); w.Code != http.StatusOK {
		t.Fatalf("re-report = %d", w.Code)
	}
	edges, _ = st.ListLineageEdges(ws.ID)
	if len(edges) != 2 {
		t.Fatalf("re-reporting duplicated edges: %d", len(edges))
	}
}

func TestReportLineageRejectsHalfAMovement(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lake := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lake"}
	if err := st.CreateItem(lake, nil); err != nil {
		t.Fatal(err)
	}
	ref := `[{"itemId":"` + lake.ID + `","path":"Tables/t"}]`

	for name, body := range map[string]string{
		"no step":     `{"reads":` + ref + `,"writes":` + ref + `}`,
		"reads only":  `{"step":"s","reads":` + ref + `}`,
		"writes only": `{"step":"s","writes":` + ref + `}`,
		"neither end": `{"step":"s"}`,
		"incomplete reference": `{"step":"s","reads":[{"itemId":"` + lake.ID +
			`"}],"writes":` + ref + `}`,
		"not json": `{`,
	} {
		w := do(a.reportLineage, admin, "POST", body, map[string]string{"wid": ws.ID})
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s = %d; want 400 (an incomplete claim is not a fact)", name, w.Code)
		}
	}
	// Reporting lineage is a claim about the workspace's data, so it takes the
	// same role writing that data would.
	w := do(a.reportLineage, viewer, "POST",
		`{"step":"s","reads":`+ref+`,"writes":`+ref+`}`, map[string]string{"wid": ws.ID})
	if w.Code != http.StatusForbidden {
		t.Fatalf("viewer report = %d; want 403", w.Code)
	}
}
