package semanticmodel

import (
	"reflect"
	"testing"
)

// Roles are parsed, never skipped. Both parsers used to drop them without a
// word, so a secured model evaluated as though it had no security — the same
// silence-as-success failure the OneLake constraints bug had. Every sample here
// is Microsoft's own, from the TMSL Roles reference, the Analysis Services OLS
// page and the TMDL overview.

const tmslTables = `"tables":[{"name":"Store","columns":[{"name":"Store Code","dataType":"int64"}]},
  {"name":"Product","columns":[{"name":"Name","dataType":"string"}]},
  {"name":"Employee","columns":[{"name":"Base Rate","dataType":"double"}]}]`

func TestTMSLRolesAreParsed(t *testing.T) {
	m, err := ParseTMSL([]byte(`{"name":"m","compatibilityLevel":1567,"model":{` + tmslTables + `,
	  "roles": [
	    {"name": "Users", "description": "All allowed users to query the model", "modelPermission": "read",
	     "members": [{"memberName": "ada@contoso.com", "memberId": "11111111-0000-0000-0000-000000000001",
	                  "identityProvider": "AzureAD", "memberType": "user"}],
	     "tablePermissions": [
	       {"name": "Store", "filterExpression": "'Store'[Store Code] IN {1,10,20,30}"},
	       {"name": "Product", "metadataPermission": "none"},
	       {"name": "Employee", "columnPermissions": [{"name": "Base Rate", "metadataPermission": "none"}]}
	     ]},
	    {"name": "Multiline", "modelPermission": "read",
	     "tablePermissions": [{"name": "Store", "filterExpression": ["'Store'[Store Code] = 1", "|| 'Store'[Store Code] = 2"]}]}
	  ]}}`))
	if err != nil {
		t.Fatal(err)
	}
	want := []Role{
		{Name: "Users", ModelPermission: "read",
			Members: []RoleMember{{Name: "ada@contoso.com", ID: "11111111-0000-0000-0000-000000000001",
				IdentityProvider: "AzureAD", Type: "user"}},
			TablePermissions: []TablePermission{
				{Table: "Store", FilterExpression: "'Store'[Store Code] IN {1,10,20,30}"},
				{Table: "Product", MetadataPermission: "none"},
				{Table: "Employee", ColumnPermissions: []ColumnPermission{{Column: "Base Rate", MetadataPermission: "none"}}},
			}},
		{Name: "Multiline", ModelPermission: "read",
			TablePermissions: []TablePermission{{Table: "Store",
				FilterExpression: "'Store'[Store Code] = 1\n|| 'Store'[Store Code] = 2"}}},
	}
	if !reflect.DeepEqual(m.Roles, want) {
		t.Fatalf("roles =\n%#v\nwant\n%#v", m.Roles, want)
	}
}

func TestATMSLFilterExpressionThatIsNeitherStringNorLinesIsRefused(t *testing.T) {
	_, err := ParseTMSL([]byte(`{"name":"m","model":{` + tmslTables + `,
	  "roles":[{"name":"r","tablePermissions":[{"name":"Store","filterExpression":{"not":"dax"}}]}]}}`))
	if err == nil {
		t.Fatal("a filterExpression that is not DAX text was accepted")
	}
}

func TestTMDLRolesAreParsed(t *testing.T) {
	m, err := ParseTMDL(map[string][]byte{
		"definition/tables/Store.tmdl": []byte("table Store\n\tcolumn 'Store Code'\n\t\tdataType: int64\n"),
		// The TMDL overview's own role, verbatim.
		"definition/roles/Role_Store1.tmdl": []byte("role Role_Store1\n\tmodelPermission: read\n\n\ttablePermission Store = 'Store'[Store Code] IN {1,10,20,30}\n"),
		// The OLS page's two forms: a secured table, and a secured column as a
		// child property; plus a column's default-property form, a member, and
		// a filter continuing onto the next lines.
		"definition/roles/CategoriesOLS.tmdl": []byte(`role CategoriesOLS
	modelPermission: read
	member 'ada@contoso.com' = user
		memberId: 11111111-0000-0000-0000-000000000001
		identityProvider: AzureAD
	member 'Sales Team'
		memberType: group

	tablePermission Customers
		metadataPermission: none

	tablePermission Employee
		columnPermission Address
			metadataPermission: none
		columnPermission 'Base Rate' = none

	tablePermission Store =
			'Store'[Store Code] = 1
			|| 'Store'[Store Code] = 2
`),
	})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Role{}
	for _, r := range m.Roles {
		byName[r.Name] = r
	}
	if got := byName["Role_Store1"]; !reflect.DeepEqual(got, Role{Name: "Role_Store1", ModelPermission: "read",
		TablePermissions: []TablePermission{{Table: "Store", FilterExpression: "'Store'[Store Code] IN {1,10,20,30}"}}}) {
		t.Errorf("Role_Store1 = %#v", got)
	}
	want := Role{Name: "CategoriesOLS", ModelPermission: "read",
		Members: []RoleMember{
			{Name: "ada@contoso.com", ID: "11111111-0000-0000-0000-000000000001", IdentityProvider: "AzureAD", Type: "user"},
			{Name: "Sales Team", Type: "group"},
		},
		TablePermissions: []TablePermission{
			{Table: "Customers", MetadataPermission: "none"},
			{Table: "Employee", ColumnPermissions: []ColumnPermission{
				{Column: "Address", MetadataPermission: "none"},
				{Column: "Base Rate", MetadataPermission: "none"},
			}},
			{Table: "Store", FilterExpression: "'Store'[Store Code] = 1\n|| 'Store'[Store Code] = 2"},
		}}
	if got := byName["CategoriesOLS"]; !reflect.DeepEqual(got, want) {
		t.Errorf("CategoriesOLS =\n%#v\nwant\n%#v", got, want)
	}
}

// A model with no roles parses as one with none — not nil-versus-empty
// ambiguity a caller could trip on.
func TestAModelWithoutRolesHasNone(t *testing.T) {
	m, err := ParseTMSL([]byte(`{"name":"m","model":{` + tmslTables + `}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Roles) != 0 {
		t.Fatalf("roles = %#v", m.Roles)
	}
}

// A filter written over several lines may indent its own continuation further,
// as DAX authors do for nested conditions; every line belongs to the filter.
func TestATMDLFilterKeepsDeeperIndentedContinuation(t *testing.T) {
	m, err := ParseTMDL(map[string][]byte{
		"definition/tables/Store.tmdl": []byte("table Store\n\tcolumn Territory\n\t\tdataType: string\n"),
		"definition/roles/R.tmdl":      []byte("role R\n\ttablePermission Store =\n\t\t\t'Store'[Territory] = \"NC\"\n\t\t\t\t|| 'Store'[Territory] = \"SC\"\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := m.Roles[0].TablePermissions[0].FilterExpression,
		"'Store'[Territory] = \"NC\"\n|| 'Store'[Territory] = \"SC\""; got != want {
		t.Fatalf("filter = %q, want %q", got, want)
	}
}
