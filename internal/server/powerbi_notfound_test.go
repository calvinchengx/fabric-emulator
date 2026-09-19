package server_test

// An unrouted path under an API root must answer as that API, never as the
// operator portal's HTML.
//
// The guard existed for /v1 and was written because Azure PowerShell dies on
// HTML with "Unexpected character encountered while parsing value: <"
// (docs/23). The POWER BI roots were missing from it, so the same defect was
// still live one spelling over: MicrosoftPowerBIMgmt reported "Unable to
// deserialize the response" for Get-PowerBIReport, which reads like a
// serialisation bug in the reports surface rather than a route that does not
// exist. `az rest` prints the HTML and moves on, which is why nothing caught
// it before a typed client was pointed at the surface.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// unroutedPowerBIPaths are real Power BI routes this emulator does not
// implement, each one reached by a cmdlet in e2e/powerbi-ps.
var unroutedPowerBIPaths = []string{
	"/v1.0/myorg/admin/groups",            // Get-PowerBIWorkspace -Scope Organization
	"/v1.0/myorg/reports",                 // Get-PowerBIReport
	"/v1.0/myorg/groups/any-guid/reports", // Get-PowerBIReport -WorkspaceId
	"/v1.0/myorg/capacities",              // Get-PowerBICapacity
	"/powerbi/globalservice/v201606/nope", // an unknown global-service call
}

func TestAnUnroutedPowerBIPathIsNotHTML(t *testing.T) {
	f := newFixture(t)

	for _, path := range unroutedPowerBIPaths {
		t.Run(path, func(t *testing.T) {
			res, err := http.Get(f.fabric.URL + path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = res.Body.Close() }()
			body, _ := io.ReadAll(res.Body)

			// The assertion that matters is not the status: it is that a
			// client can PARSE the answer. HTML is what made the failure
			// unreadable, and the status was 200 while it did so.
			if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Fatalf("Content-Type = %q, want JSON — body begins %q",
					ct, string(body[:min(len(body), 60)]))
			}
			if strings.Contains(string(body), "<html") || strings.Contains(string(body), "<meta") {
				t.Fatalf("the SPA answered an API path: %q", string(body[:min(len(body), 120)]))
			}
			if res.StatusCode != http.StatusNotFound {
				t.Errorf("status = %d, want 404", res.StatusCode)
			}
		})
	}
}

// TestAnUnroutedPowerBIPathUsesPowerBIsOwnShape. Measured, not assumed: a
// request to the real api.powerbi.com for an unrouted path answers ASP.NET Web
// API's default envelope — a capital-M "Message", no nested error object —
// which is a different shape from Fabric's {"error":{"code","message"}}.
// Emitting Fabric's here would be a second wrong shape in place of the first.
func TestAnUnroutedPowerBIPathUsesPowerBIsOwnShape(t *testing.T) {
	f := newFixture(t)

	res, err := http.Get(f.fabric.URL + "/v1.0/myorg/reports")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()

	var got map[string]any
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	message, ok := got["Message"].(string)
	if !ok {
		t.Fatalf(`no "Message" key; got %v — Fabric's envelope is not Power BI's`, got)
	}
	if _, wrong := got["error"]; wrong {
		t.Errorf(`carries Fabric's "error" object as well as Power BI's "Message": %v`, got)
	}
	// The real service echoes the URI, and it is the part that says WHICH
	// request went unrouted when several are in flight.
	if !strings.Contains(message, "/v1.0/myorg/reports") {
		t.Errorf("message does not name the request URI: %q", message)
	}
}

// TestAFabricPathKeepsFabricsShape — the Power BI branch must not have been
// applied to the whole server. /v1 is a different product with a different
// envelope, and clients parse both.
func TestAFabricPathKeepsFabricsShape(t *testing.T) {
	f := newFixture(t)

	res, err := http.Get(f.fabric.URL + "/v1/no-such-route")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()

	var got struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if got.Error.Code != "UnknownEndpoint" {
		t.Errorf("code = %q, want UnknownEndpoint — the Fabric envelope is unchanged", got.Error.Code)
	}
}

// TestThePortalStillAnswersHTML. The guard is scoped to API roots, and a
// portal route that started returning JSON would be this fix overreaching —
// the SPA is what a browser is meant to get.
func TestThePortalStillAnswersHTML(t *testing.T) {
	f := newFixture(t)

	res, err := http.Get(f.fabric.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(res.Body)

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if !strings.Contains(strings.ToLower(string(body)), "<html") {
		t.Errorf("the portal root no longer serves the SPA: %q", string(body[:min(len(body), 120)]))
	}
}

// TestAnUnroutedAPIPathAnswersTheAPIShapeForEveryVerb is the half the original
// guard missed.
//
// The catch-all rejected any non-GET/HEAD request with http.NotFound BEFORE it
// asked whether the path was an API path, so every write verb to an
// unimplemented route answered Go's default plaintext body, "404 page not
// found". That is not JSON, so no typed client can deserialise it -- the exact
// failure the /v1 guard was written to stop, left live for four verbs out of
// six.
//
// Found by asserting Power BI's Imports surface, whose only non-GET operations
// are POSTs: POST /v1.0/myorg/imports/createTemporaryUploadLocation answered
// "404 page not found\n" and took an e2e harness down inside json.loads.
func TestAnUnroutedAPIPathAnswersTheAPIShapeForEveryVerb(t *testing.T) {
	f := newFixture(t)

	cases := []struct {
		path string
		// The Power BI roots answer ASP.NET's envelope; /v1 answers Fabric's.
		wantKey string
	}{
		{"/v1.0/myorg/imports/createTemporaryUploadLocation", "Message"},
		{"/v1.0/myorg/imports", "Message"},
		{"/v1/nothing/here", "error"},
	}
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		for _, c := range cases {
			t.Run(method+" "+c.path, func(t *testing.T) {
				req, err := http.NewRequest(method, f.fabric.URL+c.path, strings.NewReader("{}"))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Content-Type", "application/json")
				res, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer res.Body.Close()
				body, _ := io.ReadAll(res.Body)
				if res.StatusCode != http.StatusNotFound {
					t.Fatalf("status = %d, want 404; body %q", res.StatusCode, body)
				}
				var got map[string]any
				if err := json.Unmarshal(body, &got); err != nil {
					t.Fatalf("body is not JSON, so no typed client can read it: %q", body)
				}
				if _, ok := got[c.wantKey]; !ok {
					t.Errorf("body %q has no %q key — wrong envelope for this root", body, c.wantKey)
				}
			})
		}
	}
}
