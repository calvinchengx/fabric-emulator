package api

// Capacities (docs/02, ## Capacities): an assignable object only — no
// SKU/billing/throttling model. They exist because real tooling checks them:
// fabric-cicd refuses to publish into a workspace with no capacityId.

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// listCapacities returns the capacities the caller can see (all of them —
// capacity RBAC is tenant-level and out of scope).
func (a *API) listCapacities(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	cs, err := a.Store.ListCapacities()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"value": cs})
}

// assignToCapacity attaches the workspace to a capacity (202 LRO, no result).
func (a *API) assignToCapacity(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	ws, _, ok := a.requireRole(w, r.PathValue("wid"), p, store.RoleAdmin)
	if !ok {
		return
	}
	var body struct {
		CapacityID string `json:"capacityId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.CapacityID == "" {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "capacityId is required.")
		return
	}
	if _, err := a.Store.GetCapacity(body.CapacityID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "CapacityNotFound", "No capacity matches capacityId.")
			return
		}
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	ws.CapacityID = body.CapacityID
	if err := a.Store.UpdateWorkspace(ws); err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	a.startOperation(w, r, "AssignToCapacity", "")
}

// unassignFromCapacity detaches the workspace (202 LRO, no result).
func (a *API) unassignFromCapacity(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	ws, _, ok := a.requireRole(w, r.PathValue("wid"), p, store.RoleAdmin)
	if !ok {
		return
	}
	ws.CapacityID = ""
	if err := a.Store.UpdateWorkspace(ws); err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	a.startOperation(w, r, "UnassignFromCapacity", "")
}
