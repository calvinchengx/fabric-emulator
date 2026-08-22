package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// dataAccessRoles is the authoring half of OneLake security. The assertions
// that matter are the refusals: who may write, what a PUT does to roles it did
// not mention, and what happens to a payload that disagrees with itself. A
// suite that only stored and read back a role would pass against a handler
// that ignored permissions entirely.

const roleBody = `{"name":"readers","decisionRules":[{"effect":"Permit","permission":[
  {"attributeName":"Path","attributeValueIncludedIn":["Tables/dbo/Customers"]},
  {"attributeName":"Action","attributeValueIncludedIn":["Read"]}]}],
  "members":{"microsoftEntraMembers":[{"objectId":"11111111-1111-1111-1111-111111111111"}]}}`

func secItem(t *testing.T, a *API, st *store.Store) (*store.Workspace, *store.Item) {
	t.Helper()
	ws := seedWorkspace(t, st)
	it := &store.Item{WorkspaceID: ws.ID, DisplayName: "lake", Type: "Lakehouse"}
	if err := st.CreateItem(it, nil); err != nil {
		t.Fatal(err)
	}
	return ws, it
}

func secPV(ws *store.Workspace, it *store.Item) map[string]string {
	return map[string]string{"wid": ws.ID, "iid": it.ID}
}

func TestDataAccessRolesRoundTrip(t *testing.T) {
	a, st := newAPI(t)
	ws, it := secItem(t, a, st)

	put := do(a.putDataAccessRoles, admin, "PUT", `{"value":[`+roleBody+`]}`, secPV(ws, it))
	if put.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", put.Code, put.Body)
	}
	// The PUT answers with the stored set, so a client need not re-read.
	var body struct{ Value []json.RawMessage }
	if err := json.Unmarshal(put.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Value) != 1 {
		t.Fatalf("PUT returned %d roles", len(body.Value))
	}

	get := do(a.listDataAccessRoles, admin, "GET", "", secPV(ws, it))
	if get.Code != http.StatusOK {
		t.Fatalf("GET = %d %s", get.Code, get.Body)
	}
	var back struct {
		Value []struct {
			Name    string `json:"name"`
			Members struct {
				Entra []struct {
					ObjectID string `json:"objectId"`
				} `json:"microsoftEntraMembers"`
			} `json:"members"`
		}
	}
	if err := json.Unmarshal(get.Body.Bytes(), &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Value) != 1 || back.Value[0].Name != "readers" {
		t.Fatalf("GET = %s", get.Body)
	}
	// The member survived: a role whose members were dropped grants nothing and
	// would look like a policy that simply does not work.
	if len(back.Value[0].Members.Entra) != 1 {
		t.Fatalf("members were dropped: %s", get.Body)
	}
}

// "This API updates role definitions by creating, updating, and deleting roles
// to match the payload you send." A merge would leave a revoked role live.
func TestPutReplacesTheWholeRoleSet(t *testing.T) {
	a, st := newAPI(t)
	ws, it := secItem(t, a, st)
	two := `{"value":[` + roleBody + `,{"name":"writers","decisionRules":[],"members":{}}]}`
	if w := do(a.putDataAccessRoles, admin, "PUT", two, secPV(ws, it)); w.Code != http.StatusOK {
		t.Fatalf("first PUT = %d %s", w.Code, w.Body)
	}
	if w := do(a.putDataAccessRoles, admin, "PUT", `{"value":[`+roleBody+`]}`, secPV(ws, it)); w.Code != http.StatusOK {
		t.Fatalf("second PUT = %d %s", w.Code, w.Body)
	}
	get := do(a.listDataAccessRoles, admin, "GET", "", secPV(ws, it))
	var back struct {
		Value []struct {
			Name string `json:"name"`
		}
	}
	_ = json.Unmarshal(get.Body.Bytes(), &back)
	if len(back.Value) != 1 || back.Value[0].Name != "readers" {
		t.Fatalf("the omitted role survived the replace: %s", get.Body)
	}
}

// An empty value list clears the policy — the documented way to revoke
// everything, and it must not be mistaken for "no change".
func TestPutWithNoRolesClearsThePolicy(t *testing.T) {
	a, st := newAPI(t)
	ws, it := secItem(t, a, st)
	do(a.putDataAccessRoles, admin, "PUT", `{"value":[`+roleBody+`]}`, secPV(ws, it))
	if w := do(a.putDataAccessRoles, admin, "PUT", `{"value":[]}`, secPV(ws, it)); w.Code != http.StatusOK {
		t.Fatalf("clearing PUT = %d %s", w.Code, w.Body)
	}
	get := do(a.listDataAccessRoles, admin, "GET", "", secPV(ws, it))
	var back struct{ Value []json.RawMessage }
	_ = json.Unmarshal(get.Body.Bytes(), &back)
	if len(back.Value) != 0 {
		t.Fatalf("policy survived a clearing PUT: %s", get.Body)
	}
}

// "Can edit OneLake security roles: Admin yes, Member yes, Contributor no,
// Viewer no." A Viewer that could rewrite the roles could grant itself the data.
func TestOnlyAdminsAndMembersMayWriteRoles(t *testing.T) {
	a, st := newAPI(t)
	ws, it := secItem(t, a, st)

	if w := do(a.putDataAccessRoles, viewer, "PUT", `{"value":[`+roleBody+`]}`, secPV(ws, it)); w.Code != http.StatusForbidden {
		t.Fatalf("viewer PUT = %d, want 403", w.Code)
	}
	// A viewer may still READ the policy, which is what makes the write gate a
	// real distinction rather than the item gate under another name.
	if w := do(a.listDataAccessRoles, viewer, "GET", "", secPV(ws, it)); w.Code != http.StatusOK {
		t.Fatalf("viewer GET = %d, want 200", w.Code)
	}
	// And a principal with no role on the workspace sees nothing either way.
	if w := do(a.listDataAccessRoles, nobody, "GET", "", secPV(ws, it)); w.Code == http.StatusOK {
		t.Fatalf("a non-member read the policy: %d", w.Code)
	}
}

// Two rules under one name is a payload whose author disagrees with itself.
// Storing the last would silently discard the first.
func TestDuplicateRoleNamesAreRefused(t *testing.T) {
	a, st := newAPI(t)
	ws, it := secItem(t, a, st)
	dup := `{"value":[` + roleBody + `,` + roleBody + `]}`
	w := do(a.putDataAccessRoles, admin, "PUT", dup, secPV(ws, it))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("duplicate names = %d, want 400 (%s)", w.Code, w.Body)
	}
	// And nothing was written: a rejected PUT is not a partial one.
	get := do(a.listDataAccessRoles, admin, "GET", "", secPV(ws, it))
	var back struct{ Value []json.RawMessage }
	_ = json.Unmarshal(get.Body.Bytes(), &back)
	if len(back.Value) != 0 {
		t.Fatalf("a refused PUT wrote %d roles", len(back.Value))
	}
}

func TestMalformedRolePayloadsAreRefused(t *testing.T) {
	a, st := newAPI(t)
	ws, it := secItem(t, a, st)
	for _, tc := range []struct{ name, body string }{
		{"not json", `{`},
		{"role is not an object", `{"value":["nope"]}`},
		{"role has no name", `{"value":[{"decisionRules":[]}]}`},
		{"role name is empty", `{"value":[{"name":""}]}`},
	} {
		w := do(a.putDataAccessRoles, admin, "PUT", tc.body, secPV(ws, it))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400 (%s)", tc.name, w.Code, w.Body)
		}
	}
}

func TestRolesOnAnUnknownItemAre404(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	vals := map[string]string{"wid": ws.ID, "iid": "no-such-item"}
	if w := do(a.listDataAccessRoles, admin, "GET", "", vals); w.Code != http.StatusNotFound {
		t.Errorf("GET = %d, want 404", w.Code)
	}
	if w := do(a.putDataAccessRoles, admin, "PUT", `{"value":[]}`, vals); w.Code != http.StatusNotFound {
		t.Errorf("PUT = %d, want 404", w.Code)
	}
}

// A failure to READ THE POLICY must surface as a failure, not as an empty
// policy — "this item has no roles" is a statement about the data, and a caller
// acting on it would conclude nothing is protected.
//
// Uses the repo's disk-store idiom: a second connection drops tables out from
// under the handlers, reaching branches a healthy store never does.
func TestPolicyFailuresAreNotReportedAsAbsence(t *testing.T) {
	a, st, dir := newDiskAPI(t)
	ws := seedWorkspace(t, st)
	it := &store.Item{WorkspaceID: ws.ID, DisplayName: "lake", Type: "Lakehouse"}
	if err := st.CreateItem(it, nil); err != nil {
		t.Fatal(err)
	}
	vals := secPV(ws, it)

	// The roles table is gone; the item and workspace are intact.
	dropTable(t, dir, "onelake_roles")
	if w := do(a.listDataAccessRoles, admin, "GET", "", vals); w.Code != http.StatusInternalServerError {
		t.Errorf("GET with no roles table = %d, want 500 (%s)", w.Code, w.Body)
	}
	if w := do(a.putDataAccessRoles, admin, "PUT", `{"value":[`+roleBody+`]}`, vals); w.Code != http.StatusInternalServerError {
		t.Errorf("PUT with no roles table = %d, want 500 (%s)", w.Code, w.Body)
	}

	// And an unreadable ITEM is a failure too, not a 404. The surrounding
	// surface answers 404 for any GetItem error; on a security surface that
	// invites a caller to conclude the item — and its policy — never existed.
	dropTable(t, dir, "items")
	if w := do(a.listDataAccessRoles, admin, "GET", "", vals); w.Code != http.StatusInternalServerError {
		t.Errorf("GET with no items table = %d, want 500 (%s)", w.Code, w.Body)
	}
}
