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

	cfg = testConfig(t)
	cfg.AirflowURL = "://bad"
	cfg.AirflowDAGDir = t.TempDir()
	if _, err := server.New(cfg, nil); err == nil {
		t.Error("New with a bad AirflowURL succeeded")
	}

	cfg = testConfig(t)
	cfg.AirflowURL = "http://airflow:8080"
	if _, err := server.New(cfg, nil); err == nil {
		t.Error("New without an AirflowDAGDir succeeded")
	}

	cfg = testConfig(t)
	cfg.KQLURL = "://bad"
	if _, err := server.New(cfg, nil); err == nil {
		t.Error("New with a bad KQLURL succeeded")
	}

	cfg = testConfig(t)
	cfg.MLflowURL = "://bad"
	if _, err := server.New(cfg, nil); err == nil {
		t.Error("New with a bad MLflowURL succeeded")
	}
}

func TestNewWiresMLflowBackend(t *testing.T) {
	cfg := testConfig(t)
	cfg.MLflowURL = "http://mlflow:5000"
	srv, err := server.New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if srv.API.MLflowURL == nil || srv.API.MLflowURL.String() != cfg.MLflowURL || srv.API.MLflowHTTP == nil {
		t.Fatalf("MLflow backend was not wired: %+v", srv.API.MLflowURL)
	}
}

func TestNewWiresAirflowBackend(t *testing.T) {
	cfg := testConfig(t)
	cfg.AirflowURL = "http://airflow:8080"
	cfg.AirflowDAGDir = t.TempDir()
	cfg.AirflowUsername = "fabric"
	cfg.AirflowPassword = "secret"
	srv, err := server.New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if srv.API.Airflow == nil {
		t.Fatal("Airflow backend was not wired")
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

	// TDS without a relay backend answers the stub.
	cfg2 := testConfig(t)
	cfg2.SQLTDSAddr = ":0"
	stub, err := server.New(cfg2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stub.Close()
	if stub.TDS == nil || stub.TDS.Backend != nil {
		t.Errorf("stub TDS wiring: %+v", stub.TDS)
	}
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

// TestUnknownAPIPathIsJSON404: an unrouted path under an API prefix must
// answer as an API — a Fabric-shaped JSON 404 — never the portal SPA.
//
// Regression for a real client failure: Azure PowerShell probed
// /metadata/endpoints, got 200 text/html from the SPA fallback, and died with
// "Unexpected character encountered while parsing value: <", which says
// nothing about the actual problem. A 404 is survivable; 200 HTML is not.
func TestUnknownAPIPathIsJSON404(t *testing.T) {
	srv, err := server.New(testConfig(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	h := srv.Handler()

	for _, path := range []string{
		"/v1/nonsense",
		"/v1/deploymentPipelines/x/notARoute",
		"/v1",
		"/metadata/endpoints",
		"/subscriptions/abc",
		"/_emulator/not-a-thing",
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, w.Code)
		}
		if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Errorf("GET %s Content-Type = %q, want JSON", path, ct)
		}
		if body := w.Body.String(); strings.Contains(body, "<!doctype html") {
			t.Errorf("GET %s served the SPA shell", path)
		}
		var env struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Errorf("GET %s body is not parseable JSON: %v", path, err)
		} else if env.Error.Code == "" {
			t.Errorf("GET %s has no error code: %s", path, w.Body)
		}
	}

	// The portal itself still serves its SPA for non-API deep links.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/some/portal/deep/link", nil))
	if w.Code != http.StatusOK {
		t.Errorf("portal deep link = %d, want the SPA shell", w.Code)
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
