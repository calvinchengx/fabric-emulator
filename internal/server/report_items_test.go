package server_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"
)

// A Power BI Report is DEFINITION-SHAPED, and that is the whole claim.
//
// parity.md grades reports 🟢 for definition round-trip while stating that
// RENDERING is deliberately absent — a report is drawn client-side from a
// definition plus query results, so there is no server-side renderer in Fabric
// for the emulator to be unfaithful to. That makes the round-trip the entire
// server-side contract, and an ungrounded 🟢 on it would be the overclaim this
// map exists to prevent. Hence this test: the bytes a client PUTs are the bytes
// it GETs back, per part, through the typed `reports` collection.
//
// The definition shape is Fabric's PBIR: `report.json` carries the visual
// layout and `definition.pbir` binds the report to its semantic model. Neither
// is interpreted here, which is the point — an emulator that PARSED a report
// definition would be claiming knowledge of a format whose renderer it does
// not have.
func TestReportDefinitionRoundTripsByteForByte(t *testing.T) {
	f := newFixture(t)
	var ws struct{ ID string }
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "reports-ws"}, &ws)

	layout := []byte(`{"$schema":"https://developer.microsoft.com/json-schemas/fabric/item/report/definition/report/1.0.0/schema.json","sections":[{"name":"page1","visualContainers":[]}]}`)
	binding := []byte(`{"version":"1.0","datasetReference":{"byPath":{"path":"../sales.SemanticModel"}}}`)
	parts := []map[string]string{
		{"path": "report.json", "payload": base64.StdEncoding.EncodeToString(layout), "payloadType": "InlineBase64"},
		{"path": "definition.pbir", "payload": base64.StdEncoding.EncodeToString(binding), "payloadType": "InlineBase64"},
	}

	base := "/v1/workspaces/" + ws.ID + "/reports"
	// Creating WITH a definition is an LRO — 202 plus an operation to poll,
	// which is Fabric's own contract and the reason this is not a plain 201.
	var created struct{ ID, Type string }
	resp := f.call("POST", base, f.token, map[string]any{
		"displayName": "sales-report",
		"definition":  map[string]any{"parts": parts},
	}, &created)
	f.mustStatus(resp, http.StatusAccepted, "create report with definition")
	opID := resp.Header.Get("x-ms-operation-id")
	if opID == "" {
		t.Fatal("202 carried no x-ms-operation-id, so the LRO cannot be followed")
	}
	var op struct{ Status string }
	f.call("GET", "/v1/operations/"+opID, f.token, nil, &op)
	if op.Status != "Succeeded" {
		t.Fatalf("create operation = %q, want Succeeded", op.Status)
	}
	// The created item comes from the operation RESULT, not the 202 body —
	// Fabric's contract, and the reason a caller polls at all.
	f.mustStatus(f.call("GET", "/v1/operations/"+opID+"/result", f.token, nil, &created),
		http.StatusOK, "operation result")
	if created.ID == "" {
		t.Fatalf("operation result carried no item id: %+v", created)
	}
	if created.Type != "Report" {
		t.Fatalf("type = %q, want Report", created.Type)
	}

	var got struct {
		Definition struct {
			Parts []struct{ Path, Payload, PayloadType string }
		}
	}
	f.mustStatus(f.call("POST", base+"/"+created.ID+"/getDefinition", f.token, nil, &got),
		http.StatusOK, "getDefinition")

	back := map[string][]byte{}
	for _, p := range got.Definition.Parts {
		raw, err := base64.StdEncoding.DecodeString(p.Payload)
		if err != nil {
			t.Fatalf("part %q payload is not base64: %v", p.Path, err)
		}
		back[p.Path] = raw
	}
	for path, want := range map[string][]byte{"report.json": layout, "definition.pbir": binding} {
		if string(back[path]) != string(want) {
			t.Fatalf("%s round-tripped as %q, want %q", path, back[path], want)
		}
	}
	// The layout must survive as PARSEABLE JSON, not merely as equal bytes:
	// a store that helpfully re-encoded would still pass a bytes check if it
	// happened to be idempotent, and would break a client that diffs the file.
	var probe map[string]any
	if err := json.Unmarshal(back["report.json"], &probe); err != nil {
		t.Fatalf("report.json is no longer valid JSON: %v", err)
	}
	if _, ok := probe["sections"]; !ok {
		t.Fatalf("report.json lost its sections: %v", probe)
	}
}

// The typed definition route must refuse another type's id. Fabric's URL names
// one item type; serving a notebook's definition from `…/reports/{id}/…` would
// be a cross-type read through the one route whose purpose is to be typed.
func TestTypedDefinitionRouteRefusesAnotherType(t *testing.T) {
	f := newFixture(t)
	var ws struct{ ID string }
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "cross"}, &ws)
	var nb struct{ ID string }
	f.call("POST", "/v1/workspaces/"+ws.ID+"/notebooks", f.token,
		map[string]any{"displayName": "nb"}, &nb)

	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/reports/"+nb.ID+"/getDefinition",
		f.token, nil, nil), http.StatusNotFound, "notebook id via the reports route")
	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/notebooks/"+nb.ID+"/getDefinition",
		f.token, nil, nil), http.StatusOK, "notebook id via its own route")
}
