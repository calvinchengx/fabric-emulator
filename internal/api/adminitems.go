package api

// Tenant-wide item administration:
//
//	GET /v1/admin/items?workspaceId=&capacityId=&state=&type=&continuationToken=
//
// Per the REST reference (admin/items/list-items). Note the envelope key is
// `itemEntities` — a third spelling, after the user-facing `value` and the
// admin workspace surface's `workspaces`. These are not derivable from each
// other, so each comes from its own reference page.
//
// The reference documents `Active` as the only ItemState, and dedicated error
// codes for a bad type (`InvalidItemType`) or state (`InvalidItemState`).
//
// Tenant-admin gate: as elsewhere in /v1/admin, the emulator has no
// Fabric-administrator role model.

import (
	"net/http"
	"strings"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

func (a *API) registerAdminItems(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/admin/items", a.withTenantRead(a.adminListItems))
}

// adminItem is the documented Item object. Fields the emulator does not model
// (creatorPrincipal, defaultIdentity, tags) are omitted rather than faked.
type adminItem struct {
	ID              string `json:"id"`
	Type            string `json:"type"`
	Name            string `json:"name"`
	Description     string `json:"description,omitempty"`
	State           string `json:"state"`
	LastUpdatedDate string `json:"lastUpdatedDate"`
	WorkspaceID     string `json:"workspaceId"`
	CapacityID      string `json:"capacityId,omitempty"`
	FolderID        string `json:"folderId,omitempty"`
}

func (a *API) adminListItems(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	q := r.URL.Query()

	// `Active` is the only documented ItemState.
	if raw := q.Get("state"); raw != "" && !strings.EqualFold(raw, "Active") {
		writeErr(w, http.StatusBadRequest, "InvalidItemState",
			"Item state isn't valid. The only supported state is Active.")
		return
	}
	wantType := ""
	if raw := q.Get("type"); raw != "" {
		canonical, ok := store.CanonicalItemType(raw)
		if !ok {
			writeErr(w, http.StatusBadRequest, "InvalidItemType", "Item type isn't valid.")
			return
		}
		wantType = canonical
	}

	items, err := a.Store.AllItems()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	// The item carries its workspace's capacity, so resolve those once.
	workspaces, err := a.Store.ListAllWorkspaces()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	capacityOf := make(map[string]string, len(workspaces))
	for _, ws := range workspaces {
		capacityOf[ws.ID] = ws.CapacityID
	}

	wantWorkspace, wantCapacity := q.Get("workspaceId"), q.Get("capacityId")
	out := []adminItem{}
	for _, it := range items {
		capacity := capacityOf[it.WorkspaceID]
		switch {
		case wantType != "" && wantType != it.Type,
			wantWorkspace != "" && wantWorkspace != it.WorkspaceID,
			wantCapacity != "" && wantCapacity != capacity:
			continue
		}
		out = append(out, adminItem{
			ID: it.ID, Type: it.Type, Name: it.DisplayName, Description: it.Description,
			State: "Active",
			// The emulator records creation time, not a separate modified
			// time, so that is what is reported here.
			LastUpdatedDate: time.Unix(it.CreatedAt, 0).UTC().Format("2006-01-02T15:04:05"),
			WorkspaceID:     it.WorkspaceID, CapacityID: capacity, FolderID: it.FolderID,
		})
	}
	writePageKeyed(a, w, r, "itemEntities", out)
}
