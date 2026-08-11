package api

// What a real tenant refuses in a VariableLibrary definition.
//
// MEASURED 2026-08-11 against a Fabric trial. Both rejections below were
// produced by POSTing the payload and reading the failed operation, not
// inferred from a schema — the published schema for variables.json constrains
// `type` only to `^[A-Za-z][A-Za-z0-9]{0,63}$` and declares `value: true` (any
// JSON), so it cannot answer either question.
//
// WHY THE NONSENSE TYPE IS THE LOAD-BEARING CASE. Sending only plausible types
// and watching them succeed would have proved nothing: a create that ignores
// `type` entirely accepts them too, and a round-trip would echo our own guesses
// back as if the tenant had confirmed them. The tenant naming
// `TotallyMadeUpType` and nothing else is what makes the acceptance of the
// others evidence. Any probe for "is this validated" needs a control that must
// fail.

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// varLibBody builds a createItem payload carrying one variables.json.
func varLibBody(t *testing.T, name string, variables ...map[string]any) string {
	t.Helper()
	doc, err := json.Marshal(map[string]any{"variables": variables})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{
		"displayName": name, "type": "VariableLibrary",
		"definition": map[string]any{"parts": []map[string]any{{
			"path": "variables.json", "payloadType": "InlineBase64",
			"payload": base64.StdEncoding.EncodeToString(doc),
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestVariableLibraryRefusesATypeTheTenantRefuses(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)

	// The eight types a tenant accepted, in one library. Values are the shapes
	// it stored them as, read back with getDefinition: Integer is a JSON
	// number, Boolean a JSON bool, DateTime a string, and ConnectionReference
	// an object carrying a connection id.
	conn := &store.Connection{DisplayName: "kv", ConnectivityType: "ShareableCloud"}
	if err := st.CreateConnection(conn); err != nil {
		t.Fatal(err)
	}
	ok := varLibBody(t, "accepted",
		map[string]any{"name": "s", "type": "String", "value": "x"},
		map[string]any{"name": "g", "type": "Guid", "value": "11111111-2222-3333-4444-555555555555"},
		map[string]any{"name": "d", "type": "DateTime", "value": "2026-08-11T00:00:00Z"},
		map[string]any{"name": "i", "type": "Integer", "value": 42},
		map[string]any{"name": "n", "type": "Number", "value": 1.5},
		map[string]any{"name": "b", "type": "Boolean", "value": true},
		map[string]any{"name": "ir", "type": "ItemReference",
			"value": map[string]any{"itemId": "a", "workspaceId": ws.ID}},
		map[string]any{"name": "cr", "type": "ConnectionReference",
			"value": map[string]any{"connectionId": conn.ID}},
	)
	// 201 or 202: createItem is an LRO surface, so a success is whichever the
	// stack is configured for. What matters here is that it is not a rejection.
	if w := do(a.createItem, admin, "POST", ok, map[string]string{"wid": ws.ID}); w.Code >= 300 {
		t.Fatalf("every tenant-accepted type = %d %s", w.Code, w.Body.Bytes())
	}

	// The control. Without this the case above is vacuous.
	bad := varLibBody(t, "rejected",
		map[string]any{"name": "s", "type": "String", "value": "x"},
		map[string]any{"name": "junk", "type": "TotallyMadeUpType", "value": "x"},
	)
	w := do(a.createItem, admin, "POST", bad, map[string]string{"wid": ws.ID})
	if w.Code != http.StatusBadRequest || errorCode(t, w) != "InvalidVariableType" {
		t.Fatalf("nonsense type = %d %s; want 400 InvalidVariableType", w.Code, w.Body.Bytes())
	}
	// The message names the offending type: "not supported" alone would leave a
	// reader diffing eight declarations by hand.
	if body := w.Body.String(); !containsAll(body, "TotallyMadeUpType", "not supported") {
		t.Errorf("message does not name the type: %s", body)
	}
}

// A ConnectionReference's value is a GUID, and the tenant checks it resolves.
// That is the half that makes a library environment-BOUND: promoting one
// between workspaces carries an id that must exist on the far side.
func TestVariableLibraryRefusesAConnectionThatDoesNotExist(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)

	dangling := varLibBody(t, "dangling",
		map[string]any{"name": "cr", "type": "ConnectionReference",
			"value": map[string]any{"connectionId": "00000000-1111-2222-3333-444444444444"}},
	)
	w := do(a.createItem, admin, "POST", dangling, map[string]string{"wid": ws.ID})
	if w.Code != http.StatusBadRequest || errorCode(t, w) != "InvalidContent" {
		t.Fatalf("dangling connectionId = %d %s; want 400 InvalidContent",
			w.Code, w.Body.Bytes())
	}

	// The same library with a connection that exists is accepted — so the
	// refusal above is about resolution, not about the shape.
	conn := &store.Connection{DisplayName: "real", ConnectivityType: "ShareableCloud"}
	if err := st.CreateConnection(conn); err != nil {
		t.Fatal(err)
	}
	good := varLibBody(t, "resolvable",
		map[string]any{"name": "cr", "type": "ConnectionReference",
			"value": map[string]any{"connectionId": conn.ID}},
	)
	if w := do(a.createItem, admin, "POST", good, map[string]string{"wid": ws.ID}); w.Code >= 300 {
		t.Fatalf("resolvable connectionId = %d %s", w.Code, w.Body.Bytes())
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
