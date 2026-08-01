package api

// Tenant settings:
//
//	GET  /v1/admin/tenantsettings
//	POST /v1/admin/tenantsettings/{tenantSettingName}/update
//
// The payload is the documented TenantSetting object from the Fabric REST
// reference (admin/tenants/list-tenant-settings): settingName, title, enabled,
// canSpecifySecurityGroups, tenantSettingGroup, the three delegate* flags, the
// enabled/excluded security-group lists, and typed properties. The response
// envelope is the standard paged `value` + continuationToken/continuationUri,
// and per the reference those two are *removed* when there are no more pages
// rather than sent as null.
//
// The update is a real Fabric API (admin/tenants/update-tenant-setting), not
// an emulator affordance: `enabled` is required, and the response wraps the
// result in `{"tenantSettings": [...]}` rather than returning the object bare.

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

func (a *API) registerTenantSettings(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/admin/tenantsettings", a.withAuth(a.listTenantSettings))
	mux.HandleFunc("POST /v1/admin/tenantsettings/{name}/update", a.withAuth(a.updateTenantSetting))
}

func (a *API) listTenantSettings(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	settings, err := a.Store.ListTenantSettings()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	// writePage applies the shared continuationToken/continuationUri paging,
	// which is the envelope this API documents.
	writePage(a, w, r, settings)
}

func (a *API) updateTenantSetting(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var patch struct {
		// `enabled` is documented as required.
		Enabled                  *bool                             `json:"enabled"`
		CanSpecifySecurityGroups *bool                             `json:"canSpecifySecurityGroups"`
		DelegateToCapacity       *bool                             `json:"delegateToCapacity"`
		DelegateToDomain         *bool                             `json:"delegateToDomain"`
		DelegateToWorkspace      *bool                             `json:"delegateToWorkspace"`
		EnabledSecurityGroups    *[]store.TenantSettingSecurityGrp `json:"enabledSecurityGroups"`
		ExcludedSecurityGroups   *[]store.TenantSettingSecurityGrp `json:"excludedSecurityGroups"`
		Properties               *[]store.TenantSettingProperty    `json:"properties"`
	}
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "Malformed JSON body.")
		return
	}
	if patch.Enabled == nil {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "enabled is required.")
		return
	}
	s, err := a.Store.GetTenantSetting(r.PathValue("name"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "TenantSettingNotFound",
				"No tenant setting has that settingName.")
			return
		}
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	if patch.Enabled != nil {
		s.Enabled = *patch.Enabled
	}
	if patch.CanSpecifySecurityGroups != nil {
		s.CanSpecifySecurityGroups = *patch.CanSpecifySecurityGroups
	}
	if patch.DelegateToCapacity != nil {
		s.DelegateToCapacity = *patch.DelegateToCapacity
	}
	if patch.DelegateToDomain != nil {
		s.DelegateToDomain = *patch.DelegateToDomain
	}
	if patch.DelegateToWorkspace != nil {
		s.DelegateToWorkspace = *patch.DelegateToWorkspace
	}
	if patch.EnabledSecurityGroups != nil {
		s.EnabledSecurityGroups = *patch.EnabledSecurityGroups
	}
	if patch.ExcludedSecurityGroups != nil {
		s.ExcludedSecurityGroups = *patch.ExcludedSecurityGroups
	}
	if patch.Properties != nil {
		for _, prop := range *patch.Properties {
			if !store.ValidTenantSettingPropertyType(prop.Type) {
				writeErr(w, http.StatusBadRequest, "InvalidRequest",
					"property type must be one of FreeText, Url, Boolean, MailEnabledSecurityGroup, Integer.")
				return
			}
		}
		s.Properties = *patch.Properties
	}
	// A setting enabled for the whole organization cannot also name the
	// groups it is enabled for: the reference defines canSpecifySecurityGroups
	// false as "enabled for the entire organization".
	if !s.CanSpecifySecurityGroups && len(s.EnabledSecurityGroups) > 0 {
		writeErr(w, http.StatusBadRequest, "InvalidRequest",
			"enabledSecurityGroups requires canSpecifySecurityGroups to be true.")
		return
	}
	if err := a.Store.UpdateTenantSetting(s); err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	// The documented response is a list wrapper, not the bare object.
	writeJSON(w, http.StatusOK, map[string]any{"tenantSettings": []*store.TenantSetting{s}})
}
