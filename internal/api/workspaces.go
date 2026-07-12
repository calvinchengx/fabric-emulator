package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// listWorkspaces returns the workspaces the caller holds any role on.
func (a *API) listWorkspaces(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	ws, err := a.Store.ListWorkspacesFor(p.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	if ws == nil {
		ws = []*store.Workspace{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"value": ws})
}

// createWorkspace creates a workspace; the caller becomes its Admin.
func (a *API) createWorkspace(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var body struct {
		DisplayName string `json:"displayName"`
		Description string `json:"description"`
		CapacityID  string `json:"capacityId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.DisplayName == "" {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "displayName is required.")
		return
	}
	ws := &store.Workspace{DisplayName: body.DisplayName, Description: body.Description, CapacityID: body.CapacityID}
	if err := a.Store.CreateWorkspace(ws, store.Principal{ID: p.ID, Type: p.Type}); err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, ws)
}

// workspaceBody is the GET wire shape: the workspace plus its identity when
// one is provisioned.
type workspaceBody struct {
	*store.Workspace
	WorkspaceIdentity *store.WorkspaceIdentity `json:"workspaceIdentity,omitempty"`
}

func (a *API) getWorkspace(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	ws, _, ok := a.requireRole(w, r.PathValue("wid"), p, store.RoleViewer)
	if !ok {
		return
	}
	body := workspaceBody{Workspace: ws}
	if wi, err := a.Store.GetWorkspaceIdentity(ws.ID); err == nil {
		body.WorkspaceIdentity = wi
	}
	writeJSON(w, http.StatusOK, body)
}

func (a *API) updateWorkspace(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	ws, _, ok := a.requireRole(w, r.PathValue("wid"), p, store.RoleAdmin)
	if !ok {
		return
	}
	var body struct {
		DisplayName *string `json:"displayName"`
		Description *string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "Malformed JSON body.")
		return
	}
	renamed := false
	if body.DisplayName != nil {
		renamed = *body.DisplayName != ws.DisplayName
		ws.DisplayName = *body.DisplayName
	}
	if body.Description != nil {
		ws.Description = *body.Description
	}
	if err := a.Store.UpdateWorkspace(ws); err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	// Name-follows-workspace: the entra identity is renamed with it.
	if renamed {
		if err := a.identityRenameFollows(ws.ID, ws.DisplayName); err != nil {
			writeErr(w, http.StatusBadGateway, "WorkspaceIdentityRenameFailed", err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, ws)
}

func (a *API) deleteWorkspace(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	if _, _, ok := a.requireRole(w, wid, p, store.RoleAdmin); !ok {
		return
	}
	// Cascade: the entra identity is deleted with its workspace.
	if err := a.identityCascadeDelete(wid); err != nil {
		writeErr(w, http.StatusBadGateway, "WorkspaceIdentityDeprovisioningFailed", err.Error())
		return
	}
	if err := a.Store.DeleteWorkspace(wid); err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}

// ---- role assignments ----

func (a *API) listRoleAssignments(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	// Members and above see the access list.
	if _, _, ok := a.requireRole(w, wid, p, store.RoleMember); !ok {
		return
	}
	ras, err := a.Store.ListRoleAssignments(wid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"value": ras})
}

// createRoleAssignment: Admins grant anything; Members grant roles at or
// below Member (the documented "add others with lower permissions").
func (a *API) createRoleAssignment(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	_, callerRole, ok := a.requireRole(w, wid, p, store.RoleMember)
	if !ok {
		return
	}
	var ra store.RoleAssignment
	if err := json.NewDecoder(r.Body).Decode(&ra); err != nil ||
		ra.Principal.ID == "" || store.RoleRank(ra.Role) < 0 {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "principal.id and a valid role are required.")
		return
	}
	if callerRole != store.RoleAdmin && store.RoleRank(ra.Role) > store.RoleRank(store.RoleMember) {
		writeErr(w, http.StatusForbidden, "InsufficientPrivileges", "Only Admins can grant the Admin role.")
		return
	}
	if ra.Principal.Type == "" {
		ra.Principal.Type = "User"
	}
	ra.WorkspaceID = wid
	ra.ID = ""
	if err := a.Store.CreateRoleAssignment(&ra); err != nil {
		writeErr(w, http.StatusConflict, "PrincipalAlreadyHasRole", "The principal already has a role on this workspace.")
		return
	}
	writeJSON(w, http.StatusCreated, ra)
}

func (a *API) updateRoleAssignment(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	if _, _, ok := a.requireRole(w, wid, p, store.RoleAdmin); !ok {
		return
	}
	var body struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || store.RoleRank(body.Role) < 0 {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "A valid role is required.")
		return
	}
	if err := a.Store.UpdateRoleAssignment(wid, r.PathValue("raid"), body.Role); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "RoleAssignmentNotFound", "No such role assignment.")
			return
		}
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	ra, err := a.Store.GetRoleAssignment(wid, r.PathValue("raid"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ra)
}

func (a *API) deleteRoleAssignment(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	if _, _, ok := a.requireRole(w, wid, p, store.RoleAdmin); !ok {
		return
	}
	if err := a.Store.DeleteRoleAssignment(wid, r.PathValue("raid")); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "RoleAssignmentNotFound", "No such role assignment.")
			return
		}
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}
