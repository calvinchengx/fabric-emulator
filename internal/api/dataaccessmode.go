package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// A SQL analytics endpoint's data access mode (docs/60).
//
// EMULATOR-NATIVE, because Fabric has no API for it: the mode is switched in the
// endpoint's Security tab — "Select User's identity access mode … or select
// Delegated identity access mode … Then select Apply." The route sits under the
// endpoint's own Fabric path with the `_emulator` segment naming what it is,
// and is authenticated and gated as the portal is: "an Admin or Member must
// switch it". It is not under the unauthenticated /_emulator/ control prefix,
// where anyone could change a workspace's security model.

type dataAccessModeBody struct {
	DataAccessMode string `json:"dataAccessMode"`
}

// dataAccessModeEndpoint resolves the SQL analytics endpoint in the path, the
// lakehouse it serves and its current mode, answering the error itself when it
// cannot.
func (a *API) dataAccessModeEndpoint(w http.ResponseWriter, r *http.Request, p *auth.Principal, min string) (*store.Item, *store.Item, string, bool) {
	wid := r.PathValue("wid")
	if _, _, ok := a.requireRole(w, wid, p, min); !ok {
		return nil, nil, "", false
	}
	ep, err := a.Store.GetItem(wid, r.PathValue("epid"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "ItemNotFound", "The item is not available.")
		return nil, nil, "", false
	}
	if ep.Type != "SQLEndpoint" {
		writeErr(w, http.StatusBadRequest, "DataAccessModeNotSupported",
			"Data access modes apply to a lakehouse's SQL analytics endpoint; a "+ep.Type+
				" is secured by T-SQL and nothing else.")
		return nil, nil, "", false
	}
	props, err := a.Store.ItemProperties(ep.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return nil, nil, "", false
	}
	lake, err := a.Store.GetItemByID(props[propParentLakehouse])
	if err != nil {
		writeErr(w, http.StatusNotFound, "ItemNotFound", "The SQL analytics endpoint serves no lakehouse.")
		return nil, nil, "", false
	}
	mode := store.NormalizeAccessMode(props[store.PropDataAccessMode])
	if mode == "" {
		mode = store.AccessModeDelegated
	}
	return ep, lake, mode, true
}

// getDataAccessMode reports the mode; anyone who can see the workspace can read
// it, as the Security tab shows it.
func (a *API) getDataAccessMode(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	_, _, mode, ok := a.dataAccessModeEndpoint(w, r, p, store.RoleViewer)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, dataAccessModeBody{DataAccessMode: mode})
}

// putDataAccessMode switches the mode. Setting the mode it already has changes
// nothing — no sessions closed, nothing dropped — since there is no switch.
func (a *API) putDataAccessMode(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	ep, lake, from, ok := a.dataAccessModeEndpoint(w, r, p, store.RoleMember)
	if !ok {
		return
	}
	var body dataAccessModeBody
	raw, _ := io.ReadAll(r.Body)
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "The body must be {\"dataAccessMode\": ...}: "+err.Error())
		return
	}
	to := store.NormalizeAccessMode(body.DataAccessMode)
	if to == "" {
		writeErr(w, http.StatusBadRequest, "InvalidRequest",
			"dataAccessMode must be "+store.AccessModeDelegated+" or "+store.AccessModeUserIdentity+".")
		return
	}
	if from != to {
		var err error
		if a.SwitchDataAccessMode != nil {
			err = a.SwitchDataAccessMode(r.Context(), ep, lake, to)
		} else {
			err = a.Store.SetItemProperties(ep.ID, map[string]string{store.PropDataAccessMode: to})
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, dataAccessModeBody{DataAccessMode: to})
}
