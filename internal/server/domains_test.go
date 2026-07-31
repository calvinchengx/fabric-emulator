package server_test

import (
	"net/http"
	"testing"
)

type domainResp struct {
	ID                string `json:"id"`
	DisplayName       string `json:"displayName"`
	Description       string `json:"description"`
	ParentDomainID    string `json:"parentDomainId"`
	ContributorsScope string `json:"contributorsScope"`
}

// The admin domains surface: the tree (one subdomain level), workspace
// assignment (a workspace belongs to at most one domain), and the two
// domain-scoped roles.
func TestAdminDomains(t *testing.T) {
	f := newFixture(t)

	// Create a domain; contributorsScope defaults rather than coming back empty.
	var finance domainResp
	f.mustStatus(f.call("POST", "/v1/admin/domains", f.token,
		map[string]any{"displayName": "Finance", "description": "money"}, &finance),
		http.StatusCreated, "create domain")
	if finance.ID == "" || finance.ContributorsScope == "" {
		t.Fatalf("create returned %+v", finance)
	}

	// Display names are unique tenant-wide.
	f.mustStatus(f.call("POST", "/v1/admin/domains", f.token,
		map[string]any{"displayName": "Finance"}, nil),
		http.StatusConflict, "duplicate domain name")

	// A subdomain of a domain is allowed...
	var payables domainResp
	f.mustStatus(f.call("POST", "/v1/admin/domains", f.token,
		map[string]any{"displayName": "Payables", "parentDomainId": finance.ID}, &payables),
		http.StatusCreated, "create subdomain")
	// ...but the hierarchy stops at two levels.
	f.mustStatus(f.call("POST", "/v1/admin/domains", f.token,
		map[string]any{"displayName": "Deeper", "parentDomainId": payables.ID}, nil),
		http.StatusBadRequest, "subdomain of a subdomain")

	// Patch applies only the fields sent.
	var patched domainResp
	f.mustStatus(f.call("PATCH", "/v1/admin/domains/"+finance.ID, f.token,
		map[string]any{"description": "finance data"}, &patched), http.StatusOK, "patch")
	if patched.Description != "finance data" || patched.DisplayName != "Finance" {
		t.Fatalf("patch = %+v", patched)
	}

	// Assign two workspaces, then confirm the listing.
	var ws1, ws2 struct{ ID string }
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "ws-a"}, &ws1)
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "ws-b"}, &ws2)
	f.mustStatus(f.call("POST", "/v1/admin/domains/"+finance.ID+"/assignWorkspaces", f.token,
		map[string]any{"workspacesIds": []string{ws1.ID, ws2.ID}}, nil), http.StatusOK, "assign")
	var listed struct{ Value []struct{ ID string } }
	f.mustStatus(f.call("GET", "/v1/admin/domains/"+finance.ID+"/workspaces", f.token, nil, &listed),
		http.StatusOK, "domain workspaces")
	if len(listed.Value) != 2 {
		t.Fatalf("assigned workspaces = %+v, want 2", listed.Value)
	}

	// Re-assigning to another domain moves the workspace rather than
	// duplicating it: membership is single-valued.
	f.mustStatus(f.call("POST", "/v1/admin/domains/"+payables.ID+"/assignWorkspaces", f.token,
		map[string]any{"workspacesIds": []string{ws1.ID}}, nil), http.StatusOK, "reassign")
	listed.Value = nil
	f.call("GET", "/v1/admin/domains/"+finance.ID+"/workspaces", f.token, nil, &listed)
	if len(listed.Value) != 1 || listed.Value[0].ID != ws2.ID {
		t.Fatalf("after move, Finance = %+v, want only ws-b", listed.Value)
	}

	// An unknown workspace id is rejected, not silently stored.
	f.mustStatus(f.call("POST", "/v1/admin/domains/"+finance.ID+"/assignWorkspaces", f.token,
		map[string]any{"workspacesIds": []string{"00000000-0000-4000-8000-000000000000"}}, nil),
		http.StatusNotFound, "assign unknown workspace")

	// Unassigning one workspace leaves the rest; unassigning an id that is
	// not in the domain is a no-op rather than an error.
	f.mustStatus(f.call("POST", "/v1/admin/domains/"+finance.ID+"/unassignWorkspaces", f.token,
		map[string]any{"workspacesIds": []string{ws1.ID}}, nil), http.StatusOK, "unassign non-member")
	listed.Value = nil
	f.call("GET", "/v1/admin/domains/"+finance.ID+"/workspaces", f.token, nil, &listed)
	if len(listed.Value) != 1 {
		t.Fatalf("after no-op unassign, Finance = %+v, want 1", listed.Value)
	}
	f.mustStatus(f.call("POST", "/v1/admin/domains/"+finance.ID+"/unassignWorkspaces", f.token,
		map[string]any{"workspacesIds": []string{ws2.ID}}, nil), http.StatusOK, "unassign member")
	listed.Value = nil
	f.call("GET", "/v1/admin/domains/"+finance.ID+"/workspaces", f.token, nil, &listed)
	if len(listed.Value) != 0 {
		t.Fatalf("after unassign, Finance = %+v, want empty", listed.Value)
	}

	// unassignAllWorkspaces empties a populated domain in one call.
	f.call("POST", "/v1/admin/domains/"+finance.ID+"/assignWorkspaces", f.token,
		map[string]any{"workspacesIds": []string{ws2.ID}}, nil)
	f.mustStatus(f.call("POST", "/v1/admin/domains/"+finance.ID+"/unassignAllWorkspaces", f.token,
		nil, nil), http.StatusOK, "unassignAll")
	listed.Value = nil
	f.call("GET", "/v1/admin/domains/"+finance.ID+"/workspaces", f.token, nil, &listed)
	if len(listed.Value) != 0 {
		t.Fatalf("after unassignAll, Finance = %+v, want empty", listed.Value)
	}
	// The same calls against an unknown domain 404.
	missing := "/v1/admin/domains/00000000-0000-4000-8000-000000000000"
	f.mustStatus(f.call("POST", missing+"/unassignWorkspaces", f.token,
		map[string]any{"workspacesIds": []string{ws2.ID}}, nil), http.StatusNotFound, "unassign unknown domain")
	f.mustStatus(f.call("POST", missing+"/unassignAllWorkspaces", f.token, nil, nil),
		http.StatusNotFound, "unassignAll unknown domain")
	f.mustStatus(f.call("GET", missing+"/workspaces", f.token, nil, nil),
		http.StatusNotFound, "list unknown domain workspaces")
	f.mustStatus(f.call("GET", missing+"/roleAssignments", f.token, nil, nil),
		http.StatusNotFound, "list unknown domain roles")

	// Re-assign so the nonEmptyOnly check below still has a populated domain.
	f.call("POST", "/v1/admin/domains/"+finance.ID+"/assignWorkspaces", f.token,
		map[string]any{"workspacesIds": []string{ws2.ID}}, nil)

	// nonEmptyOnly filters out domains with nothing assigned.
	var empty domainResp
	f.call("POST", "/v1/admin/domains", f.token, map[string]any{"displayName": "Unused"}, &empty)
	var all, nonEmpty struct{ Domains []domainResp }
	f.call("GET", "/v1/admin/domains", f.token, nil, &all)
	f.call("GET", "/v1/admin/domains?nonEmptyOnly=true", f.token, nil, &nonEmpty)
	if len(nonEmpty.Domains) >= len(all.Domains) {
		t.Fatalf("nonEmptyOnly did not filter: %d of %d", len(nonEmpty.Domains), len(all.Domains))
	}
	for _, d := range nonEmpty.Domains {
		if d.ID == empty.ID {
			t.Fatalf("nonEmptyOnly returned the empty domain")
		}
	}

	// Role assignment: bulk assign, list, bulk unassign.
	principals := []map[string]string{{"id": "11111111-1111-4111-8111-111111111111", "type": "User"}}
	f.mustStatus(f.call("POST", "/v1/admin/domains/"+finance.ID+"/roleAssignments/bulkAssign", f.token,
		map[string]any{"type": "Admins", "principals": principals}, nil), http.StatusOK, "bulkAssign")
	var roles struct {
		Value []struct {
			Role      string
			Principal struct{ ID string }
		}
	}
	f.mustStatus(f.call("GET", "/v1/admin/domains/"+finance.ID+"/roleAssignments", f.token, nil, &roles),
		http.StatusOK, "list roles")
	if len(roles.Value) != 1 || roles.Value[0].Role != "Admins" {
		t.Fatalf("roles = %+v", roles.Value)
	}
	// An unknown role name is refused.
	f.mustStatus(f.call("POST", "/v1/admin/domains/"+finance.ID+"/roleAssignments/bulkAssign", f.token,
		map[string]any{"type": "Owners", "principals": principals}, nil),
		http.StatusBadRequest, "bad role name")
	f.mustStatus(f.call("POST", "/v1/admin/domains/"+finance.ID+"/roleAssignments/bulkUnassign", f.token,
		map[string]any{"type": "Admins", "principals": principals}, nil), http.StatusOK, "bulkUnassign")
	roles.Value = nil
	f.call("GET", "/v1/admin/domains/"+finance.ID+"/roleAssignments", f.token, nil, &roles)
	if len(roles.Value) != 0 {
		t.Fatalf("roles after unassign = %+v", roles.Value)
	}

	// Deleting a parent takes its subdomain with it.
	f.mustStatus(f.call("DELETE", "/v1/admin/domains/"+finance.ID, f.token, nil, nil),
		http.StatusOK, "delete domain")
	f.mustStatus(f.call("GET", "/v1/admin/domains/"+finance.ID, f.token, nil, nil),
		http.StatusNotFound, "deleted domain")
	f.mustStatus(f.call("GET", "/v1/admin/domains/"+payables.ID, f.token, nil, nil),
		http.StatusNotFound, "subdomain cascaded")
	// Unknown ids 404 rather than 500.
	f.mustStatus(f.call("DELETE", "/v1/admin/domains/"+finance.ID, f.token, nil, nil),
		http.StatusNotFound, "delete twice")
}
