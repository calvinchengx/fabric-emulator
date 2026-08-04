package api

// The gold → semantic model hop. docs/parity.md claims this as 🟢 Real with
// `producer: DirectLake`, and until these tests existed NOTHING exercised it:
// `directLakeSource` sat at 0% statement coverage and no test anywhere asserted
// a DirectLake edge. The claim was true of the code and unwitnessed by the
// suite — the exact shape of the witness-coverage gap the audit recorded.
//
// The lineage is recorded when the DEFINITION lands, so these drive the real
// createItem handler rather than the store, which is why the existing Direct
// Lake fixtures (they call st.CreateItem directly) never reached it.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// directLakeEdges returns the DirectLake-produced edges recorded in a workspace.
func directLakeEdges(t *testing.T, st *store.Store, wid string) []*store.LineageEdge {
	t.Helper()
	all, err := st.ListLineageEdges(wid)
	if err != nil {
		t.Fatal(err)
	}
	var out []*store.LineageEdge
	for _, e := range all {
		if e.Producer == store.ProducerDirectLake {
			out = append(out, e)
		}
	}
	return out
}

// createModelViaAPI publishes a SemanticModel through the real handler, which is
// what triggers recordModelLineage.
func createModelViaAPI(t *testing.T, a *API, wid, name string, bim []byte) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"displayName": name,
		"type":        "SemanticModel",
		"definition": map[string]any{"parts": []map[string]string{{
			"path":        "model.bim",
			"payloadType": "InlineBase64",
			"payload":     base64.StdEncoding.EncodeToString(bim),
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	w := do(a.createItem, admin, "POST", string(body), map[string]string{"wid": wid})
	if w.Code != http.StatusCreated && w.Code != http.StatusAccepted {
		t.Fatalf("create semantic model = %d %s", w.Code, w.Body.Bytes())
	}
	// Creation may answer 202 with the id behind an operation, so the item is
	// resolved by name rather than parsed out of a body whose shape varies.
	it, err := a.Store.GetItemByName(wid, name, "SemanticModel")
	if err != nil {
		t.Fatalf("model %q was not created: %v", name, err)
	}
	return it.ID
}

// TestDirectLakeDefinitionRecordsALineageEdge is the witness for the parity
// claim: publishing a Direct Lake model records gold → model with the DirectLake
// producer, pointing at the lakehouse table the binding actually names.
func TestDirectLakeDefinitionRecordsALineageEdge(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lake := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lake"}
	if err := st.CreateItem(lake, nil); err != nil {
		t.Fatal(err)
	}

	dsID := createModelViaAPI(t, a, ws.ID, "Direct Sales", directLakeModel(ws.ID, lake.ID))

	edges := directLakeEdges(t, st, ws.ID)
	if len(edges) != 1 {
		t.Fatalf("recorded %d DirectLake edge(s); want exactly 1", len(edges))
	}
	e := edges[0]
	// The source is the lakehouse table the binding names — entityName "sales"
	// under schemaName "dbo" — not the model, and not a guess.
	if e.SourceItemID != lake.ID || e.SourcePath != "Tables/dbo/sales" {
		t.Errorf("source = %s %q; want the lakehouse's Tables/dbo/sales",
			e.SourceItemID, e.SourcePath)
	}
	if e.TargetItemID != dsID || e.TargetPath != "Tables/Sales" {
		t.Errorf("target = %s %q; want the model's Tables/Sales", e.TargetItemID, e.TargetPath)
	}
	if e.ActivityName != "DirectLake" {
		t.Errorf("activity = %q; want DirectLake", e.ActivityName)
	}
}

// TestImportModelRecordsNoLineage pins the deliberate absence, which is half the
// claim: an import model's rows arrive in the definition already detached from
// wherever they were selected, so inventing a source would be a guess. A test
// that only proved Direct Lake works would let a change start fabricating edges
// for import models without anything noticing.
func TestImportModelRecordsNoLineage(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)

	// Same shape, but the partition is an import query rather than a binding.
	bim := []byte(`{"name":"Imported","compatibilityLevel":1604,"model":{
	  "tables":[{"name":"Sales","columns":[{"name":"Region","dataType":"string"}],
	    "partitions":[{"name":"Sales","mode":"import",
	      "source":{"type":"m","expression":"let Source = Csv.Document(x) in Source"}}]}]}}`)
	createModelViaAPI(t, a, ws.ID, "Imported", bim)

	if edges := directLakeEdges(t, st, ws.ID); len(edges) != 0 {
		t.Fatalf("an import model produced %d lineage edge(s); its provenance is "+
			"not knowable here and must not be invented", len(edges))
	}
}

// TestDirectLakeBindingThatCannotResolveRecordsNothing: a binding naming a
// lakehouse that does not exist records no edge rather than one pointing at a
// table nobody has. Same rule as the nested-CTE fix — a catalog inventing
// provenance is worse than one missing it.
func TestDirectLakeBindingThatCannotResolveRecordsNothing(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)

	missing := fmt.Sprintf("%s/%s", ws.ID, "00000000-0000-0000-0000-000000000000")
	bim := []byte(fmt.Sprintf(`{"name":"Dangling","compatibilityLevel":1604,"model":{
	  "expressions":[{"name":"DL","kind":"m","expression":"let Source = AzureStorage.DataLake(\"https://onelake.dfs.fabric.microsoft.com/%s\", [HierarchicalNavigation=true]) in Source"}],
	  "tables":[{"name":"Sales","columns":[{"name":"Region","dataType":"string"}],
	    "partitions":[{"name":"Sales","mode":"directLake",
	      "source":{"type":"entity","entityName":"sales","expressionSource":"DL"}}]}]}}`, missing))
	createModelViaAPI(t, a, ws.ID, "Dangling", bim)

	if edges := directLakeEdges(t, st, ws.ID); len(edges) != 0 {
		t.Fatalf("a binding to a lakehouse that does not exist produced %d edge(s); "+
			"want none", len(edges))
	}
}
