package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

type dsResp struct {
	Context string `json:"@odata.context"`
	Value   []struct {
		DatasourceType    string `json:"datasourceType"`
		ConnectionDetails struct {
			URL  string `json:"url"`
			Path string `json:"path"`
		} `json:"connectionDetails"`
	} `json:"value"`
}

func readDatasources(t *testing.T, a *API, dsID string) dsResp {
	t.Helper()
	w := do(a.listDatasources, admin, "GET", "", map[string]string{"datasetId": dsID})
	if w.Code != 200 {
		t.Fatalf("datasources = %d %s", w.Code, w.Body.Bytes())
	}
	var out dsResp
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// The lineage a governance tool needs: this model reads that lakehouse, and it
// resolves to IDS so the answer survives a rename.
func TestDatasourcesReportsTheDirectLakeSource(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lake := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "sales-lake"}
	if err := st.CreateItem(lake, nil); err != nil {
		t.Fatal(err)
	}
	ds := directLakeDatasetOn(t, st, ws.ID, lake.ID)

	got := readDatasources(t, a, ds.ID)
	if got.Context == "" {
		t.Error("@odata.context missing from the datasources wrapper")
	}
	if len(got.Value) != 1 {
		t.Fatalf("datasources = %+v, want exactly one (the lakehouse)", got.Value)
	}
	d := got.Value[0]
	if !strings.Contains(d.ConnectionDetails.URL, lake.ID) || !strings.Contains(d.ConnectionDetails.URL, ws.ID) {
		t.Errorf("url %q should name the workspace %s and lakehouse %s by id",
			d.ConnectionDetails.URL, ws.ID, lake.ID)
	}
	if d.DatasourceType == "" {
		t.Error("datasourceType is required for a caller to interpret the row")
	}
}

// A model whose rows are an inline snapshot reads nothing, so an EMPTY list is
// the honest answer — unlike the My-workspace list in datasets.go, where empty
// would be indistinguishable from a workspace that happens to have none.
func TestDatasourcesIsEmptyForAnInlineModel(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	ds := createSemanticModel(t, st, ws.ID)

	got := readDatasources(t, a, ds.ID)
	if len(got.Value) != 0 {
		t.Errorf("inline-data model reported datasources it does not have: %+v", got.Value)
	}
}

// Several tables normally share one Direct Lake expression. The datasource is
// the LAKEHOUSE, not the table, so it must appear once — a caller counting
// sources should not see a number that tracks table count.
func TestDatasourcesDeduplicatesSharedExpressions(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lake := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "sales-lake"}
	if err := st.CreateItem(lake, nil); err != nil {
		t.Fatal(err)
	}
	ds := multiTableDirectLakeDataset(t, st, ws.ID, lake.ID)

	got := readDatasources(t, a, ds.ID)
	if len(got.Value) != 1 {
		t.Errorf("two tables over one lakehouse gave %d datasources, want 1: %+v",
			len(got.Value), got.Value)
	}
}

// A table pointing at an expression that is not in the model is a broken model.
// Saying so beats omitting the row, which would report a shorter list as though
// the model were smaller than it is.
func TestDatasourcesFailsOnADanglingExpression(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	ds := &store.Item{WorkspaceID: ws.ID, Type: "SemanticModel", DisplayName: "Broken"}
	bim := `{"name":"Broken","compatibilityLevel":1604,"model":{"tables":[
        {"name":"Sales","columns":[{"name":"Region","dataType":"string","sourceColumn":"Region"}],
         "partitions":[{"mode":"directLake","source":{"type":"entity","entityName":"sales",
          "expressionSource":"NoSuchExpression"}}]}]}}`
	if err := st.CreateItem(ds, []store.DefinitionPart{{
		Path: "model.bim", PayloadType: "InlineBase64", Payload: b64(bim)}}); err != nil {
		t.Fatal(err)
	}
	w := do(a.listDatasources, admin, "GET", "", map[string]string{"datasetId": ds.ID})
	if w.Code != 400 {
		t.Fatalf("dangling expression = %d, want 400: %s", w.Code, w.Body.Bytes())
	}
	if !strings.Contains(w.Body.String(), "NoSuchExpression") {
		t.Errorf("error should name the missing expression: %s", w.Body.Bytes())
	}
}

func TestDatasourcesRBACAndRouting(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	ds := createSemanticModel(t, st, ws.ID)

	if w := do(a.listDatasources, viewer, "GET", "", map[string]string{"datasetId": ds.ID}); w.Code != 200 {
		t.Errorf("viewer = %d, want 200", w.Code)
	}
	if w := do(a.listDatasources, nobody, "GET", "", map[string]string{"datasetId": ds.ID}); w.Code != 403 {
		t.Errorf("ungranted = %d, want 403", w.Code)
	}

	mux := http.NewServeMux()
	a.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	for _, path := range []string{
		"/v1.0/myorg/datasets/d-1/datasources",
		"/v1.0/myorg/groups/w-1/datasets/d-1/datasources",
	} {
		resp, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			t.Errorf("GET %s -> 404: the route is not registered", path)
		}
	}
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// directLakeDatasetOn publishes the shared Direct Lake fixture over a lakehouse
// the caller already made, so a test can assert on that lakehouse's id.
func directLakeDatasetOn(t *testing.T, st *store.Store, wid, lakeID string) *store.Item {
	t.Helper()
	ds := &store.Item{WorkspaceID: wid, Type: "SemanticModel", DisplayName: "Direct Sales"}
	if err := st.CreateItem(ds, []store.DefinitionPart{{
		Path: "model.bim", PayloadType: "InlineBase64",
		Payload: base64.StdEncoding.EncodeToString(directLakeModel(wid, lakeID))}}); err != nil {
		t.Fatal(err)
	}
	return ds
}

// multiTableDirectLakeDataset: two tables, ONE shared expression — the shape a
// real Direct Lake model has, and the one that would show a duplicate source if
// the endpoint keyed on tables instead of on the lakehouse.
func multiTableDirectLakeDataset(t *testing.T, st *store.Store, wid, lakeID string) *store.Item {
	t.Helper()
	bim := fmt.Sprintf(`{"name":"TwoTables","compatibilityLevel":1604,"model":{
      "expressions":[{"name":"DL","kind":"m","expression":"let Source = AzureStorage.DataLake(\"https://onelake.dfs.fabric.microsoft.com/%s/%s\", [HierarchicalNavigation=true]) in Source"}],
      "tables":[
        {"name":"Sales","columns":[{"name":"Region","dataType":"string","sourceColumn":"region"}],
         "partitions":[{"mode":"directLake","source":{"type":"entity","entityName":"sales","expressionSource":"DL"}}]},
        {"name":"Returns","columns":[{"name":"Region","dataType":"string","sourceColumn":"region"}],
         "partitions":[{"mode":"directLake","source":{"type":"entity","entityName":"returns","expressionSource":"DL"}}]}]}}`,
		wid, lakeID)
	ds := &store.Item{WorkspaceID: wid, Type: "SemanticModel", DisplayName: "Two Tables"}
	if err := st.CreateItem(ds, []store.DefinitionPart{{
		Path: "model.bim", PayloadType: "InlineBase64", Payload: b64(bim)}}); err != nil {
		t.Fatal(err)
	}
	return ds
}
