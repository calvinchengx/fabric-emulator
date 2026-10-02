package onelake

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// "OneLake security doesn't support the combination of two or more roles where
// one contains RLS rules and another contains CLS rules. Users that try to
// access tables that are part of an unsupported role combination receive query
// errors." Unioned, the two roles granted everything: the column role's rows
// were whole and the row role's columns were whole.

func roleBody(t *testing.T, name, path string, constraints map[string]any, members ...string) []byte {
	t.Helper()
	rule := map[string]any{"effect": "Permit", "permission": []map[string]any{
		{"attributeName": "Path", "attributeValueIncludedIn": []string{path}},
		{"attributeName": "Action", "attributeValueIncludedIn": []string{"Read"}},
	}}
	if constraints != nil {
		rule["constraints"] = constraints
	}
	var ms []map[string]string
	for _, m := range members {
		ms = append(ms, map[string]string{"objectId": m})
	}
	b, err := json.Marshal(map[string]any{"name": name, "decisionRules": []map[string]any{rule},
		"members": map[string]any{"microsoftEntraMembers": ms}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mixedRoles(t *testing.T, f *fixture) {
	t.Helper()
	const table = "Tables/dbo/Customers"
	if err := f.st.PutOneLakeRoles(f.it.ID, []store.OneLakeRole{
		{ItemID: f.it.ID, Name: "rows", Body: roleBody(t, "rows", table,
			narrowingConstraints(table, "SELECT * FROM Customers WHERE region = 1", nil), "both-1", "rows-1")},
		{ItemID: f.it.ID, Name: "cols", Body: roleBody(t, "cols", table,
			narrowingConstraints(table, "", []string{"name"}), "both-1")},
	}); err != nil {
		t.Fatal(err)
	}
}

// A direct read by a principal in both roles is refused, naming the
// combination — it used to be served whole.
func TestMixedRowAndColumnRolesBlockADirectRead(t *testing.T) {
	f := newFixture(t)
	for _, p := range []string{"both-1", "rows-1"} {
		grantRole(t, f, p, store.RoleViewer)
	}
	seedFile(t, f, "Tables/dbo/Customers/part-0.parquet")
	mixedRoles(t, f)
	url := "/" + f.ws.ID + "/" + f.it.ID + "/Tables/dbo/Customers/part-0.parquet"
	if w := f.do("GET", url, f.storageToken("both-1"), nil); w.Code != http.StatusForbidden ||
		!strings.Contains(w.Body.String(), "a combination OneLake security does not support") {
		t.Fatalf("in both roles = %d %s, want the combination refused", w.Code, w.Body)
	}
	// One role alone is the supported row-level case, refused for its own reason.
	if w := f.do("GET", url, f.storageToken("rows-1"), nil); w.Code != http.StatusForbidden ||
		strings.Contains(w.Body.String(), "combination") || !strings.Contains(w.Body.String(), "row-level security") {
		t.Fatalf("in the row role only = %d %s, want the row-level refusal", w.Code, w.Body)
	}
}

// principalAccess has no field for a query error, so an engine is given BOTH
// restrictions — never more than either role grants on its own.
func TestPrincipalAccessGivesAnEngineBothRestrictions(t *testing.T) {
	f := newFixture(t)
	grantRole(t, f, "engine-1", store.RoleMember)
	grantRole(t, f, "both-1", store.RoleViewer)
	mixedRoles(t, f)
	out, code := askAccess(t, f, "engine-1", "both-1", "Tables")
	if code != http.StatusOK {
		t.Fatalf("principalAccess = %d", code)
	}
	for _, e := range out.Value {
		if e.Path != "Tables/dbo/Customers" {
			continue
		}
		if e.Rows == "" || len(e.Columns) != 1 || e.Columns[0] != "name" {
			t.Fatalf("Customers = rows %q columns %v, want the filter AND the column list", e.Rows, e.Columns)
		}
		return
	}
	t.Fatalf("no Customers entry in %+v", out.Value)
}
