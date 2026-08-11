package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/calvinchengx/fabric-emulator/internal/testsupport"
)

// A lakehouse gets a SQLEndpoint ITEM, because a tenant gives it one.
//
// Measured 2026-08-11: one lakehouse + one warehouse left THREE items in a real
// workspace, the third being `SQLEndpoint  803c8e33-…  lake` — the lakehouse's
// display name, its own id, and that id is what sqlEndpointProperties.id reports.
func TestALakehouseGetsItsOwnSQLEndpointItem(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lake := seedItem(t, st, ws.ID, "Lakehouse", "lake")
	a.applyCreationPayload(lake, nil)

	eps, err := st.ListItems(ws.ID, "SQLEndpoint")
	if err != nil || len(eps) != 1 {
		t.Fatalf("lakehouse produced %d SQLEndpoint item(s), want 1 (err=%v)", len(eps), err)
	}
	if eps[0].DisplayName != "lake" {
		t.Errorf("endpoint display name = %q, want the lakehouse's %q", eps[0].DisplayName, "lake")
	}
	// The id must DIFFER from the lakehouse's: a consumer using it as a database
	// name is the failure the emulator used to prevent by omitting the field.
	if eps[0].ID == lake.ID {
		t.Error("the endpoint reuses the lakehouse id — the divergence this fixes")
	}
}

func TestTheReportedEndpointIDIsTheItemsOwn(t *testing.T) {
	a, st := newAPI(t)
	a.SQLEndpointPort = "1433"
	ws := seedWorkspace(t, st)
	lake := seedItem(t, st, ws.ID, "Lakehouse", "lake")
	a.applyCreationPayload(lake, nil)

	_, body := typedItemBody(t, a, "localhost:9443", ws.ID, lake.ID, "Lakehouse")
	props, _ := body["properties"].(map[string]any)
	ep, ok := props["sqlEndpointProperties"].(map[string]any)
	if !ok {
		t.Fatalf("no sqlEndpointProperties: %v", props)
	}
	eps, _ := st.ListItems(ws.ID, "SQLEndpoint")
	if len(eps) != 1 || ep["id"] != eps[0].ID {
		t.Fatalf("reported id %v does not name the SQLEndpoint item", ep["id"])
	}
	if ep["id"] == lake.ID {
		t.Error("reported the lakehouse id as the endpoint id")
	}
}

// A second create must not accumulate endpoints — a lakehouse has one.
func TestTheEndpointItemIsIdempotent(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lake := seedItem(t, st, ws.ID, "Lakehouse", "lake")
	a.applyCreationPayload(lake, nil)
	a.applyCreationPayload(lake, nil)
	eps, _ := st.ListItems(ws.ID, "SQLEndpoint")
	if len(eps) != 1 {
		t.Fatalf("%d endpoints after two calls, want 1", len(eps))
	}
}

func refreshCall(t *testing.T, a *API, wid, epid string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "/x", nil)
	r.SetPathValue("wid", wid)
	r.SetPathValue("epid", epid)
	w := httptest.NewRecorder()
	a.refreshSQLEndpointMetadata(w, r, admin)
	return w
}

// Without a SQL engine the refresh must SAY SO. An empty 200 would be
// indistinguishable from "the lakehouse has no tables" — the shape of lie this
// surface exists to remove.
func TestRefreshMetadataWithoutASQLEngineIsHonest(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lake := seedItem(t, st, ws.ID, "Lakehouse", "lake")
	a.applyCreationPayload(lake, nil)
	eps, _ := st.ListItems(ws.ID, "SQLEndpoint")

	w := refreshCall(t, a, ws.ID, eps[0].ID)
	if w.Code != 501 {
		t.Fatalf("refreshMetadata with no engine = %d, want 501: %s", w.Code, w.Body)
	}
}

// The tenant answers `{"value": []}` with a plain 200 — no LRO, no Location.
//
// Needs a real SQL Server, because reflection is real: it queries the database it
// is rebuilding. Skips without one (CI runs a SQL Server service), which is the
// repo's declared-skip pattern rather than a fake pool — a nil *sql.DB does not
// behave like an empty database, it panics.
func TestRefreshMetadataAnswersTheTenantsEnvelope(t *testing.T) {
	a, st := newAPI(t)
	db := testsupport.OpenMSSQL(t)
	ws := seedWorkspace(t, st)
	lake := seedItem(t, st, ws.ID, "Lakehouse", "lake")
	a.applyCreationPayload(lake, nil)
	eps, _ := st.ListItems(ws.ID, "SQLEndpoint")
	// A lakehouse with no Delta tables reflects nothing, which is the measured case.
	a.LakehouseDB = func(context.Context, string) (*sql.DB, error) { return db, nil }

	w := refreshCall(t, a, ws.ID, eps[0].ID)
	if w.Code != 200 {
		t.Fatalf("refreshMetadata = %d, want 200: %s", w.Code, w.Body)
	}
	if w.Header().Get("Location") != "" {
		t.Error("answered with a Location header; the tenant does not")
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	v, ok := body["value"].([]any)
	if !ok || len(v) != 0 {
		t.Fatalf("body = %s, want {\"value\": []}", w.Body)
	}
}

// Only a SQLEndpoint id resolves: a lakehouse id here is a 404, because the two
// are different items and confusing them is what the old omission guarded against.
func TestRefreshMetadataRejectsANonEndpointID(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lake := seedItem(t, st, ws.ID, "Lakehouse", "lake")
	a.applyCreationPayload(lake, nil)
	a.LakehouseDB = func(context.Context, string) (*sql.DB, error) { return nil, nil }

	// A 404 is reached before the pool is ever asked for, so the stub above is
	// never called — the id check is the assertion.
	if w := refreshCall(t, a, ws.ID, lake.ID); w.Code != 404 {
		t.Fatalf("a lakehouse id under /sqlEndpoints/ = %d, want 404", w.Code)
	}
}

// RBAC: the refresh mutates the SQL view, so a Viewer must not drive it.
func TestRefreshMetadataRequiresContributor(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lake := seedItem(t, st, ws.ID, "Lakehouse", "lake")
	a.applyCreationPayload(lake, nil)
	eps, _ := st.ListItems(ws.ID, "SQLEndpoint")
	a.LakehouseDB = func(context.Context, string) (*sql.DB, error) { return nil, nil }

	r := httptest.NewRequest("POST", "/x", nil)
	r.SetPathValue("wid", ws.ID)
	r.SetPathValue("epid", eps[0].ID)
	w := httptest.NewRecorder()
	a.refreshSQLEndpointMetadata(w, r, viewer)
	if w.Code == 200 {
		t.Fatalf("a Viewer refreshed the endpoint metadata: %s", w.Body)
	}
	_ = store.RoleViewer
}

// The properties a tenant reports and the emulator did not, measured 2026-08-11.
// Each is the emulator-strict direction (absent here, present there), which is
// the safe direction — but an absent `oneLakeTablesPath` is exactly what makes
// someone hardcode a OneLake path instead, and a hardcoded path does not survive
// the toggle.
func TestALakehouseReportsItsOneLakePaths(t *testing.T) {
	a, st := newAPI(t)
	a.SQLEndpointPort = "1433"
	ws := seedWorkspace(t, st)
	lake := seedItem(t, st, ws.ID, "Lakehouse", "lake")
	a.applyCreationPayload(lake, nil)

	_, body := typedItemBody(t, a, "localhost:9443", ws.ID, lake.ID, "Lakehouse")
	props, _ := body["properties"].(map[string]any)
	want := "http://localhost:9443/onelake/" + ws.ID + "/" + lake.ID
	if props["oneLakeTablesPath"] != want+"/Tables" {
		t.Errorf("oneLakeTablesPath = %v, want %q", props["oneLakeTablesPath"], want+"/Tables")
	}
	if props["oneLakeFilesPath"] != want+"/Files" {
		t.Errorf("oneLakeFilesPath = %v, want %q", props["oneLakeFilesPath"], want+"/Files")
	}
}

// OneLake is where the data is whether or not anything serves T-SQL over it, so
// the paths survive a stack with no SQL sidecar.
func TestOneLakePathsSurviveNoSQLEndpoint(t *testing.T) {
	a, st := newAPI(t)
	a.SQLEndpointPort = "" // contract-only stack
	ws := seedWorkspace(t, st)
	lake := seedItem(t, st, ws.ID, "Lakehouse", "lake")
	a.applyCreationPayload(lake, nil)

	_, body := typedItemBody(t, a, "localhost:9443", ws.ID, lake.ID, "Lakehouse")
	props, _ := body["properties"].(map[string]any)
	if props["oneLakeTablesPath"] == nil {
		t.Fatalf("no OneLake paths without a SQL endpoint: %v", props)
	}
	if _, present := props["sqlEndpointProperties"]; present {
		t.Error("advertised a SQL endpoint that is not listening")
	}
}

func TestAWarehouseReportsConnectionInfoAndCreationMode(t *testing.T) {
	a, st := newAPI(t)
	a.SQLEndpointPort = "1433"
	ws := seedWorkspace(t, st)
	wh := seedItem(t, st, ws.ID, "Warehouse", "dw")

	_, body := typedItemBody(t, a, "localhost:9443", ws.ID, wh.ID, "Warehouse")
	props, _ := body["properties"].(map[string]any)
	// A tenant returns the same address under both names; a client reading
	// `connectionInfo` got nothing here.
	if props["connectionInfo"] != props["connectionString"] {
		t.Errorf("connectionInfo %v != connectionString %v",
			props["connectionInfo"], props["connectionString"])
	}
	if props["creationMode"] != "New" {
		t.Errorf("creationMode = %v, want New", props["creationMode"])
	}
}
