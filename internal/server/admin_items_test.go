package server_test

import (
	"net/http"
	"testing"
	"time"
)

type adminItemsPage struct {
	// Third envelope key on this API surface: `value` (user-facing),
	// `workspaces` (admin workspaces), `itemEntities` (admin items).
	ItemEntities []struct {
		ID              string `json:"id"`
		Type            string `json:"type"`
		Name            string `json:"name"`
		Description     string `json:"description"`
		State           string `json:"state"`
		LastUpdatedDate string `json:"lastUpdatedDate"`
		WorkspaceID     string `json:"workspaceId"`
		CapacityID      string `json:"capacityId"`
	} `json:"itemEntities"`
}

func (f *fixture) adminItems(t *testing.T, query string) adminItemsPage {
	t.Helper()
	url := "/v1/admin/items"
	if query != "" {
		url += "?" + query
	}
	var page adminItemsPage
	f.mustStatus(f.call("GET", url, f.token, nil, &page), http.StatusOK, "admin items")
	return page
}

// The tenant-wide listing spans workspaces and uses the documented shape.
func TestAdminListItemsSpansWorkspaces(t *testing.T) {
	f := newFixture(t)
	var wsA, wsB struct{ ID string }
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "items-a"}, &wsA)
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "items-b"}, &wsB)
	var nb, lh struct{ ID string }
	f.call("POST", "/v1/workspaces/"+wsA.ID+"/notebooks", f.token,
		map[string]any{"displayName": "nb", "description": "a notebook"}, &nb)
	f.call("POST", "/v1/workspaces/"+wsB.ID+"/lakehouses", f.token, map[string]any{"displayName": "lh"}, &lh)

	page := f.adminItems(t, "")
	seen := map[string]bool{}
	for _, it := range page.ItemEntities {
		seen[it.ID] = true
		if it.State != "Active" {
			t.Fatalf("item %s state = %q, want Active (the only documented state)", it.ID, it.State)
		}
		if it.WorkspaceID == "" || it.CapacityID == "" {
			t.Fatalf("item %s missing workspaceId/capacityId: %+v", it.ID, it)
		}
		// lastUpdatedDate is the documented format, without a zone suffix.
		if _, err := time.Parse("2006-01-02T15:04:05", it.LastUpdatedDate); err != nil {
			t.Fatalf("lastUpdatedDate %q not in the documented format: %v", it.LastUpdatedDate, err)
		}
	}
	if !seen[nb.ID] || !seen[lh.ID] {
		t.Fatalf("listing did not span both workspaces (%d items)", len(page.ItemEntities))
	}
	// The item name comes back under `name`, and description round-trips.
	for _, it := range page.ItemEntities {
		if it.ID == nb.ID {
			if it.Name != "nb" || it.Type != "Notebook" || it.Description != "a notebook" {
				t.Fatalf("notebook = %+v", it)
			}
		}
	}
}

// The documented filters, and the documented error codes for bad values.
func TestAdminListItemsFiltersAndValidation(t *testing.T) {
	f := newFixture(t)
	var wsA, wsB struct{ ID string }
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "flt-a"}, &wsA)
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "flt-b"}, &wsB)
	f.call("POST", "/v1/workspaces/"+wsA.ID+"/notebooks", f.token, map[string]any{"displayName": "n1"}, nil)
	f.call("POST", "/v1/workspaces/"+wsA.ID+"/lakehouses", f.token, map[string]any{"displayName": "l1"}, nil)
	f.call("POST", "/v1/workspaces/"+wsB.ID+"/notebooks", f.token, map[string]any{"displayName": "n2"}, nil)

	// workspaceId narrows to one workspace.
	byWS := f.adminItems(t, "workspaceId="+wsB.ID)
	if len(byWS.ItemEntities) != 1 || byWS.ItemEntities[0].Name != "n2" {
		t.Fatalf("workspaceId filter = %+v", byWS.ItemEntities)
	}
	// type narrows across workspaces, and is case-insensitive.
	if got := f.adminItems(t, "type=Notebook"); len(got.ItemEntities) != 2 {
		t.Fatalf("type=Notebook returned %d, want 2", len(got.ItemEntities))
	}
	if got := f.adminItems(t, "type=notebook"); len(got.ItemEntities) != 2 {
		t.Fatal("type filter should be case-insensitive")
	}
	// capacityId filters; an unknown one is empty rather than an error.
	all := f.adminItems(t, "")
	if got := f.adminItems(t, "capacityId="+all.ItemEntities[0].CapacityID); len(got.ItemEntities) == 0 {
		t.Fatal("capacityId filter returned nothing for a real capacity")
	}
	if got := f.adminItems(t, "capacityId=00000000-0000-4000-8000-000000000000"); len(got.ItemEntities) != 0 {
		t.Fatalf("unknown capacityId returned %d items", len(got.ItemEntities))
	}
	// state=Active is accepted; anything else is InvalidItemState.
	if got := f.adminItems(t, "state=Active"); len(got.ItemEntities) != len(all.ItemEntities) {
		t.Fatal("state=Active should return everything")
	}

	for _, tc := range []struct{ query, code string }{
		{"type=NotAType", "InvalidItemType"},
		{"state=Deleted", "InvalidItemState"},
	} {
		var errBody struct {
			ErrorCode string `json:"errorCode"`
		}
		resp := f.call("GET", "/v1/admin/items?"+tc.query, f.token, nil, &errBody)
		f.mustStatus(resp, http.StatusBadRequest, tc.query)
		if errBody.ErrorCode != tc.code {
			t.Fatalf("%s errorCode = %q, want %q", tc.query, errBody.ErrorCode, tc.code)
		}
	}
}
