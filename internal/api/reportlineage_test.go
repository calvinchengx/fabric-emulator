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

	body := `{"step":"silver","reads":[
	    {"itemId":"` + lake.ID + `","path":"Tables/bronze_orders"},
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

// TestReportLineageMovesAreNotACrossProduct: the precise form. A silver step
// reads two bronze tables and writes three silver ones, but the quarantine
// comes from the orders alone — so the cross product would record three
// movements that never happened. `moves` reports the derivations the code
// actually computes.
func TestReportLineageMovesAreNotACrossProduct(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lake := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lake"}
	if err := st.CreateItem(lake, nil); err != nil {
		t.Fatal(err)
	}
	ref := func(p string) string { return `{"itemId":"` + lake.ID + `","path":"Tables/` + p + `"}` }

	body := `{"step":"silver","moves":[
	  {"reads":[` + ref("bronze_customers") + `],"writes":[` + ref("silver_customers") + `]},
	  {"reads":[` + ref("bronze_orders") + `],
	   "writes":[` + ref("silver_orders") + `,` + ref("silver_quarantine_orders") + `]}]}`
	w := do(a.reportLineage, admin, "POST", body, map[string]string{"wid": ws.ID})
	if w.Code != http.StatusOK {
		t.Fatalf("report = %d %s", w.Code, w.Body)
	}

	edges, err := st.ListLineageEdges(ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range edges {
		got[e.SourcePath+" -> "+e.TargetPath] = true
	}
	want := []string{
		"Tables/bronze_customers -> Tables/silver_customers",
		"Tables/bronze_orders -> Tables/silver_orders",
		"Tables/bronze_orders -> Tables/silver_quarantine_orders",
	}
	if len(edges) != len(want) {
		t.Fatalf("recorded %d edges, want %d: %v", len(edges), len(want), got)
	}
	for _, k := range want {
		if !got[k] {
			t.Errorf("missing edge %q", k)
		}
	}
	// The three the cross product would have invented.
	for _, k := range []string{
		"Tables/bronze_customers -> Tables/silver_orders",
		"Tables/bronze_customers -> Tables/silver_quarantine_orders",
		"Tables/bronze_orders -> Tables/silver_customers",
	} {
		if got[k] {
			t.Errorf("recorded %q, a movement that never happened", k)
		}
	}

	// A movement missing an end is rejected, per movement rather than overall.
	half := `{"step":"s","moves":[{"reads":[` + ref("a") + `],"writes":[` + ref("b") + `]},
	                              {"reads":[` + ref("c") + `]}]}`
	if w := do(a.reportLineage, admin, "POST", half, map[string]string{"wid": ws.ID}); w.Code != http.StatusBadRequest {
		t.Fatalf("half a movement = %d; want 400", w.Code)
	}
}
