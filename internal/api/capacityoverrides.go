package api

// Capacity-level tenant setting overrides:
//
//	GET  /v1/admin/capacities/delegatedTenantSettingOverrides
//	POST /v1/admin/capacities/{capacityId}/delegatedTenantSettingOverrides/{tenantSettingName}/update
//
// Per admin/tenants/list-capacities-tenant-settings-overrides. Note there is
// **no `/v1/admin/capacities` list API** in Fabric — capacity listing lives on
// the Core surface at `/v1/capacities`. The only capacity-scoped admin API is
// this override list, and its envelope key is `value` (unlike the admin
// workspace and item surfaces).
//
// Both are real Fabric APIs. The update request body carries only `enabled`
// (required), `delegateToWorkspace` and the two security-group lists — notably
// *not* `properties` or `delegatedFrom`, which the tenant-level update does
// accept. Its response wraps the result in `{"overrides": [...]}`.

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

func (a *API) registerCapacityOverrides(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/admin/capacities/delegatedTenantSettingOverrides",
		a.withTenantRead(a.listCapacityOverrides))
	mux.HandleFunc("POST /v1/admin/capacities/{cid}/delegatedTenantSettingOverrides/{name}/update",
		a.withTenantAdmin(a.putCapacityOverride))
}

// capacityOverride is the documented CapacityTenantSettingOverride: a capacity
// id and the settings overridden on it.
type capacityOverride struct {
	ID             string                        `json:"id"`
	TenantSettings []store.CapacityTenantSetting `json:"tenantSettings"`
}

func (a *API) listCapacityOverrides(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	order, byCapacity, err := a.Store.CapacityOverrides()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	out := make([]capacityOverride, 0, len(order))
	for _, capacityID := range order {
		out = append(out, capacityOverride{ID: capacityID, TenantSettings: byCapacity[capacityID]})
	}
	writePage(a, w, r, out)
}

func (a *API) putCapacityOverride(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	capacityID, name := r.PathValue("cid"), r.PathValue("name")
	if _, err := a.Store.GetCapacity(capacityID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "CapacityNotFound", "No capacity matches that id.")
			return
		}
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	// The override starts from the tenant-level setting, so an override is
	// always a modification of a real setting rather than a free-floating one.
	base, err := a.Store.GetTenantSetting(name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "TenantSettingNotFound",
				"No tenant setting has that settingName.")
			return
		}
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	if !base.DelegateToCapacity {
		writeErr(w, http.StatusBadRequest, "InvalidRequest",
			"That tenant setting is not delegated to capacities (delegateToCapacity is false).")
		return
	}

	// The documented request body — no `properties` and no `delegatedFrom`,
	// unlike the tenant-level update.
	var patch struct {
		Enabled                *bool                             `json:"enabled"`
		DelegateToWorkspace    *bool                             `json:"delegateToWorkspace"`
		EnabledSecurityGroups  *[]store.TenantSettingSecurityGrp `json:"enabledSecurityGroups"`
		ExcludedSecurityGroups *[]store.TenantSettingSecurityGrp `json:"excludedSecurityGroups"`
	}
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "Malformed JSON body.")
		return
	}
	if patch.Enabled == nil {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "enabled is required.")
		return
	}

	override := store.CapacityTenantSetting{
		SettingName: base.SettingName, Title: base.Title,
		Enabled: base.Enabled, CanSpecifySecurityGroups: base.CanSpecifySecurityGroups,
		TenantSettingGroup:     base.TenantSettingGroup,
		EnabledSecurityGroups:  base.EnabledSecurityGroups,
		ExcludedSecurityGroups: base.ExcludedSecurityGroups,
		Properties:             base.Properties,
		DelegateToWorkspace:    base.DelegateToWorkspace,
		// An override created at a capacity was delegated from the tenant;
		// the request body has no say in this.
		DelegatedFrom: store.DelegatedFromTenant,
	}
	override.Enabled = *patch.Enabled
	if patch.DelegateToWorkspace != nil {
		override.DelegateToWorkspace = *patch.DelegateToWorkspace
	}
	if patch.EnabledSecurityGroups != nil {
		override.EnabledSecurityGroups = *patch.EnabledSecurityGroups
	}
	if patch.ExcludedSecurityGroups != nil {
		override.ExcludedSecurityGroups = *patch.ExcludedSecurityGroups
	}
	if err := a.Store.SetCapacityOverride(capacityID, &override); err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	// The documented response wraps the result in `overrides`.
	writeJSON(w, http.StatusOK, map[string]any{
		"overrides": []store.CapacityTenantSetting{override},
	})
}
