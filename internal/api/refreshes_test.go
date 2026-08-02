package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// directLakeDataset publishes a Direct Lake model over a lakehouse.
func directLakeDataset(t *testing.T, st *store.Store, wid string) *store.Item {
	t.Helper()
	lake := &store.Item{WorkspaceID: wid, Type: "Lakehouse", DisplayName: "sales-lake"}
	if err := st.CreateItem(lake, nil); err != nil {
		t.Fatal(err)
	}
	ds := &store.Item{WorkspaceID: wid, Type: "SemanticModel", DisplayName: "Direct Sales"}
	if err := st.CreateItem(ds, []store.DefinitionPart{{
		Path: "model.bim", PayloadType: "InlineBase64",
		Payload: base64.StdEncoding.EncodeToString(directLakeModel(wid, lake.ID))}}); err != nil {
		t.Fatal(err)
	}
	return ds
}

// A refresh of a model whose rows are an inline snapshot must be REFUSED.
//
// This is the whole reason the endpoint discriminates. Returning Completed
// would tell a caller their numbers had been brought up to date when nothing
// was re-read — and a polling client would then trust stale data with the
// service's own word for it. An error the caller can act on beats a success
// they cannot check.
func TestRefreshRefusesAModelWithNothingToReadFrom(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	ds := createSemanticModel(t, st, ws.ID) // inline data.json

	w := do(a.postRefresh, admin, "POST", `{"notifyOption":"NoNotification"}`,
		map[string]string{"datasetId": ds.ID, "groupId": ws.ID})
	if w.Code != 400 {
		t.Fatalf("refresh of an inline-data model = %d, want 400: %s", w.Code, w.Body.Bytes())
	}
	if body := w.Body.String(); !strings.Contains(body, "data.json") {
		t.Errorf("error should name why it cannot refresh: %s", body)
	}
	// And nothing must be recorded — a refused refresh is not history.
	h := do(a.listRefreshes, admin, "GET", "", map[string]string{"datasetId": ds.ID})
	var hist struct {
		Value []map[string]any `json:"value"`
	}
	if err := json.Unmarshal(h.Body.Bytes(), &hist); err != nil {
		t.Fatal(err)
	}
	if len(hist.Value) != 0 {
		t.Errorf("a refused refresh was recorded: %s", h.Body.Bytes())
	}
}

// The accept path: 202, a RequestId a client can poll on, and history that
// reports it.
func TestRefreshLifecycle(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	ds := directLakeDataset(t, st, ws.ID)

	w := do(a.postRefresh, admin, "POST", `{"notifyOption":"NoNotification"}`,
		map[string]string{"datasetId": ds.ID, "groupId": ws.ID})
	if w.Code != 202 {
		t.Fatalf("refresh = %d, want 202: %s", w.Code, w.Body.Bytes())
	}
	reqID := w.Header().Get("RequestId")
	if reqID == "" {
		t.Fatal("no RequestId header; a polling client has nothing to poll on")
	}

	// History carries it, with the shape the swagger defines.
	h := do(a.listRefreshes, admin, "GET", "", map[string]string{"datasetId": ds.ID})
	if h.Code != 200 {
		t.Fatalf("history = %d %s", h.Code, h.Body.Bytes())
	}
	var hist struct {
		Context string `json:"@odata.context"`
		Value   []struct {
			RequestID   string `json:"requestId"`
			RefreshType string `json:"refreshType"`
			Status      string `json:"status"`
			StartTime   string `json:"startTime"`
			EndTime     string `json:"endTime"`
		} `json:"value"`
	}
	if err := json.Unmarshal(h.Body.Bytes(), &hist); err != nil {
		t.Fatal(err)
	}
	if hist.Context == "" {
		t.Error("@odata.context missing from the refreshes wrapper")
	}
	if len(hist.Value) != 1 || hist.Value[0].RequestID != reqID {
		t.Fatalf("history = %+v, want the request just made (%s)", hist.Value, reqID)
	}
	got := hist.Value[0]
	if got.Status != "Completed" || got.RefreshType != "ViaApi" {
		t.Errorf("refresh = %+v, want Completed/ViaApi", got)
	}
	// A terminated refresh has both times; a client computes duration from them.
	if got.StartTime == "" || got.EndTime == "" {
		t.Errorf("a Completed refresh must carry start and end: %+v", got)
	}

	// And the by-id form returns the same record.
	one := do(a.getRefresh, admin, "GET", "",
		map[string]string{"datasetId": ds.ID, "refreshId": reqID})
	if one.Code != 200 {
		t.Fatalf("get refresh = %d %s", one.Code, one.Body.Bytes())
	}
	var rec struct{ RequestID, Status string }
	if err := json.Unmarshal(one.Body.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.RequestID != reqID || rec.Status != "Completed" {
		t.Errorf("get refresh = %+v, want %s/Completed", rec, reqID)
	}

	// An unknown id is a 404, not an empty object.
	if miss := do(a.getRefresh, admin, "GET", "",
		map[string]string{"datasetId": ds.ID, "refreshId": "no-such"}); miss.Code != 404 {
		t.Errorf("unknown refresh id = %d, want 404", miss.Code)
	}
}

// History is newest-first, which is the order the real API documents and what a
// client reads [0] of to find the latest run. $top then means "the most recent
// N", not "the oldest N" — getting this backwards would hand a caller the first
// refresh ever made as though it were the current one.
func TestRefreshHistoryIsNewestFirst(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	ds := directLakeDataset(t, st, ws.ID)

	var ids []string
	for i := 0; i < 3; i++ {
		w := do(a.postRefresh, admin, "POST", "{}", map[string]string{"datasetId": ds.ID})
		if w.Code != 202 {
			t.Fatalf("refresh %d = %d", i, w.Code)
		}
		ids = append(ids, w.Header().Get("RequestId"))
	}

	read := func(query string) []string {
		t.Helper()
		r := httptest.NewRequest("GET", "/x?"+query, nil)
		r.SetPathValue("datasetId", ds.ID)
		w := httptest.NewRecorder()
		a.listRefreshes(w, r, admin)
		var hist struct {
			Value []struct {
				RequestID string `json:"requestId"`
			} `json:"value"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &hist); err != nil {
			t.Fatal(err)
		}
		out := make([]string, 0, len(hist.Value))
		for _, v := range hist.Value {
			out = append(out, v.RequestID)
		}
		return out
	}

	all := read("")
	if len(all) != 3 || all[0] != ids[2] || all[2] != ids[0] {
		t.Fatalf("history order = %v, want newest first (%v reversed)", all, ids)
	}
	if top := read("$top=1"); len(top) != 1 || top[0] != ids[2] {
		t.Errorf("$top=1 = %v, want the most recent refresh %s", top, ids[2])
	}
}

// Triggering a refresh writes; reading history does not. A Viewer must be able
// to see history and must not be able to start one.
func TestRefreshRBAC(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	ds := directLakeDataset(t, st, ws.ID)

	if w := do(a.postRefresh, viewer, "POST", "{}", map[string]string{"datasetId": ds.ID}); w.Code != 403 {
		t.Errorf("viewer refresh = %d, want 403", w.Code)
	}
	if w := do(a.listRefreshes, viewer, "GET", "", map[string]string{"datasetId": ds.ID}); w.Code != 200 {
		t.Errorf("viewer history = %d, want 200", w.Code)
	}
	if w := do(a.listRefreshes, nobody, "GET", "", map[string]string{"datasetId": ds.ID}); w.Code != 403 {
		t.Errorf("ungranted history = %d, want 403", w.Code)
	}
}

// As with the dataset routes, the handler tests inject path values, so the
// patterns themselves need driving through the real mux — including that
// /refreshes/{refreshId} does not shadow /refreshes.
func TestRefreshRoutesAreRegistered(t *testing.T) {
	a, _ := newAPI(t)
	mux := http.NewServeMux()
	a.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	const base = "/v1.0/myorg/groups/w-1/datasets/d-1/refreshes"
	for _, tc := range []struct{ method, path string }{
		{"POST", base},
		{"GET", base},
		{"GET", base + "/r-1"},
		{"POST", "/v1.0/myorg/datasets/d-1/refreshes"},
		{"GET", "/v1.0/myorg/datasets/d-1/refreshes"},
		{"GET", "/v1.0/myorg/datasets/d-1/refreshes/r-1"},
	} {
		req, err := http.NewRequest(tc.method, srv.URL+tc.path, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", tc.method, tc.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			t.Errorf("%s %s -> 404: the route is not registered", tc.method, tc.path)
		}
	}
}
