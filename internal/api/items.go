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
	if _, ok := a.requireRole(w, wid, p, store.RoleViewer); !ok {
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
	writeJSON(w, http.StatusOK, map[string]any{"value": items})
}

// createItem: without a definition it completes synchronously (201, like the
// real API); with a definition it is a long-running operation (202 → poll →
// result), which is what fabric-cicd and git tooling exercise.
func (a *API) createItem(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	if _, ok := a.requireRole(w, wid, p, store.RoleContributor); !ok {
		return
	}
	var body struct {
		DisplayName string `json:"displayName"`
		Type        string `json:"type"`
		Description string `json:"description"`
		Definition  *struct {
			Parts []store.DefinitionPart `json:"parts"`
		} `json:"definition"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil ||
		strings.TrimSpace(body.DisplayName) == "" || strings.TrimSpace(body.Type) == "" {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "displayName and type are required.")
		return
	}
	it := &store.Item{WorkspaceID: wid, Type: body.Type, DisplayName: body.DisplayName, Description: body.Description}
	var parts []store.DefinitionPart
	if body.Definition != nil {
		parts = body.Definition.Parts
	}
	if err := a.Store.CreateItem(it, parts); err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	if body.Definition == nil {
		writeJSON(w, http.StatusCreated, it)
		return
	}
	a.startOperation(w, r, "CreateItem", it.ID)
}

func (a *API) getItem(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	if _, ok := a.requireRole(w, wid, p, store.RoleViewer); !ok {
		return
	}
	it, err := a.Store.GetItem(wid, r.PathValue("iid"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "ItemNotFound", "The item is not available.")
		return
	}
	writeJSON(w, http.StatusOK, it)
}

func (a *API) updateItem(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	if _, ok := a.requireRole(w, wid, p, store.RoleContributor); !ok {
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
	if body.DisplayName != nil {
		it.DisplayName = *body.DisplayName
	}
	if body.Description != nil {
		it.Description = *body.Description
	}
	if err := a.Store.UpdateItem(it); err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, it)
}

func (a *API) deleteItem(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	if _, ok := a.requireRole(w, wid, p, store.RoleContributor); !ok {
		return
	}
	if err := a.Store.DeleteItem(wid, r.PathValue("iid")); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "ItemNotFound", "The item is not available.")
			return
		}
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
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
	default:
		w.WriteHeader(http.StatusOK)
	}
}
