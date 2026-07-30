package server_test

// Host-routed handler dispatch (the onelake.blob./onelake. surfaces and the
// azurite-style /onelake account-prefixed path), plus New's wiring branches
// that need no live backends: bad Spark URLs fail loudly and a warehouse SQL
// backend wires lazily (go-mssqldb opens pools without dialing).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/config"
	"github.com/calvinchengx/fabric-emulator/internal/server"
)

// TestHandlerHostRouting: each Host/path spelling reaches a OneLake surface —
// unauthenticated requests draw the data plane's 401 challenge, not the
// control plane's SPA fallback or JSON errors.
func TestHandlerHostRouting(t *testing.T) {
	f := newFixture(t)

	cases := []struct {
		name string
		host string
		path string
	}{
		{"blob host", "onelake.blob.fabric.localhost", "/ws/item"},
		{"dfs host", "onelake.dfs.fabric.localhost", "/ws/item/Files/x"},
		{"account-prefixed path", "", "/onelake/ws/item"},
		{"account-prefixed root", "", "/onelake"},
	}
	for _, tc := range cases {
		req, err := http.NewRequest("GET", f.fabric.URL+tc.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if tc.host != "" {
			req.Host = tc.host
		}
		resp, err := f.fabric.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status = %d; want 401 from the OneLake surface", tc.name, resp.StatusCode)
		}
	}
}

// testConfig builds a minimal valid config that never contacts entra (JWKS is
// fetched lazily, on the first bearer validation).
func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := &config.Config{EntraIssuer: "https://entra.invalid/tid/v2.0"}
	if err := cfg.Finish(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestNewRejectsBadEngineURLs(t *testing.T) {
	cfg := testConfig(t)
	cfg.SparkAgentURL = "://bad"
	if _, err := server.New(cfg, nil); err == nil {
		t.Error("New with a bad SparkAgentURL succeeded")
	}

	cfg = testConfig(t)
	cfg.SparkLivyURL = "://bad"
	if _, err := server.New(cfg, nil); err == nil {
		t.Error("New with a bad SparkLivyURL succeeded")
	}

	cfg = testConfig(t)
	cfg.SQLTDSAddr = ":0"
	cfg.WarehouseSQLURL = "sqlserver://sa@localhost:notaport"
	if _, err := server.New(cfg, nil); err == nil {
		t.Error("New with a bad WarehouseSQLURL succeeded")
	}
}

// TestNewWiresWarehouseSQLBackend: with SQLTDSAddr + WarehouseSQLURL set, New
// builds the TDS endpoint with a relay backend and hooks the mirror + pipeline
// SQL integration — all lazily, no SQL Server dialed.
func TestNewWiresWarehouseSQLBackend(t *testing.T) {
	cfg := testConfig(t)
	cfg.SQLTDSAddr = ":0"
	cfg.WarehouseSQLURL = "sqlserver://sa:pass@127.0.0.1:1433?database=master"
	srv, err := server.New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if srv.TDS == nil || srv.TDS.Backend == nil || srv.TDS.OnConnect == nil {
		t.Fatalf("TDS wiring incomplete: %+v", srv.TDS)
	}
	if srv.API.MirrorItem == nil || srv.API.SQLDB == nil {
		t.Error("mirror/pipeline SQL hooks not wired")
	}
	if state := warehouseState(t, srv); state != "relay" {
		t.Errorf("portal warehouse state = %q, want relay", state)
	}

	// TDS without a relay backend answers the stub.
	cfg2 := testConfig(t)
	cfg2.SQLTDSAddr = ":0"
	stub, err := server.New(cfg2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stub.Close()
	if state := warehouseState(t, stub); state != "stub" {
		t.Errorf("portal warehouse state = %q, want stub", state)
	}
}

// warehouseState reads the portal's warehouse wiring report.
func warehouseState(t *testing.T, srv *server.Server) string {
	t.Helper()
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/_emulator/portal/warehouse", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("portal warehouse = %d", w.Code)
	}
	var body struct {
		TDSListener string `json:"tdsListener"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.TDSListener
}

// TestPortalNonGETFallsThrough: the SPA fallback serves only GET/HEAD; other
// methods 404 rather than answering with the shell.
func TestPortalNonGETFallsThrough(t *testing.T) {
	f := newFixture(t)
	resp, err := http.Post(f.fabric.URL+"/some/spa/route", "text/plain", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST to SPA route = %d; want 404", resp.StatusCode)
	}
}

// TestPortalOperationsListsRows: an async item create enqueues an LRO that the
// portal operations view then renders.
func TestPortalOperationsListsRows(t *testing.T) {
	f := newFixture(t)
	var ws struct{ ID string }
	f.mustStatus(f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "w"}, &ws),
		http.StatusCreated, "create workspace")
	req := map[string]any{
		"displayName": "nb", "type": "Notebook",
		"definition": map[string]any{"parts": []map[string]string{
			{"path": ".platform", "payload": "e30=", "payloadType": "InlineBase64"},
		}},
	}
	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/items", f.token, req, nil),
		http.StatusAccepted, "async create")

	var ops struct {
		Value []struct{ ID, Kind, Status string } `json:"value"`
	}
	if code := f.portalJSON(t, "/_emulator/portal/operations", &ops); code != http.StatusOK {
		t.Fatalf("portal operations: %d", code)
	}
	if len(ops.Value) == 0 || ops.Value[0].ID == "" || ops.Value[0].Kind == "" {
		t.Fatalf("operations rows = %+v", ops.Value)
	}
}

// TestPortalStoreErrors: a closed store surfaces as 500s on every portal data
// endpoint (not panics, not empty 200s).
func TestPortalStoreErrors(t *testing.T) {
	srv, err := server.New(testConfig(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.Close() // every subsequent store call errors
	h := srv.Handler()
	for _, path := range []string{
		"/_emulator/portal/workspaces",
		"/_emulator/portal/workspaces/some-id",
		"/_emulator/portal/operations",
		"/_emulator/portal/connections",
		"/_emulator/portal/shortcuts",
		"/_emulator/portal/capacities",
		"/_emulator/portal/jobs",
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != http.StatusInternalServerError {
			t.Errorf("GET %s on a closed store = %d; want 500", path, w.Code)
		}
	}
}
