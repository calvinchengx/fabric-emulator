package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// A Power BI client's first call is a LIST, not a query. Until this endpoint
// existed the only way to obtain a datasetId was to watch the Fabric item
// API's long-running operation, so a client that started where real clients
// start could not find a model that was sitting right there.
func TestListDatasetsInGroup(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	ds := createSemanticModel(t, st, ws.ID)

	// Items that are not semantic models must not appear: a dataset id that
	// executeQueries cannot answer for is worse than one that does not list.
	if err := st.CreateItem(&store.Item{
		WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lh"}, nil); err != nil {
		t.Fatal(err)
	}

	w := do(a.listDatasetsInGroup, admin, "GET", "", map[string]string{"groupId": ws.ID})
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.Bytes())
	}
	var resp struct {
		Context string `json:"@odata.context"`
		Value   []struct {
			ID                string `json:"id"`
			Name              string `json:"name"`
			IsRefreshable     bool   `json:"isRefreshable"`
			AddRowsAPIEnabled bool   `json:"addRowsAPIEnabled"`
		} `json:"value"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Value) != 1 {
		t.Fatalf("listed %d datasets, want only the SemanticModel: %s", len(resp.Value), w.Body.Bytes())
	}
	if resp.Value[0].ID != ds.ID || resp.Value[0].Name != "RetailAnalysis" {
		t.Errorf("dataset = %+v, want id %s named RetailAnalysis", resp.Value[0], ds.ID)
	}
	// The OData wrapper is part of the golden shape, not decoration — a client
	// deserialising into the swagger's Datasets type needs it present.
	if resp.Context == "" {
		t.Error("@odata.context missing; the swagger's list wrapper carries it")
	}
	// Definite false, not absent: a client deciding whether to offer a refresh
	// needs "no" rather than "unknown", and today the answer really is no.
	if resp.Value[0].IsRefreshable || resp.Value[0].AddRowsAPIEnabled {
		t.Errorf("dataset claims capabilities it does not have: %+v", resp.Value[0])
	}
}

// The id a client discovers by listing must be the id executeQueries accepts.
// If these ever diverge, discovery is decorative.
func TestListedDatasetIDIsQueryable(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	createSemanticModel(t, st, ws.ID)

	w := do(a.listDatasetsInGroup, admin, "GET", "", map[string]string{"groupId": ws.ID})
	var resp struct {
		Value []struct {
			ID string `json:"id"`
		} `json:"value"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || len(resp.Value) != 1 {
		t.Fatalf("list failed: %v %s", err, w.Body.Bytes())
	}
	discovered := resp.Value[0].ID

	q := `{"queries":[{"query":"EVALUATE 'Store'"}]}`
	if got := do(a.executeQueries, admin, "POST", q,
		map[string]string{"datasetId": discovered, "groupId": ws.ID}); got.Code != 200 {
		t.Fatalf("the discovered id did not query: %d %s", got.Code, got.Body.Bytes())
	}
}

func TestGetDataset(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	ds := createSemanticModel(t, st, ws.ID)

	// Both path forms, as the swagger defines them.
	for _, vars := range []map[string]string{
		{"datasetId": ds.ID},
		{"datasetId": ds.ID, "groupId": ws.ID},
	} {
		w := do(a.getDataset, admin, "GET", "", vars)
		if w.Code != 200 {
			t.Fatalf("vars %v: %d %s", vars, w.Code, w.Body.Bytes())
		}
		var d struct{ ID, Name string }
		if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
			t.Fatal(err)
		}
		if d.ID != ds.ID {
			t.Errorf("vars %v: id = %q, want %q", vars, d.ID, ds.ID)
		}
	}

	// A wrong group must not read a dataset it does not contain — the same rule
	// executeQueries applies, so an id discovered in one workspace cannot be
	// read through another.
	if w := do(a.getDataset, admin, "GET", "",
		map[string]string{"datasetId": ds.ID, "groupId": "00000000-0000-0000-0000-000000000000"}); w.Code != 404 {
		t.Errorf("cross-workspace read = %d, want 404", w.Code)
	}
	// A non-SemanticModel id is not a dataset.
	lh := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lh"}
	if err := st.CreateItem(lh, nil); err != nil {
		t.Fatal(err)
	}
	if w := do(a.getDataset, admin, "GET", "", map[string]string{"datasetId": lh.ID}); w.Code != 404 {
		t.Errorf("lakehouse as dataset = %d, want 404", w.Code)
	}
}

// Discovery is a read, so it obeys the same RBAC as querying.
func TestDatasetsRBAC(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	ds := createSemanticModel(t, st, ws.ID)

	if w := do(a.listDatasetsInGroup, viewer, "GET", "", map[string]string{"groupId": ws.ID}); w.Code != 200 {
		t.Errorf("viewer list = %d, want 200", w.Code)
	}
	if w := do(a.listDatasetsInGroup, nobody, "GET", "", map[string]string{"groupId": ws.ID}); w.Code != 403 {
		t.Errorf("nobody list = %d, want 403", w.Code)
	}
	if w := do(a.getDataset, nobody, "GET", "", map[string]string{"datasetId": ds.ID}); w.Code != 403 {
		t.Errorf("nobody get = %d, want 403", w.Code)
	}
}

// The My-workspace list must SAY it has nothing to describe rather than
// returning an empty array. `{"value": []}` is well-formed and would be
// indistinguishable from a personal workspace that happens to be empty — the
// caller would conclude their model failed to publish. This emulator models
// workspaces only, and the error names the route that does work.
func TestMyWorkspaceListRefusesRatherThanLying(t *testing.T) {
	a, st := newAPI(t)
	seedWorkspace(t, st)

	w := do(a.listDatasetsMyWorkspace, admin, "GET", "", nil)
	if w.Code == 200 {
		t.Fatalf("returned a body for a workspace that does not exist: %s", w.Body.Bytes())
	}
	if w.Code != 404 {
		t.Errorf("status = %d, want 404", w.Code)
	}
	if body := w.Body.String(); !strings.Contains(body, "groups/{groupId}/datasets") {
		t.Errorf("error should name the route that works: %s", body)
	}
}

// The five tests above call handlers directly, with path values injected. That
// verifies the logic and NOT the routing — a mistyped pattern, a missing
// registration, or a path that shadows another would pass every one of them and
// only surface in CI against a real client.
//
// This drives the actual ServeMux, so the route patterns are under test too.
// Auth is not configured here (PBIAuth is nil), which the Power BI surface
// answers with 501 — that is enough to prove the route EXISTS and reached its
// handler, and it distinguishes cleanly from the 404 an unregistered path gives.
func TestDatasetRoutesAreRegistered(t *testing.T) {
	a, _ := newAPI(t)
	mux := http.NewServeMux()
	a.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	const ws, ds = "w-1", "d-1"
	for _, path := range []string{
		"/v1.0/myorg/groups/" + ws + "/datasets",
		"/v1.0/myorg/groups/" + ws + "/datasets/" + ds,
		"/v1.0/myorg/datasets/" + ds,
		"/v1.0/myorg/datasets",
	} {
		resp, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			t.Errorf("GET %s -> 404: the route is not registered", path)
		}
	}

	// And the executeQueries route still resolves under the same prefix — the
	// dataset paths must not shadow it, which is the failure a nested pattern
	// would produce.
	resp, err := srv.Client().Post(
		srv.URL+"/v1.0/myorg/groups/"+ws+"/datasets/"+ds+"/executeQueries",
		"application/json", strings.NewReader(`{"queries":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		t.Error("executeQueries -> 404: a dataset route shadowed it")
	}
}
