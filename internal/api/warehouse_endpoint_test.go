package api

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

func TestSQLPortOf(t *testing.T) {
	for addr, want := range map[string]string{
		":1433":            "1433",
		"0.0.0.0:1433":     "1433",
		"127.0.0.1:11533":  "11533",
		"1433":             "1433", // a bare port is a natural thing to write
		"":                 "",
		"[::]:1433":        "1433",
		"not-an-address:":  "",
		"host:port:extra:": "",
	} {
		if got := SQLPortOf(addr); got != want {
			t.Errorf("SQLPortOf(%q) = %q, want %q", addr, got, want)
		}
	}
}

// getItem fetches one item through the API and returns the decoded body.
func getItemBody(t *testing.T, a *API, host, wid, iid string) map[string]any {
	t.Helper()
	r := httptest.NewRequest("GET", "/x", nil)
	r.Host = host
	r.SetPathValue("wid", wid)
	r.SetPathValue("iid", iid)
	w := httptest.NewRecorder()
	a.getItem(w, r, admin)
	if w.Code != 200 {
		t.Fatalf("getItem = %d %s", w.Code, w.Body.Bytes())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func warehouseConnStr(t *testing.T, a *API, host, wid, iid string) (string, bool) {
	t.Helper()
	body := getItemBody(t, a, host, wid, iid)
	props, ok := body["properties"].(map[string]any)
	if !ok {
		return "", false
	}
	cs, ok := props["connectionString"].(string)
	return cs, ok
}

// TestWarehouseAdvertisesAConnectionStringTheCallerCanDial.
//
// The address is echoed from the request rather than configured, because the
// right answer depends on where the caller is standing: a container on the
// compose network reaches this emulator as `fabric-emulator`, a laptop reaches
// the same emulator as `localhost`. One configured hostname would be wrong for
// one of them — and wrong as a connection timeout, not as a visible bad answer.
func TestWarehouseAdvertisesAConnectionStringTheCallerCanDial(t *testing.T) {
	a, st := newAPI(t)
	a.SQLEndpointPort = "1433"
	ws := seedWorkspace(t, st)
	wh := &store.Item{WorkspaceID: ws.ID, Type: "Warehouse", DisplayName: "gold"}
	if err := st.CreateItem(wh, nil); err != nil {
		t.Fatal(err)
	}

	for host, want := range map[string]string{
		"fabric-emulator:9443": "fabric-emulator:1433",
		"localhost:9443":       "localhost:1433",
		// The HTTP port must not leak into the SQL address: they are different
		// listeners, and the port the client used to reach REST says nothing
		// about the one it should dial for TDS.
		"localhost:19543": "localhost:1433",
		"fabric.example":  "fabric.example:1433",
	} {
		got, ok := warehouseConnStr(t, a, host, ws.ID, wh.ID)
		if !ok || got != want {
			t.Errorf("Host %q -> %q (present=%v), want %q", host, got, ok, want)
		}
	}
}

// TestWarehouseReportsNoConnectionStringWithoutASQLEndpoint.
//
// A blank-but-present property would read as "this warehouse has no address",
// which is a different and wronger claim than "this build serves no SQL". The
// contract-only stack (no sqlserver sidecar) is a supported configuration, and
// it should say nothing rather than something false.
func TestWarehouseReportsNoConnectionStringWithoutASQLEndpoint(t *testing.T) {
	a, st := newAPI(t)
	a.SQLEndpointPort = "" // FABRIC_SQL_TDS_ADDR unset
	ws := seedWorkspace(t, st)
	wh := &store.Item{WorkspaceID: ws.ID, Type: "Warehouse", DisplayName: "gold"}
	if err := st.CreateItem(wh, nil); err != nil {
		t.Fatal(err)
	}

	body := getItemBody(t, a, "localhost:9443", ws.ID, wh.ID)
	if props, ok := body["properties"]; ok {
		t.Fatalf("a warehouse with no SQL endpoint advertised properties: %v", props)
	}
}

// TestOnlyAWarehouseGetsAConnectionString: the property is type-specific, and a
// Lakehouse's SQL analytics endpoint is a separate surface with its own name.
func TestOnlyAWarehouseGetsAConnectionString(t *testing.T) {
	a, st := newAPI(t)
	a.SQLEndpointPort = "1433"
	ws := seedWorkspace(t, st)
	lake := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lake"}
	if err := st.CreateItem(lake, nil); err != nil {
		t.Fatal(err)
	}

	if cs, ok := warehouseConnStr(t, a, "localhost:9443", ws.ID, lake.ID); ok {
		t.Fatalf("a Lakehouse advertised a warehouse connectionString: %q", cs)
	}
}
