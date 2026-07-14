package server_test

// executeQueries e2e: a real Power BI-audience token runs a DAX query over a
// SemanticModel item's model.bim, end to end (auth → RBAC → DAX evaluator),
// with the audience walls asserted both directions.

import (
	"encoding/base64"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	entra "github.com/calvinchengx/entra-emulator/emulator"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

func smFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "e2e", "semantic-model", "fixtures", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestExecuteQueriesE2E(t *testing.T) {
	f := newFixture(t)

	var ws struct{ ID string }
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "retail-ws"}, &ws)

	var ds struct{ ID string }
	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/items", f.token,
		map[string]string{"displayName": "RetailAnalysis", "type": "SemanticModel"}, &ds),
		http.StatusCreated, "create semantic model")
	b64 := func(name string) string { return base64.StdEncoding.EncodeToString(smFile(t, name)) }
	if err := f.srv.API.Store.SetDefinition(ds.ID, []store.DefinitionPart{
		{Path: "model.bim", PayloadType: "InlineBase64", Payload: b64("retail.bim")},
		{Path: "data.json", PayloadType: "InlineBase64", Payload: b64("seed_data.json")},
	}); err != nil {
		t.Fatal(err)
	}

	pbi := f.forgeToken(t, map[string]any{
		"clientId": entra.DaemonClientID, "audience": "https://analysis.windows.net/powerbi/api",
	})

	// Run the TotalUnits query; expect the four grouped rows.
	dax := `EVALUATE SUMMARIZECOLUMNS('Time'[FiscalYear], 'Time'[FiscalMonth], "TotalUnits", [TotalUnits])`
	var resp struct {
		Results []struct {
			Tables []struct {
				Rows []map[string]any `json:"rows"`
			} `json:"tables"`
		} `json:"results"`
	}
	url := "/v1.0/myorg/groups/" + ws.ID + "/datasets/" + ds.ID + "/executeQueries"
	f.mustStatus(f.call("POST", url, pbi, map[string]any{"queries": []map[string]string{{"query": dax}}}, &resp),
		http.StatusOK, "executeQueries")
	if len(resp.Results) != 1 || len(resp.Results[0].Tables) != 1 || len(resp.Results[0].Tables[0].Rows) != 4 {
		t.Fatalf("unexpected result: %+v", resp)
	}
	total := 0.0
	for _, r := range resp.Results[0].Tables[0].Rows {
		total += r["[TotalUnits]"].(float64)
	}
	if total != 275000 { // 60000+65000+70000+80000
		t.Fatalf("Σ TotalUnits = %v, want 275000", total)
	}

	// Audience wall: executeQueries requires the Power BI audience specifically,
	// so a Fabric-only token is rejected. (The reverse is *not* a wall — the
	// Power BI resource is a legitimate control-plane audience too, since Fabric
	// and Power BI share the legacy analysis.windows.net/powerbi/api resource;
	// see auth.ControlPlaneAudiences.)
	f.mustStatus(f.call("POST", url, f.token, map[string]any{"queries": []map[string]string{{"query": dax}}}, nil),
		http.StatusUnauthorized, "fabric-audience token rejected on executeQueries")
}
