package api

// Tenant-wide workspace administration:
//
//	GET /v1/admin/workspaces?type=&capacityId=&name=&state=&continuationToken=
//
// Shaped exactly as the REST reference documents it
// (admin/workspaces/list-workspaces), which differs from the *user-facing*
// workspace API in two ways worth knowing:
//
//   - the envelope key is `workspaces`, not `value`;
//   - the name field is `name`, not `displayName`.
//
// Both would have been wrong if derived from the /v1/workspaces surface.
//
// Tenant-admin gate: as with the other admin routes, the emulator has no
// Fabric-administrator role model, so any authenticated principal may call
// this. Real Fabric requires Tenant.Read.All. Documented, not pretended.

import (
	"net/http"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
)

// Documented WorkspaceType values.
var adminWorkspaceTypes = map[string]string{
	"personal": "Personal", "workspace": "Workspace", "adminworkspace": "AdminWorkspace",
}

// Documented WorkspaceState values. The emulator has no soft delete, so every
// workspace it holds is Active — a `state=Deleted` filter correctly returns
// nothing rather than erroring.
var adminWorkspaceStates = map[string]string{"active": "Active", "deleted": "Deleted"}

func (a *API) registerAdminWorkspaces(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/admin/workspaces", a.withAuth(a.adminListWorkspaces))
}

// adminWorkspace is the documented Workspace object. Fields the emulator does
// not model (encryption, tags) are omitted rather than sent empty.
type adminWorkspace struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	State      string `json:"state"`
	CapacityID string `json:"capacityId,omitempty"`
	DomainID   string `json:"domainId,omitempty"`
}

func (a *API) adminListWorkspaces(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	q := r.URL.Query()

	// The reference documents BadRequest for an invalid type/state rather
	// than silently returning everything.
	wantType := ""
	if raw := q.Get("type"); raw != "" {
		canonical, ok := adminWorkspaceTypes[strings.ToLower(raw)]
		if !ok {
			writeErr(w, http.StatusBadRequest, "BadRequest",
				"type must be one of personal, workspace, adminworkspace.")
			return
		}
		wantType = canonical
	}
	wantState := ""
	if raw := q.Get("state"); raw != "" {
		canonical, ok := adminWorkspaceStates[strings.ToLower(raw)]
		if !ok {
			writeErr(w, http.StatusBadRequest, "BadRequest",
				"state must be one of active, deleted.")
			return
		}
		wantState = canonical
	}

	all, err := a.Store.ListAllWorkspaces()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	domains, err := a.Store.WorkspaceDomains()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}

	wantCapacity, wantName := q.Get("capacityId"), q.Get("name")
	out := []adminWorkspace{}
	for _, ws := range all {
		// Every emulator workspace is a live, non-personal Workspace.
		const gotType, gotState = "Workspace", "Active"
		switch {
		case wantType != "" && wantType != gotType,
			wantState != "" && wantState != gotState,
			wantCapacity != "" && wantCapacity != ws.CapacityID,
			wantName != "" && !strings.EqualFold(wantName, ws.DisplayName):
			continue
		}
		out = append(out, adminWorkspace{
			ID: ws.ID, Name: ws.DisplayName, Type: gotType, State: gotState,
			CapacityID: ws.CapacityID, DomainID: domains[ws.ID],
		})
	}
	writePageKeyed(w, r, "workspaces", out)
}
