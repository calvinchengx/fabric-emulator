package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// EVERY typed collection must expose EVERY verb the REST reference prints for
// it — checked by enumeration, not by inspection.
//
// WHY THIS EXISTS. `getDefinition` and `updateDefinition` were unrouted on all
// ~25 typed collections, and their handlers were tested the whole time: the
// unit tests call `do(a.getDefinition, …)` against a fabricated request to
// `/x`, so they prove the HANDLER and say nothing about whether any URL reaches
// it. There are 404 such direct call sites in `internal/api`. A client
// following Microsoft's own article — which prints
// `POST …/copyJobs/{copyJobId}/getDefinition` — got Go's unrouted 404 while
// every test stayed green.
//
// The shape generalises: two lists that must agree (registered routes, and the
// URLs the docs print) with nothing checking that they do, failing silently
// because the half everyone tests is the half that works. The fix for that
// shape is always the same — enumerate one list and diff it against the other.
//
// The assertion is deliberately NOT on status codes. A route may legitimately
// answer 200, 400 or a JSON 404; what it may never do is fall through to the
// mux's default, which is plain-text "404 page not found". That string is the
// signal for "no route", and it is the only thing this test treats as failure.
func TestEveryTypedCollectionExposesTheDocumentedVerbs(t *testing.T) {
	f := newFixture(t)
	var ws struct{ ID string }
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "routes"}, &ws)

	// The collections, taken from the emulator's own map so a newly added one
	// is covered the day it is added rather than the day someone remembers.
	collections := []string{
		"notebooks", "lakehouses", "warehouses", "dataPipelines", "semanticModels",
		"reports", "environments", "eventhouses", "kqlDatabases", "sparkJobDefinitions",
		"mirroredDatabases", "eventstreams", "sqlDatabases", "apacheAirflowJobs",
		"dataflows", "mlExperiments", "mlModels", "copyJobs", "kqlDashboards",
		"kqlQuerysets", "reflexes", "warehouseSnapshots",
	}

	unrouted := func(method, path string) bool {
		t.Helper()
		req, err := http.NewRequest(method, f.fabric.URL+path, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+f.token)
		resp, err := f.fabric.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		// "No route" arrives in TWO shapes, and an earlier version of this
		// test only knew one — which its own mutation run caught. Go's
		// ServeMux default is plain text, but the emulator also mounts a JSON
		// catch-all answering `UnknownEndpoint`, and which one you get depends
		// on the method. Checking body SHAPE therefore saw missing POST routes
		// and was blind to missing GET routes. Both are checked now, by
		// MEANING rather than by format.
		if resp.StatusCode != http.StatusNotFound {
			return false
		}
		var body struct {
			Error struct{ Code string } `json:"error"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			return true // plain text: Go's own unrouted default
		}
		return body.Error.Code == "UnknownEndpoint"
	}

	for _, coll := range collections {
		t.Run(coll, func(t *testing.T) {
			base := "/v1/workspaces/" + ws.ID + "/" + coll
			var it struct{ ID string }
			f.call("POST", base, f.token, map[string]any{"displayName": coll + "-probe"}, &it)
			if it.ID == "" {
				t.Fatalf("could not create a %s to probe with", coll)
			}
			for _, r := range []struct{ method, path string }{
				{"GET", base},
				{"POST", base},
				{"GET", base + "/" + it.ID},
				{"PATCH", base + "/" + it.ID},
				{"POST", base + "/" + it.ID + "/getDefinition"},
				{"POST", base + "/" + it.ID + "/updateDefinition"},
				{"GET", base + "/" + it.ID + "/jobs/instances/00000000-0000-0000-0000-000000000000"},
				{"POST", base + "/" + it.ID + "/jobs/instances/00000000-0000-0000-0000-000000000000/cancel"},
				{"DELETE", base + "/" + it.ID},
			} {
				if unrouted(r.method, r.path) {
					t.Errorf("NO ROUTE: %s %s", r.method,
						strings.Replace(r.path, ws.ID, "{wid}", 1))
				}
			}
		})
	}
}
