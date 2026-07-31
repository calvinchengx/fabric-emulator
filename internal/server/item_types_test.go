package server_test

import (
	"net/http"
	"testing"
)

// Real Fabric rejects a type outside the documented ItemType enumeration with
// InvalidItemType, and accepts the ones inside it.
func TestItemTypeValidation(t *testing.T) {
	f := newFixture(t)
	var ws struct{ ID string }
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "types"}, &ws)
	items := "/v1/workspaces/" + ws.ID + "/items"

	for _, bad := range []string{"NotAnItemType", "Lakehous", "Note book", "notebook!"} {
		var errBody struct {
			ErrorCode string `json:"errorCode"`
		}
		resp := f.call("POST", items, f.token,
			map[string]any{"displayName": "x-" + bad, "type": bad}, &errBody)
		f.mustStatus(resp, http.StatusBadRequest, "reject "+bad)
		if errBody.ErrorCode != "InvalidItemType" {
			t.Fatalf("errorCode for %q = %q, want InvalidItemType", bad, errBody.ErrorCode)
		}
	}

	// Types that exist in the enum but had no typed collection are accepted
	// through the generic surface.
	for _, good := range []string{"GraphQLApi", "VariableLibrary", "UserDataFunction",
		"DigitalTwinBuilder", "MountedDataFactory", "GraphModel", "Ontology", "DataAgent"} {
		var created struct{ ID, Type string }
		f.mustStatus(f.call("POST", items, f.token,
			map[string]any{"displayName": "ok-" + good, "type": good}, &created),
			http.StatusCreated, "accept "+good)
		if created.Type != good {
			t.Fatalf("stored type = %q, want %q", created.Type, good)
		}
	}
}

// Type matching is case-insensitive, and the canonical spelling is stored:
// otherwise `notebook` and `Notebook` would be two types, and the
// per-(workspace, type) display-name uniqueness rule would silently not hold.
func TestItemTypeIsCanonicalisedNotDuplicated(t *testing.T) {
	f := newFixture(t)
	var ws struct{ ID string }
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "casing"}, &ws)
	items := "/v1/workspaces/" + ws.ID + "/items"

	var created struct{ ID, Type string }
	f.mustStatus(f.call("POST", items, f.token,
		map[string]any{"displayName": "shared", "type": "notebook"}, &created),
		http.StatusCreated, "lowercase type")
	if created.Type != "Notebook" {
		t.Fatalf("type = %q, want the canonical Notebook", created.Type)
	}

	// The same name under a differently-cased spelling of the same type is a
	// conflict, not a second item.
	f.mustStatus(f.call("POST", items, f.token,
		map[string]any{"displayName": "shared", "type": "NOTEBOOK"}, nil),
		http.StatusConflict, "same type, different casing")

	// And it is reachable through the canonical typed collection.
	f.mustStatus(f.call("GET", "/v1/workspaces/"+ws.ID+"/notebooks/"+created.ID, f.token, nil, nil),
		http.StatusOK, "typed get after canonicalisation")
}

// The two collections whose segments were taken from their own reference
// pages — note GraphQLApis is capitalised where variableLibraries is not, so
// these cannot be generated from the type name.
func TestTypedCollectionsWithNonDerivableSegments(t *testing.T) {
	f := newFixture(t)
	var ws struct{ ID string }
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "segments"}, &ws)

	for collection, itemType := range map[string]string{
		"GraphQLApis":       "GraphQLApi",
		"variableLibraries": "VariableLibrary",
	} {
		t.Run(collection, func(t *testing.T) {
			base := "/v1/workspaces/" + ws.ID + "/" + collection
			var created struct{ ID, Type string }
			f.mustStatus(f.call("POST", base, f.token,
				map[string]any{"displayName": collection + "-item"}, &created),
				http.StatusCreated, "typed create")
			if created.Type != itemType {
				t.Fatalf("type = %q, want %q", created.Type, itemType)
			}
			f.mustStatus(f.call("GET", base+"/"+created.ID, f.token, nil, nil),
				http.StatusOK, "typed get")
			var listed struct{ Value []struct{ Type string } }
			f.mustStatus(f.call("GET", base, f.token, nil, &listed), http.StatusOK, "typed list")
			if len(listed.Value) != 1 || listed.Value[0].Type != itemType {
				t.Fatalf("list = %+v", listed.Value)
			}
		})
	}
}
