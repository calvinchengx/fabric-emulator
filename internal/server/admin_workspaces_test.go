package server_test

import (
	"net/http"
	"testing"
)

type adminWorkspacesPage struct {
	// The envelope key is `workspaces`, not `value` — the admin list APIs
	// name their array after the resource. Asserting the key is the point.
	Workspaces []struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		Type       string `json:"type"`
		State      string `json:"state"`
		CapacityID string `json:"capacityId"`
		DomainID   string `json:"domainId"`
	} `json:"workspaces"`
	ContinuationToken *string `json:"continuationToken"`
}

func (f *fixture) adminWorkspaces(t *testing.T, query string) adminWorkspacesPage {
	t.Helper()
	url := "/v1/admin/workspaces"
	if query != "" {
		url += "?" + query
	}
	var page adminWorkspacesPage
	f.mustStatus(f.call("GET", url, f.token, nil, &page), http.StatusOK, "admin workspaces")
	return page
}

// The tenant-wide listing uses the documented Workspace shape — which differs
// from the user-facing /v1/workspaces surface in both its envelope key and
// its name field.
func TestAdminListWorkspacesUsesDocumentedShape(t *testing.T) {
	f := newFixture(t)
	var ws struct{ ID string }
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "admin-listed"}, &ws)

	page := f.adminWorkspaces(t, "")
	var found bool
	for _, got := range page.Workspaces {
		if got.ID != ws.ID {
			continue
		}
		found = true
		if got.Name != "admin-listed" {
			t.Fatalf("name = %q; the admin surface uses `name`, not `displayName`", got.Name)
		}
		if got.Type != "Workspace" || got.State != "Active" {
			t.Fatalf("type/state = %q/%q, want Workspace/Active", got.Type, got.State)
		}
		if got.CapacityID == "" {
			t.Fatal("capacityId missing — workspaces are auto-assigned a capacity")
		}
	}
	if !found {
		t.Fatalf("workspace %s not in the admin listing (%d returned)", ws.ID, len(page.Workspaces))
	}
}

// A workspace assigned to a domain reports its domainId; an unassigned one
// omits the field rather than sending an empty string.
func TestAdminListWorkspacesReportsDomain(t *testing.T) {
	f := newFixture(t)
	var assigned, loose struct{ ID string }
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "in-domain"}, &assigned)
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "no-domain"}, &loose)

	var dom domainResp
	f.call("POST", "/v1/admin/domains", f.token, map[string]any{"displayName": "AdminListDomain"}, &dom)
	f.mustStatus(f.call("POST", "/v1/admin/domains/"+dom.ID+"/assignWorkspaces", f.token,
		map[string]any{"workspacesIds": []string{assigned.ID}}, nil), http.StatusOK, "assign")

	for _, got := range f.adminWorkspaces(t, "").Workspaces {
		switch got.ID {
		case assigned.ID:
			if got.DomainID != dom.ID {
				t.Fatalf("assigned workspace domainId = %q, want %q", got.DomainID, dom.ID)
			}
		case loose.ID:
			if got.DomainID != "" {
				t.Fatalf("unassigned workspace reported domainId %q", got.DomainID)
			}
		}
	}
}

// The documented filters, including the ones whose answer is "nothing".
func TestAdminListWorkspacesFilters(t *testing.T) {
	f := newFixture(t)
	var a, b struct{ ID string }
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "filter-alpha"}, &a)
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "filter-beta"}, &b)

	// name is an exact (case-insensitive) match, not a substring search.
	byName := f.adminWorkspaces(t, "name=FILTER-ALPHA")
	if len(byName.Workspaces) != 1 || byName.Workspaces[0].ID != a.ID {
		t.Fatalf("name filter = %+v, want just filter-alpha", byName.Workspaces)
	}
	if len(f.adminWorkspaces(t, "name=filter").Workspaces) != 0 {
		t.Fatal("name filter matched a prefix; it should be an exact match")
	}

	// capacityId filters; an unknown one returns an empty list, not an error.
	all := f.adminWorkspaces(t, "")
	if got := f.adminWorkspaces(t, "capacityId="+all.Workspaces[0].CapacityID); len(got.Workspaces) == 0 {
		t.Fatal("capacityId filter returned nothing for a real capacity")
	}
	if got := f.adminWorkspaces(t, "capacityId=00000000-0000-4000-8000-000000000000"); len(got.Workspaces) != 0 {
		t.Fatalf("unknown capacityId returned %d workspaces", len(got.Workspaces))
	}

	// The emulator has no soft delete, so state=Deleted is legitimately empty
	// while state=Active returns everything.
	if got := f.adminWorkspaces(t, "state=Deleted"); len(got.Workspaces) != 0 {
		t.Fatalf("state=Deleted returned %d workspaces", len(got.Workspaces))
	}
	if got := f.adminWorkspaces(t, "state=Active"); len(got.Workspaces) != len(all.Workspaces) {
		t.Fatalf("state=Active = %d, want %d", len(got.Workspaces), len(all.Workspaces))
	}
	// type filtering: every emulator workspace is a Workspace.
	if got := f.adminWorkspaces(t, "type=Personal"); len(got.Workspaces) != 0 {
		t.Fatalf("type=Personal returned %d workspaces", len(got.Workspaces))
	}
	if got := f.adminWorkspaces(t, "type=workspace"); len(got.Workspaces) != len(all.Workspaces) {
		t.Fatal("type filter should be case-insensitive")
	}

	// Undocumented enum values are a BadRequest, as the reference specifies.
	for _, bad := range []string{"type=Nonsense", "state=Archived"} {
		var errBody struct {
			ErrorCode string `json:"errorCode"`
		}
		resp := f.call("GET", "/v1/admin/workspaces?"+bad, f.token, nil, &errBody)
		f.mustStatus(resp, http.StatusBadRequest, bad)
		if errBody.ErrorCode != "BadRequest" {
			t.Fatalf("%s errorCode = %q, want BadRequest", bad, errBody.ErrorCode)
		}
	}
}
