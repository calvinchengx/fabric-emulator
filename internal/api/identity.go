package api

// Workspace identity: the deepest entra-emulator integration. Provisioning a
// workspace identity asks entra (over HTTP, via its admin API) to create the
// auto-managed app registration + service principal; the identity's SP is
// then granted Admin on its workspace so tokens entra mints for it
// (GET {entra}/fabric/workspaceidentities/{id}/token) can call back into
// this control plane — the customer never holds a credential, exactly as
// documented.

import (
	"errors"
	"net/http"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// provisionIdentity creates the workspace identity (202 LRO, no result; the
// identity appears on the workspace wire shape).
func (a *API) provisionIdentity(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	ws, _, ok := a.requireRole(w, wid, p, store.RoleAdmin)
	if !ok {
		return
	}
	if a.Entra == nil {
		writeErr(w, http.StatusServiceUnavailable, "IdentityProviderNotConfigured",
			"No Entra endpoint is configured for identity provisioning.")
		return
	}
	if _, err := a.Store.GetWorkspaceIdentity(wid); err == nil {
		writeErr(w, http.StatusConflict, "WorkspaceIdentityAlreadyExists",
			"The workspace already has an identity.")
		return
	}
	id, err := a.Entra.CreateWorkspaceIdentity(ws.ID, ws.DisplayName)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "WorkspaceIdentityProvisioningFailed", err.Error())
		return
	}
	if err := a.Store.SetWorkspaceIdentity(&store.WorkspaceIdentity{
		WorkspaceID: wid, IdentityID: id.ID, AppID: id.AppID,
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	// The identity acts as the workspace: grant its SP Admin so its tokens
	// (sub/appid = AppID) pass RBAC here.
	err = a.Store.CreateRoleAssignment(&store.RoleAssignment{
		WorkspaceID: wid,
		Principal:   store.Principal{ID: id.AppID, Type: "ServicePrincipal"},
		Role:        store.RoleAdmin,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	a.startOperation(w, r, "ProvisionWorkspaceIdentity", "")
}

// deprovisionIdentity deletes the identity in entra and revokes its access.
func (a *API) deprovisionIdentity(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	if _, _, ok := a.requireRole(w, wid, p, store.RoleAdmin); !ok {
		return
	}
	wi, err := a.Store.GetWorkspaceIdentity(wid)
	if err != nil {
		writeErr(w, http.StatusNotFound, "WorkspaceIdentityNotFound", "The workspace has no identity.")
		return
	}
	if a.Entra == nil {
		writeErr(w, http.StatusServiceUnavailable, "IdentityProviderNotConfigured",
			"No Entra endpoint is configured for identity provisioning.")
		return
	}
	if err := a.Entra.DeleteWorkspaceIdentity(wi.IdentityID); err != nil {
		writeErr(w, http.StatusBadGateway, "WorkspaceIdentityDeprovisioningFailed", err.Error())
		return
	}
	if err := a.Store.DeleteWorkspaceIdentity(wid); err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	// Revoke the SP's grant; a missing assignment is fine (already revoked).
	if err := a.revokePrincipal(wid, wi.AppID); err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	a.startOperation(w, r, "DeprovisionWorkspaceIdentity", "")
}

// revokePrincipal deletes a principal's role assignment on a workspace,
// treating not-found as success.
func (a *API) revokePrincipal(wid, principalID string) error {
	ras, err := a.Store.ListRoleAssignments(wid)
	if err != nil {
		return err
	}
	for _, ra := range ras {
		if ra.Principal.ID == principalID {
			if err := a.Store.DeleteRoleAssignment(wid, ra.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
	}
	return nil
}

// identityLifecycle applies the name-follows-workspace and cascade-delete
// rules; entra failures are logged into the response only when the primary
// operation cannot proceed. Called from workspace PATCH/DELETE.
func (a *API) identityRenameFollows(wid, newName string) error {
	wi, err := a.Store.GetWorkspaceIdentity(wid)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if a.Entra == nil {
		return nil
	}
	return a.Entra.RenameWorkspaceIdentity(wi.IdentityID, newName)
}

func (a *API) identityCascadeDelete(wid string) error {
	wi, err := a.Store.GetWorkspaceIdentity(wid)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if a.Entra == nil {
		return nil
	}
	return a.Entra.DeleteWorkspaceIdentity(wi.IdentityID)
}
