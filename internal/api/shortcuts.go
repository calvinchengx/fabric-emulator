package api

// OneLake shortcuts: symlinks from an item's managed folder to a target
// OneLake location. Scope is OneLake-to-OneLake only — external targets (ADLS
// Gen2, S3, Dataverse) need real cloud credentials an offline emulator cannot
// honor, so they 501. Data-plane resolution + target-side RBAC live in the
// OneLake surface (internal/onelake).

import (
	"encoding/json"
	"errors"
	"net/http"
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

// shortcutBody is the wire shape (target is OneLake-only here).
type shortcutBody struct {
	Path   string `json:"path"`
	Name   string `json:"name"`
	Target struct {
		OneLake *struct {
			WorkspaceID string `json:"workspaceId"`
			ItemID      string `json:"itemId"`
			Path        string `json:"path"`
		} `json:"oneLake"`
		// Any other target kind (adlsGen2, amazonS3, dataverse, …) present →
		// unsupported.
		ADLSGen2  json.RawMessage `json:"adlsGen2"`
		AmazonS3  json.RawMessage `json:"amazonS3"`
		Dataverse json.RawMessage `json:"dataverse"`
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
	if body.Target.ADLSGen2 != nil || body.Target.AmazonS3 != nil || body.Target.Dataverse != nil {
		writeErr(w, http.StatusNotImplemented, "ExternalShortcutTargetUnsupported",
			"The emulator supports OneLake-to-OneLake shortcuts only; external targets need real cloud credentials.")
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
		TargetWorkspace: ol.WorkspaceID, TargetItem: ol.ItemID, TargetPath: strings.Trim(ol.Path, "/"),
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
	writePage(w, r, out)
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
	return map[string]any{
		"path": sc.Path, "name": sc.Name,
		"target": map[string]any{
			"type":    "OneLake",
			"oneLake": map[string]any{"workspaceId": sc.TargetWorkspace, "itemId": sc.TargetItem, "path": sc.TargetPath},
		},
	}
}
