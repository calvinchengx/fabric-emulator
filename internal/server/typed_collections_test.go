package server_test

import (
	"net/http"
	"testing"
)

// The typed collections added for the Tier 1 sweep. Each is an alias over the
// generic item surface, so the contract worth pinning is that the collection
// forces its type, resolves its own members, and does not resolve another
// type's — the same three properties TestDefinitionRoundTripAndTypedAliases
// pins for the original set.
func TestTypedAliasesForLaterItemTypes(t *testing.T) {
	f := newFixture(t)
	var ws struct{ ID string }
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "tier1"}, &ws)

	for collection, itemType := range map[string]string{
		"copyJobs":           "CopyJob",
		"kqlDashboards":      "KQLDashboard",
		"kqlQuerysets":       "KQLQueryset",
		"reflexes":           "Reflex",
		"warehouseSnapshots": "WarehouseSnapshot",
	} {
		t.Run(collection, func(t *testing.T) {
			base := "/v1/workspaces/" + ws.ID + "/" + collection
			// Create without a "type" in the body: the collection implies it.
			var created struct{ ID, Type string }
			f.mustStatus(f.call("POST", base, f.token,
				map[string]any{"displayName": collection + "-item"}, &created),
				http.StatusCreated, "typed create")
			if created.Type != itemType {
				t.Fatalf("type = %q, want %q", created.Type, itemType)
			}
			f.mustStatus(f.call("GET", base+"/"+created.ID, f.token, nil, nil),
				http.StatusOK, "typed get")
			// A different collection must not resolve this id.
			f.mustStatus(f.call("GET", "/v1/workspaces/"+ws.ID+"/notebooks/"+created.ID, f.token, nil, nil),
				http.StatusNotFound, "cross-type get")
			// The typed list is filtered to this type.
			var listed struct{ Value []struct{ ID, Type string } }
			f.mustStatus(f.call("GET", base, f.token, nil, &listed), http.StatusOK, "typed list")
			if len(listed.Value) != 1 || listed.Value[0].Type != itemType {
				t.Fatalf("list = %+v, want exactly one %s", listed.Value, itemType)
			}
		})
	}
}
