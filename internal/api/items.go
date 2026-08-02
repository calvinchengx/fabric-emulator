package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

func (a *API) listItems(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	if _, _, ok := a.requireRole(w, wid, p, store.RoleViewer); !ok {
		return
	}
	items, err := a.Store.ListItems(wid, r.URL.Query().Get("type"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	if items == nil {
		items = []*store.Item{}
	}
	writePage(a, w, r, items)
}

// createItem: without a definition it completes synchronously (201, like the
// real API); with a definition it is a long-running operation (202 → poll →
// result), which is what fabric-cicd and git tooling exercise.
func (a *API) createItem(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	if _, _, ok := a.requireRole(w, wid, p, store.RoleContributor); !ok {
		return
	}
	var body struct {
		DisplayName string `json:"displayName"`
		Type        string `json:"type"`
		Description string `json:"description"`
		// Workspace folder to create the item in; omitted means the root.
		// fabric-cicd sends this when the repository nests items in folders.
		FolderID   string `json:"folderId"`
		Definition *struct {
			Parts []store.DefinitionPart `json:"parts"`
		} `json:"definition"`
		// creationPayload carries per-type creation settings — a KQL
		// Database's parentEventhouseItemId / databaseType, for instance
		// (fabric-docs real-time-intelligence/eventhouse-deploy-with-fabric-api.md).
		CreationPayload map[string]any `json:"creationPayload"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil ||
		strings.TrimSpace(body.DisplayName) == "" || strings.TrimSpace(body.Type) == "" {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "displayName and type are required.")
		return
	}
	// Real Fabric rejects a type outside the documented ItemType enumeration
	// with InvalidItemType. Matching is case-insensitive and the canonical
	// spelling is stored, so `notebook` and `Notebook` stay one type.
	itemType, known := store.CanonicalItemType(body.Type)
	if !known {
		writeErr(w, http.StatusBadRequest, "InvalidItemType", "Invalid item type.")
		return
	}
	body.Type = itemType
	if taken, err := a.Store.ItemNameTaken(wid, body.DisplayName, body.Type, ""); err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	} else if taken {
		writeErr(w, http.StatusConflict, "ItemDisplayNameAlreadyInUse",
			"An item of this type with this display name already exists in the workspace.")
		return
	}
	it := &store.Item{WorkspaceID: wid, Type: body.Type, DisplayName: body.DisplayName,
		Description: body.Description, FolderID: strings.TrimSpace(body.FolderID)}
	var parts []store.DefinitionPart
	if body.Definition != nil {
		parts = body.Definition.Parts
	}
	if err := a.Store.CreateItem(it, parts); err != nil {
		// The pre-check above catches the ordinary case; this is the
		// concurrent-create race the DB constraint closes.
		if errors.Is(err, store.ErrNameConflict) {
			writeErr(w, http.StatusConflict, "ItemDisplayNameAlreadyInUse",
				"An item of this type with this display name already exists in the workspace.")
			return
		}
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	a.applyCreationPayload(it, body.CreationPayload)
	// A Direct Lake model declares which lakehouse tables it reads, so its
	// definition is the moment that binding becomes a recordable hop.
	if it.Type == "SemanticModel" {
		a.recordModelLineage(it, p)
	}
	a.audit(p, &store.ActivityEvent{Operation: store.OpCreateArtifact,
		WorkspaceID: it.WorkspaceID, ArtifactID: it.ID, ArtifactName: it.DisplayName,
		Properties: map[string]any{"ArtifactKind": it.Type}})
	if body.Definition == nil {
		writeJSON(w, http.StatusCreated, a.itemView(r, it))
		return
	}
	a.startOperation(w, r, "CreateItem", it.ID)
}

func (a *API) getItem(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	if _, _, ok := a.requireRole(w, wid, p, store.RoleViewer); !ok {
		return
	}
	it, err := a.Store.GetItem(wid, r.PathValue("iid"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "ItemNotFound", "The item is not available.")
		return
	}
	writeJSON(w, http.StatusOK, a.itemView(r, it))
}

func (a *API) updateItem(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	if _, _, ok := a.requireRole(w, wid, p, store.RoleContributor); !ok {
		return
	}
	it, err := a.Store.GetItem(wid, r.PathValue("iid"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "ItemNotFound", "The item is not available.")
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
	if body.DisplayName != nil && *body.DisplayName != it.DisplayName {
		if taken, err := a.Store.ItemNameTaken(wid, *body.DisplayName, it.Type, it.ID); err != nil {
			writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
			return
		} else if taken {
			writeErr(w, http.StatusConflict, "ItemDisplayNameAlreadyInUse",
				"An item of this type with this display name already exists in the workspace.")
			return
		}
		it.DisplayName = *body.DisplayName
	}
	if body.Description != nil {
		it.Description = *body.Description
	}
	if err := a.Store.UpdateItem(it); err != nil {
		if errors.Is(err, store.ErrNameConflict) {
			writeErr(w, http.StatusConflict, "ItemDisplayNameAlreadyInUse",
				"An item of this type with this display name already exists in the workspace.")
			return
		}
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	a.audit(p, &store.ActivityEvent{Operation: store.OpUpdateArtifact,
		WorkspaceID: it.WorkspaceID, ArtifactID: it.ID, ArtifactName: it.DisplayName,
		Properties: map[string]any{"ArtifactKind": it.Type}})
	writeJSON(w, http.StatusOK, it)
}

func (a *API) deleteItem(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	if _, _, ok := a.requireRole(w, wid, p, store.RoleContributor); !ok {
		return
	}
	// Read the item before removing it: the audit record names what was
	// deleted, and after the delete there is nothing left to name it from.
	deleted, _ := a.Store.GetItem(wid, r.PathValue("iid"))
	if err := a.Store.DeleteItem(wid, r.PathValue("iid")); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "ItemNotFound", "The item is not available.")
			return
		}
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	if deleted != nil {
		a.audit(p, &store.ActivityEvent{Operation: store.OpDeleteArtifact,
			WorkspaceID: wid, ArtifactID: deleted.ID, ArtifactName: deleted.DisplayName,
			Properties: map[string]any{"ArtifactKind": deleted.Type}})
	}
	w.WriteHeader(http.StatusOK)
}

// ---- operations ----

// operationBody is the poll response.
type operationBody struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Error  *struct {
		ErrorCode string `json:"errorCode"`
		Message   string `json:"message"`
	} `json:"error,omitempty"`
}

func (a *API) getOperation(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	op, err := a.Store.GetOperation(r.PathValue("oid"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "OperationNotFound", "No such operation.")
		return
	}
	body := operationBody{ID: op.ID, Status: op.StatusAt(a.Store.Now())}
	if body.Status == store.OpFailed {
		body.Error = &struct {
			ErrorCode string `json:"errorCode"`
			Message   string `json:"message"`
		}{ErrorCode: op.FailWith, Message: "The operation failed."}
	}
	if body.Status == store.OpSucceeded && op.ResultRef != "" {
		loc := "https://" + r.Host + "/v1/operations/" + op.ID + "/result"
		w.Header().Set("Location", loc)
	}
	writeJSON(w, http.StatusOK, body)
}

func (a *API) getOperationResult(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	op, err := a.Store.GetOperation(r.PathValue("oid"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "OperationNotFound", "No such operation.")
		return
	}
	if op.StatusAt(a.Store.Now()) != store.OpSucceeded {
		writeErr(w, http.StatusBadRequest, "OperationNotComplete", "The operation has not succeeded.")
		return
	}
	switch op.Kind {
	case "CreateItem":
		it, err := a.Store.GetItemByID(op.ResultRef)
		if err != nil {
			writeErr(w, http.StatusNotFound, "ItemNotFound", "The operation result is gone.")
			return
		}
		writeJSON(w, http.StatusOK, it)
	case "Deploy":
		// "For 24 hours after the deployment is completed, the extended
		// deployment information is available in the Get Operation Result
		// API" — here it lives as long as the store does.
		dep, err := a.Store.GetDeploymentOperationByID(op.ResultRef)
		if err != nil {
			writeErr(w, http.StatusNotFound, "OperationNotFound", "The deployment result is gone.")
			return
		}
		writeJSON(w, http.StatusOK, dep)
	default:
		// Always JSON with a Content-Type — clients (fabric-cicd) parse the
		// header unconditionally.
		writeJSON(w, http.StatusOK, map[string]any{})
	}
}

// moveItem reparents an item into a workspace folder.
//
//	POST /v1/workspaces/{wid}/items/{iid}/move  {"targetFolderId": "<guid>"}
//
// fabric-cicd calls this on every redeploy where the item's folder in the
// repository differs from the deployed one, so without it a second publish of
// any repository that nests items in folders fails on every nested item.
// An empty or omitted targetFolderId moves the item back to the workspace root.
func (a *API) moveItem(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid, iid := r.PathValue("wid"), r.PathValue("iid")
	if _, _, ok := a.requireRole(w, wid, p, store.RoleContributor); !ok {
		return
	}
	it, err := a.Store.GetItem(wid, iid)
	if err != nil || it == nil {
		writeErr(w, http.StatusNotFound, "ItemNotFound", "item not found")
		return
	}
	var body struct {
		TargetFolderID string `json:"targetFolderId"`
	}
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "InvalidRequest", "malformed body")
			return
		}
	}
	target := strings.TrimSpace(body.TargetFolderID)
	// Fabric rejects a move into a folder that does not exist in the workspace;
	// silently accepting it would let a bad deploy look successful.
	if target != "" {
		folders, err := a.Store.ListFolders(wid)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
			return
		}
		found := false
		for _, f := range folders {
			if f.ID == target {
				found = true
				break
			}
		}
		if !found {
			writeErr(w, http.StatusNotFound, "FolderNotFound", "target folder not found in this workspace")
			return
		}
	}
	if err := a.Store.MoveItem(wid, iid, target); err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	it.FolderID = target
	writeJSON(w, http.StatusOK, it)
}
