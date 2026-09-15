package semanticmodel

import "testing"

func TestRoleMembership(t *testing.T) {
	r := Role{Name: "R", Members: []RoleMember{
		{Name: "ada@contoso.com", ID: "aaaa", IdentityProvider: "AzureAD", Type: "user"},
		{Name: "grace@contoso.com"},
		{Name: "Sales Team", ID: "group-oid", Type: "group"},
		{Name: "CONTOSO\\bob", ID: "bbbb", IdentityProvider: "Windows"},
		{Name: "", ID: ""},
	}}
	for name, tc := range map[string]struct {
		id, upn string
		want    bool
	}{
		"by object id":                    {"aaaa", "", true},
		"by object id, any case":          {"AAAA", "", true},
		"by UPN":                          {"someone-else", "grace@contoso.com", true},
		"by UPN, any case":                {"x", "GRACE@contoso.com", true},
		"a stranger":                      {"zzzz", "zed@contoso.com", false},
		"a group's object id":             {"group-oid", "", false},
		"a group's name as a UPN":         {"x", "Sales Team", false},
		"another provider's member":       {"bbbb", "", false},
		"an empty identity":               {"", "", false},
		"a UPN-less caller by empty name": {"q", "", false},
	} {
		if got := r.Admits(tc.id, tc.upn); got != tc.want {
			t.Errorf("%s: Admits = %v, want %v", name, got, tc.want)
		}
	}
}

func TestRolesForUnionsEveryAdmittingRole(t *testing.T) {
	m := &Model{Roles: []Role{
		{Name: "West", Members: []RoleMember{{ID: "aaaa"}}},
		{Name: "East", Members: []RoleMember{{Name: "ada@contoso.com"}}},
		{Name: "Nobody", Members: []RoleMember{{ID: "zzzz"}}},
	}}
	got := m.RolesFor("aaaa", "ada@contoso.com")
	if len(got) != 2 || got[0].Name != "West" || got[1].Name != "East" {
		t.Fatalf("roles = %v, want West and East", got)
	}
	if none := m.RolesFor("stranger", ""); len(none) != 0 {
		t.Fatalf("a stranger was admitted to %v", none)
	}
}
