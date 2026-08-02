package api

import (
	"encoding/json"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

func startScan(t *testing.T, a *API, p *auth.Principal, wsID, query string) string {
	t.Helper()
	r := httptest.NewRequest("POST", "/x?"+query, strings.NewReader(`{"workspaces":["`+wsID+`"]}`))
	w := httptest.NewRecorder()
	a.postScan(w, r, p)
	if w.Code != 202 {
		t.Fatalf("getInfo = %d %s", w.Code, w.Body.Bytes())
	}
	var req struct{ ID, Status string }
	if err := json.Unmarshal(w.Body.Bytes(), &req); err != nil {
		t.Fatal(err)
	}
	if req.ID == "" || req.Status == "" {
		t.Fatalf("scan request missing id/status: %s", w.Body.Bytes())
	}
	return req.ID
}

func scanResultFor(t *testing.T, a *API, p *auth.Principal, scanID string) scanResult {
	t.Helper()
	w := do(a.getScanResult, p, "GET", "", map[string]string{"scanId": scanID})
	if w.Code != 200 {
		t.Fatalf("scanResult = %d %s", w.Code, w.Body.Bytes())
	}
	var out scanResult
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// The full four-call crawl: what changed, scan it, poll it, read it. A crawler
// written against the real service follows exactly this path, so all four have
// to work in sequence rather than individually.
func TestScannerCrawlSequence(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lake := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "sales-lake"}
	if err := st.CreateItem(lake, nil); err != nil {
		t.Fatal(err)
	}
	directLakeDatasetOn(t, st, ws.ID, lake.ID)

	// 1. modified — the workspace must be discoverable here, or a crawler never
	//    learns it exists.
	w := do(a.listModifiedWorkspaces, admin, "GET", "", nil)
	if w.Code != 200 {
		t.Fatalf("modified = %d %s", w.Code, w.Body.Bytes())
	}
	var mods []struct{ ID string }
	if err := json.Unmarshal(w.Body.Bytes(), &mods); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range mods {
		if m.ID == ws.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("workspace %s not in modified list %+v", ws.ID, mods)
	}

	// 2-3. scan, then poll it the way a real crawler does.
	scanID := startScan(t, a, admin, ws.ID, "datasetSchema=true&datasetExpressions=true&datasourceDetails=true")
	stat := do(a.getScanStatus, admin, "GET", "", map[string]string{"scanId": scanID})
	if stat.Code != 200 {
		t.Fatalf("scanStatus = %d %s", stat.Code, stat.Body.Bytes())
	}
	var sr struct{ ID, Status string }
	if err := json.Unmarshal(stat.Body.Bytes(), &sr); err != nil {
		t.Fatal(err)
	}
	if sr.ID != scanID || sr.Status != "Succeeded" {
		t.Fatalf("scanStatus = %+v, want %s/Succeeded", sr, scanID)
	}

	// 4. the payload.
	res := scanResultFor(t, a, admin, scanID)
	if len(res.Workspaces) != 1 || res.Workspaces[0].ID != ws.ID {
		t.Fatalf("workspaces = %+v", res.Workspaces)
	}
	if len(res.Workspaces[0].Datasets) != 1 {
		t.Fatalf("datasets = %+v", res.Workspaces[0].Datasets)
	}
	ds := res.Workspaces[0].Datasets[0]

	// The whole reason this surface exists: structure. Nothing else here
	// returns tables/columns/measures.
	if len(ds.Tables) != 1 || ds.Tables[0].Name != "Sales" {
		t.Fatalf("tables = %+v, want the Sales table", ds.Tables)
	}
	cols := map[string]string{}
	for _, c := range ds.Tables[0].Columns {
		cols[c.Name] = c.DataType
	}
	if cols["Region"] == "" || cols["Amount"] == "" {
		t.Errorf("columns = %+v, want Region and Amount with data types", ds.Tables[0].Columns)
	}
	if len(ds.Tables[0].Measures) != 1 || ds.Tables[0].Measures[0].Name != "Total" {
		t.Errorf("measures = %+v, want the Total measure", ds.Tables[0].Measures)
	}
	if ds.Tables[0].Measures[0].Expression == "" {
		t.Error("a measure without its expression tells a catalog nothing about how it is computed")
	}
	if len(ds.Expressions) == 0 {
		t.Error("datasetExpressions=true was asked for but none came back")
	}
	// Lineage in the same payload — the reason this is worth doing for a
	// catalog rather than calling /datasources per dataset.
	if len(res.DatasourceInstances) != 1 ||
		!strings.Contains(res.DatasourceInstances[0].ConnectionDetails.URL, lake.ID) {
		t.Errorf("datasourceInstances = %+v, want the lakehouse %s", res.DatasourceInstances, lake.ID)
	}
}

// Optional means optional. A crawler that forgets the flag and gets the schema
// anyway will ship, then break against real Fabric where it does not.
func TestScannerOmitsSchemaUnlessAsked(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	createSemanticModel(t, st, ws.ID)

	res := scanResultFor(t, a, admin, startScan(t, a, admin, ws.ID, ""))
	if len(res.Workspaces[0].Datasets) != 1 {
		t.Fatalf("datasets = %+v", res.Workspaces[0].Datasets)
	}
	ds := res.Workspaces[0].Datasets[0]
	if len(ds.Tables) != 0 || len(ds.Expressions) != 0 {
		t.Errorf("schema returned without datasetSchema=true: tables=%d expressions=%d",
			len(ds.Tables), len(ds.Expressions))
	}
	// The dataset itself must still be listed — the flag governs depth, not
	// whether the tenant contains it.
	if ds.ID == "" || ds.Name == "" {
		t.Errorf("dataset not identified at all: %+v", ds)
	}
}

// One unreadable model must not cost a crawler the whole tenant. The swagger
// has schemaRetrievalError for exactly this.
func TestScannerReportsABrokenModelWithoutFailing(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	good := createSemanticModel(t, st, ws.ID)
	bad := &store.Item{WorkspaceID: ws.ID, Type: "SemanticModel", DisplayName: "Broken"}
	if err := st.CreateItem(bad, []store.DefinitionPart{{
		Path: "model.bim", PayloadType: "InlineBase64", Payload: b64(`{not json`)}}); err != nil {
		t.Fatal(err)
	}

	res := scanResultFor(t, a, admin, startScan(t, a, admin, ws.ID, "datasetSchema=true"))
	byID := map[string]scanDataset{}
	for _, d := range res.Workspaces[0].Datasets {
		byID[d.ID] = d
	}
	if len(byID) != 2 {
		t.Fatalf("a broken model cost the scan a dataset: %+v", res.Workspaces[0].Datasets)
	}
	if byID[bad.ID].SchemaRetrievalError == "" {
		t.Error("the broken model reported no schemaRetrievalError, so a crawler cannot tell it is incomplete")
	}
	if byID[good.ID].SchemaRetrievalError != "" {
		t.Errorf("the good model was blamed: %q", byID[good.ID].SchemaRetrievalError)
	}
	if len(byID[good.ID].Tables) == 0 {
		t.Error("the good model lost its schema because another model was broken")
	}
}

// modifiedSince is refused rather than ignored: silently returning everything
// would let a crawler record a full pass as an incremental one.
func TestScannerRefusesModifiedSince(t *testing.T) {
	a, st := newAPI(t)
	seedWorkspace(t, st)

	r := httptest.NewRequest("GET", "/x?modifiedSince=2026-01-01T00:00:00Z", nil)
	w := httptest.NewRecorder()
	a.listModifiedWorkspaces(w, r, admin)
	if w.Code != 400 {
		t.Fatalf("modifiedSince = %d, want 400: %s", w.Code, w.Body.Bytes())
	}
	if !strings.Contains(w.Body.String(), "modifiedSince") {
		t.Errorf("error should name the parameter it cannot honour: %s", w.Body.Bytes())
	}
}

// A scan names every dataset in the workspaces it covered, so its id is the
// only thing protecting it — and a scan must not be startable on a workspace
// the caller cannot read.
func TestScannerAccessControl(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	createSemanticModel(t, st, ws.ID)

	// Not a member: cannot scan.
	r := httptest.NewRequest("POST", "/x", strings.NewReader(`{"workspaces":["`+ws.ID+`"]}`))
	w := httptest.NewRecorder()
	a.postScan(w, r, nobody)
	if w.Code != 403 {
		t.Errorf("ungranted scan = %d, want 403", w.Code)
	}

	// Another principal must not read someone else's scan, even with the id.
	scanID := startScan(t, a, admin, ws.ID, "datasetSchema=true")
	if got := do(a.getScanResult, viewer, "GET", "", map[string]string{"scanId": scanID}); got.Code != 404 {
		t.Errorf("cross-principal scanResult = %d, want 404 (confirming the id exists is itself a disclosure)", got.Code)
	}
	if got := do(a.getScanStatus, viewer, "GET", "", map[string]string{"scanId": scanID}); got.Code != 404 {
		t.Errorf("cross-principal scanStatus = %d, want 404", got.Code)
	}
	// An unknown id is the same 404.
	if got := do(a.getScanResult, admin, "GET", "", map[string]string{"scanId": "no-such"}); got.Code != 404 {
		t.Errorf("unknown scan = %d, want 404", got.Code)
	}
}

// A workspace named in the body but absent must fail the scan, not silently
// return fewer workspaces — a crawler cannot tell a skipped one from an empty.
func TestScannerFailsOnAnUnknownWorkspace(t *testing.T) {
	a, st := newAPI(t)
	seedWorkspace(t, st)
	r := httptest.NewRequest("POST", "/x", strings.NewReader(`{"workspaces":["00000000-0000-0000-0000-000000000000"]}`))
	w := httptest.NewRecorder()
	a.postScan(w, r, admin)
	if w.Code != 404 {
		t.Errorf("unknown workspace = %d, want 404: %s", w.Code, w.Body.Bytes())
	}
}

func TestScannerRoutesAreRegistered(t *testing.T) {
	a, _ := newAPI(t)
	mux := http.NewServeMux()
	a.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	for _, tc := range []struct{ method, path string }{
		{"GET", "/v1.0/myorg/admin/workspaces/modified"},
		{"POST", "/v1.0/myorg/admin/workspaces/getInfo"},
		{"GET", "/v1.0/myorg/admin/workspaces/scanStatus/s-1"},
		{"GET", "/v1.0/myorg/admin/workspaces/scanResult/s-1"},
	} {
		req, err := http.NewRequest(tc.method, srv.URL+tc.path, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			t.Errorf("%s %s -> 404: the route is not registered", tc.method, tc.path)
		}
	}
}
