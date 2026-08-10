package api

// A lakehouse's SQL analytics endpoint, discovered the way real Fabric documents.
//
// WHY THIS MATTERS BEYOND ONE PROPERTY. An example that dials a configured host
// (`TDS_SERVER=localhost,1433`) cannot run against real Fabric, where the address
// is per-workspace and only the API knows it. The portable form is to ask the
// item — `GET /lakehouses/{id}` -> properties.sqlEndpointProperties.connectionString
// for the analytics endpoint, `GET /warehouses/{id}` -> properties.connectionString
// for a warehouse. The warehouse half already worked (warehouse_endpoint.go); the
// lakehouse half returned nothing, so the four medallion examples had no
// discoverable address for the surface reflect.py connects to.

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// typedItemBody drives the typed route's handler with a fake principal, as the
// rest of this suite does, and returns its decoded body.
func typedItemBody(t *testing.T, a *API, host, wid, iid, itemType string) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest("GET", "/x", nil)
	r.Host = host
	r.SetPathValue("wid", wid)
	r.SetPathValue("iid", iid)
	w := httptest.NewRecorder()
	a.typedGet(itemType)(w, r, admin)
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w.Code, body
}

func seedItem(t *testing.T, st *store.Store, wsID, itemType, name string) *store.Item {
	t.Helper()
	it := &store.Item{WorkspaceID: wsID, Type: itemType, DisplayName: name}
	if err := st.CreateItem(it, nil); err != nil {
		t.Fatal(err)
	}
	return it
}

func TestLakehouseAdvertisesItsSQLAnalyticsEndpoint(t *testing.T) {
	a, st := newAPI(t)
	a.SQLEndpointPort = "1433"
	ws := seedWorkspace(t, st)
	lake := seedItem(t, st, ws.ID, "Lakehouse", "lake")

	code, body := typedItemBody(t, a, "localhost:9443", ws.ID, lake.ID, "Lakehouse")
	if code != 200 {
		t.Fatalf("GET /lakehouses/{id} = %d %v", code, body)
	}
	props, ok := body["properties"].(map[string]any)
	if !ok {
		t.Fatalf("no properties on a lakehouse with a SQL endpoint: %v", body)
	}
	ep, ok := props["sqlEndpointProperties"].(map[string]any)
	if !ok {
		t.Fatalf("no sqlEndpointProperties: %v", props)
	}
	if ep["connectionString"] != "localhost:1433" {
		t.Errorf("connectionString = %v, want localhost:1433", ep["connectionString"])
	}
	if ep["provisioningStatus"] != "Success" {
		t.Errorf("provisioningStatus = %v, want Success", ep["provisioningStatus"])
	}
	// See kql.go: on real Fabric the analytics endpoint is a separate SQLEndpoint
	// item with its own GUID. Reporting the lakehouse id under that name would
	// invite a consumer to use it as a database name, which works here and fails
	// on real Fabric. Absent fails the other way round: locally, before it ships.
	if _, present := ep["id"]; present {
		t.Errorf("sqlEndpointProperties.id is present (%v): the emulator has no "+
			"SQLEndpoint item, so any value here would be a different id than real "+
			"Fabric's", ep["id"])
	}
	// Two surfaces, two property names. A lakehouse never carries the Warehouse
	// one (TestOnlyAWarehouseGetsAConnectionString covers the generic route).
	if cs, present := props["connectionString"]; present {
		t.Errorf("a lakehouse advertised a warehouse connectionString: %v", cs)
	}
}

// The address is echoed from the request for the same reason a warehouse's is:
// the caller's own host is by definition reachable from where the caller stands.
func TestLakehouseEndpointIsDialableFromWhereverTheCallerIs(t *testing.T) {
	a, st := newAPI(t)
	a.SQLEndpointPort = "1433"
	ws := seedWorkspace(t, st)
	lake := seedItem(t, st, ws.ID, "Lakehouse", "lake")

	for host, want := range map[string]string{
		"fabric-emulator:9443": "fabric-emulator:1433",
		"localhost:19543":      "localhost:1433",
	} {
		_, body := typedItemBody(t, a, host, ws.ID, lake.ID, "Lakehouse")
		props, _ := body["properties"].(map[string]any)
		ep, _ := props["sqlEndpointProperties"].(map[string]any)
		if ep["connectionString"] != want {
			t.Errorf("Host %q -> %v, want %q", host, ep["connectionString"], want)
		}
	}
}

// The contract-only stack (no sqlserver sidecar) is supported: say nothing rather
// than advertise an endpoint that is not listening.
func TestLakehouseSaysNothingWithoutASQLEndpoint(t *testing.T) {
	a, st := newAPI(t)
	a.SQLEndpointPort = "" // FABRIC_SQL_TDS_ADDR unset
	ws := seedWorkspace(t, st)
	lake := seedItem(t, st, ws.ID, "Lakehouse", "lake")

	_, body := typedItemBody(t, a, "localhost:9443", ws.ID, lake.ID, "Lakehouse")
	if props, ok := body["properties"]; ok {
		t.Fatalf("a lakehouse with no SQL endpoint advertised properties: %v", props)
	}
}
