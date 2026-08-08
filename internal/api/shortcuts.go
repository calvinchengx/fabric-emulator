package api

// OneLake shortcuts: symlinks from an item's managed folder to a OneLake,
// ADLS Gen2, Amazon S3 or Dataverse target. The reference documents four more
// (Google Cloud Storage, S3 compatible, Azure Blob Storage, OneDrive
// SharePoint); those are still refused rather than stubbed.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

func (a *API) registerShortcuts(mux *http.ServeMux) {
	base := "/v1/workspaces/{wid}/items/{iid}/shortcuts"
	mux.HandleFunc("POST "+base, a.withAuth(a.createShortcut))
	mux.HandleFunc("GET "+base, a.withAuth(a.listShortcuts))
	mux.HandleFunc("GET "+base+"/{path}/{name}", a.withAuth(a.getShortcut))
	mux.HandleFunc("DELETE "+base+"/{path}/{name}", a.withAuth(a.deleteShortcut))
}

// shortcutBody is the wire shape. The reference says the target "must specify
// exactly one of the supported destinations", so each kind is its own optional
// pointer and presence selects the branch.
type shortcutBody struct {
	Path   string `json:"path"`
	Name   string `json:"name"`
	Target struct {
		OneLake *struct {
			WorkspaceID string `json:"workspaceId"`
			ItemID      string `json:"itemId"`
			Path        string `json:"path"`
		} `json:"oneLake"`
		ADLSGen2 *struct {
			Location     string `json:"location"`
			Subpath      string `json:"subpath"`
			ConnectionID string `json:"connectionId"`
		} `json:"adlsGen2"`
		AmazonS3 *struct {
			Location     string `json:"location"`
			Subpath      string `json:"subpath"`
			ConnectionID string `json:"connectionId"`
		} `json:"amazonS3"`
		// Dataverse is the one documented target with no `location`: the
		// endpoint is `environmentDomain`, and the data sits at
		// `deltaLakeFolder` / `tableName` beneath it.
		Dataverse *struct {
			ConnectionID      string `json:"connectionId"`
			DeltaLakeFolder   string `json:"deltaLakeFolder"`
			EnvironmentDomain string `json:"environmentDomain"`
			TableName         string `json:"tableName"`
		} `json:"dataverse"`
	} `json:"target"`
}

func (a *API) createShortcut(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	if _, _, ok := a.requireRole(w, wid, p, store.RoleContributor); !ok {
		return
	}
	it, err := a.Store.GetItem(wid, r.PathValue("iid"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "ItemNotFound", "The item is not available.")
		return
	}
	var body shortcutBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil ||
		strings.TrimSpace(body.Name) == "" || strings.TrimSpace(body.Path) == "" {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "path and name are required.")
		return
	}
	if dv := body.Target.Dataverse; dv != nil {
		// Every field the reference marks on the Dataverse target is required
		// here, because none of them has a defensible default: the endpoint,
		// the folder and the table together ARE the location, and a shortcut
		// missing one of them cannot resolve to anything.
		u, parseErr := url.Parse(dv.EnvironmentDomain)
		if parseErr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
			strings.TrimSpace(dv.DeltaLakeFolder) == "" || strings.TrimSpace(dv.TableName) == "" ||
			dv.ConnectionID == "" {
			writeErr(w, http.StatusBadRequest, "InvalidRequest",
				"dataverse targets require an http(s) environmentDomain, deltaLakeFolder, tableName and connectionId.")
			return
		}
		if _, err := a.Store.GetConnection(dv.ConnectionID); err != nil {
			writeErr(w, http.StatusBadRequest, "ConnectionNotFound", "connectionId does not resolve to a connection.")
			return
		}
		sc := &store.Shortcut{ItemID: it.ID, Path: strings.Trim(body.Path, "/"), Name: body.Name,
			TargetType:     "Dataverse",
			TargetLocation: strings.TrimRight(dv.EnvironmentDomain, "/"),
			TargetPath:     strings.Trim(dv.DeltaLakeFolder, "/"),
			TargetTable:    strings.Trim(dv.TableName, "/"),
			ConnectionID:   dv.ConnectionID}
		if err := a.Store.CreateShortcut(sc); err != nil {
			writeErr(w, http.StatusConflict, "ShortcutAlreadyExists", "A shortcut with this path and name already exists.")
			return
		}
		writeJSON(w, http.StatusCreated, shortcutDTO(sc))
		return
	}
	if body.Target.ADLSGen2 != nil || body.Target.AmazonS3 != nil {
		typeName, location, subpath, connectionID := "ADLSGen2", "", "", ""
		if ext := body.Target.ADLSGen2; ext != nil {
			location, subpath, connectionID = ext.Location, ext.Subpath, ext.ConnectionID
		} else {
			typeName = "AmazonS3"
			location, subpath, connectionID = body.Target.AmazonS3.Location, body.Target.AmazonS3.Subpath, body.Target.AmazonS3.ConnectionID
		}
		u, parseErr := url.Parse(location)
		if parseErr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || connectionID == "" {
			writeErr(w, http.StatusBadRequest, "InvalidRequest", "external targets require an http(s) location and connectionId.")
			return
		}
		if _, err := a.Store.GetConnection(connectionID); err != nil {
			writeErr(w, http.StatusBadRequest, "ConnectionNotFound", "connectionId does not resolve to a connection.")
			return
		}
		sc := &store.Shortcut{ItemID: it.ID, Path: strings.Trim(body.Path, "/"), Name: body.Name,
			TargetType: typeName, TargetLocation: strings.TrimRight(location, "/"), TargetPath: strings.Trim(subpath, "/"), ConnectionID: connectionID}
		if err := a.Store.CreateShortcut(sc); err != nil {
			writeErr(w, http.StatusConflict, "ShortcutAlreadyExists", "A shortcut with this path and name already exists.")
			return
		}
		writeJSON(w, http.StatusCreated, shortcutDTO(sc))
		return
	}
	ol := body.Target.OneLake
	if ol == nil || ol.WorkspaceID == "" || ol.ItemID == "" {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "target.oneLake with workspaceId and itemId is required.")
		return
	}
	// The target must exist (a dangling create is rejected; a target that is
	// later deleted dangles at read time, matching real OneLake).
	if _, err := a.Store.GetItem(ol.WorkspaceID, ol.ItemID); err != nil {
		writeErr(w, http.StatusBadRequest, "TargetNotFound", "The shortcut target item does not exist.")
		return
	}
	// Reject a self-referential cycle: a shortcut cannot point at its own
	// item under a path that would resolve back through it.
	if ol.WorkspaceID == wid && ol.ItemID == it.ID {
		writeErr(w, http.StatusBadRequest, "InvalidTarget", "A shortcut cannot target its own item.")
		return
	}
	sc := &store.Shortcut{
		ItemID: it.ID, Path: strings.Trim(body.Path, "/"), Name: body.Name,
		TargetWorkspace: ol.WorkspaceID, TargetItem: ol.ItemID, TargetPath: strings.Trim(ol.Path, "/"), TargetType: "OneLake",
	}
	if err := a.Store.CreateShortcut(sc); err != nil {
		writeErr(w, http.StatusConflict, "ShortcutAlreadyExists", "A shortcut with this path and name already exists.")
		return
	}
	writeJSON(w, http.StatusCreated, shortcutDTO(sc))
}

func (a *API) listShortcuts(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	if _, _, ok := a.requireRole(w, wid, p, store.RoleViewer); !ok {
		return
	}
	it, err := a.Store.GetItem(wid, r.PathValue("iid"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "ItemNotFound", "The item is not available.")
		return
	}
	scs, err := a.Store.ListShortcuts(it.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	out := make([]any, 0, len(scs))
	for _, sc := range scs {
		out = append(out, shortcutDTO(sc))
	}
	writePage(a, w, r, out)
}

func (a *API) getShortcut(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	if _, _, ok := a.requireRole(w, wid, p, store.RoleViewer); !ok {
		return
	}
	it, err := a.Store.GetItem(wid, r.PathValue("iid"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "ItemNotFound", "The item is not available.")
		return
	}
	sc, err := a.Store.GetShortcut(it.ID, r.PathValue("path"), r.PathValue("name"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "ShortcutNotFound", "No such shortcut.")
		return
	}
	writeJSON(w, http.StatusOK, shortcutDTO(sc))
}

func (a *API) deleteShortcut(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	if _, _, ok := a.requireRole(w, wid, p, store.RoleContributor); !ok {
		return
	}
	it, err := a.Store.GetItem(wid, r.PathValue("iid"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "ItemNotFound", "The item is not available.")
		return
	}
	if err := a.Store.DeleteShortcut(it.ID, r.PathValue("path"), r.PathValue("name")); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "ShortcutNotFound", "No such shortcut.")
			return
		}
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}

// shortcutDTO is the response shape (target echoed as oneLake).
func shortcutDTO(sc *store.Shortcut) map[string]any {
	// Dataverse echoes its own four fields and NOT location/subpath: it is the
	// only documented target addressed by environment + folder + table rather
	// than by a storage URL, so reusing the storage shape would invent fields
	// the reference does not have and drop two that it does.
	if sc.TargetType == "Dataverse" {
		return map[string]any{"path": sc.Path, "name": sc.Name, "target": map[string]any{
			"type": "Dataverse", "dataverse": map[string]any{
				"connectionId":      sc.ConnectionID,
				"deltaLakeFolder":   sc.TargetPath,
				"environmentDomain": sc.TargetLocation,
				"tableName":         sc.TargetTable,
			},
		}}
	}
	if sc.TargetType == "ADLSGen2" || sc.TargetType == "AmazonS3" {
		// The wire value is the documented `Type` enum member, which for ADLS
		// is **AdlsGen2** — not the "ADLSGen2" spelling used for the stored
		// column and the Go constants. Those stay as they are (changing them
		// would need a data migration for no gain); only the response is
		// corrected, and no caller in the tree reads this field.
		key, typeName := "adlsGen2", "AdlsGen2"
		if sc.TargetType == "AmazonS3" {
			key, typeName = "amazonS3", "AmazonS3"
		}
		return map[string]any{"path": sc.Path, "name": sc.Name, "target": map[string]any{
			"type": typeName, key: map[string]any{"location": sc.TargetLocation, "subpath": "/" + sc.TargetPath, "connectionId": sc.ConnectionID},
		}}
	}
	return map[string]any{
		"path": sc.Path, "name": sc.Name,
		"target": map[string]any{
			"type":    "OneLake",
			"oneLake": map[string]any{"workspaceId": sc.TargetWorkspace, "itemId": sc.TargetItem, "path": sc.TargetPath},
		},
	}
}
