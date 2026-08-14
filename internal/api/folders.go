package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

func (a *API) getFolder(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid, fid := r.PathValue("wid"), r.PathValue("fid")
	if _, _, ok := a.requireRole(w, wid, p, store.RoleViewer); !ok {
		return
	}
	f, err := a.Store.GetFolder(wid, fid)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "FolderNotFound", "No such folder.")
			return
		}
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, f)
}

func (a *API) updateFolder(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid, fid := r.PathValue("wid"), r.PathValue("fid")
	if _, _, ok := a.requireRole(w, wid, p, store.RoleContributor); !ok {
		return
	}
	f, err := a.Store.GetFolder(wid, fid)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "FolderNotFound", "No such folder.")
			return
		}
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	var body struct {
		DisplayName *string `json:"displayName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.DisplayName == nil || *body.DisplayName == "" {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "displayName is required.")
		return
	}
	f.DisplayName = *body.DisplayName
	if err := a.Store.UpdateFolder(f); err != nil {
		if errors.Is(err, store.ErrNameConflict) {
			writeErr(w, http.StatusConflict, "FolderAlreadyExists", "A folder with this name already exists here.")
			return
		}
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, f)
}

func (a *API) deleteFolder(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid, fid := r.PathValue("wid"), r.PathValue("fid")
	if _, _, ok := a.requireRole(w, wid, p, store.RoleContributor); !ok {
		return
	}
	if err := a.Store.DeleteFolder(wid, fid); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "FolderNotFound", "No such folder.")
			return
		}
		if errors.Is(err, store.ErrFolderNotEmpty) {
			writeErr(w, http.StatusBadRequest, "FolderNotEmpty", "Move or delete the folder's contents first.")
			return
		}
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (a *API) moveFolder(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid, fid := r.PathValue("wid"), r.PathValue("fid")
	if _, _, ok := a.requireRole(w, wid, p, store.RoleContributor); !ok {
		return
	}
	var body struct {
		TargetFolderID string `json:"targetFolderId"`
	}
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "InvalidRequest", "Malformed JSON body.")
			return
		}
	}
	if err := a.Store.MoveFolder(wid, fid, body.TargetFolderID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "FolderNotFound", "No such folder.")
			return
		}
		if errors.Is(err, store.ErrFolderCycle) {
			writeErr(w, http.StatusBadRequest, "InfiniteFolderHierarchyLoop", "A folder cannot be moved under itself.")
			return
		}
		if errors.Is(err, store.ErrNameConflict) {
			writeErr(w, http.StatusConflict, "FolderAlreadyExists", "A folder with this name already exists here.")
			return
		}
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	f, err := a.Store.GetFolder(wid, fid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, f)
}
