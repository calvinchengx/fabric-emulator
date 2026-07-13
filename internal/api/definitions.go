package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

func readCloser(b []byte) io.ReadCloser { return io.NopCloser(bytes.NewReader(b)) }

// definitionEnvelope is the wire shape of an item definition: base64 parts
// plus the .platform metadata file (the CI/CD source format).
type definitionEnvelope struct {
	Definition struct {
		Parts []store.DefinitionPart `json:"parts"`
	} `json:"definition"`
}

// getDefinition returns the stored parts verbatim — exactly what
// updateDefinition or git wrote. Definitions expose item source, so reading
// them requires write-level (Contributor) access like real Fabric.
func (a *API) getDefinition(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	if _, _, ok := a.requireRole(w, wid, p, store.RoleContributor); !ok {
		return
	}
	it, err := a.Store.GetItem(wid, r.PathValue("iid"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "ItemNotFound", "The item is not available.")
		return
	}
	parts, err := a.Store.GetDefinition(it.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	var env definitionEnvelope
	env.Definition.Parts = parts
	if env.Definition.Parts == nil {
		env.Definition.Parts = []store.DefinitionPart{}
	}
	writeJSON(w, http.StatusOK, env)
}

// updateDefinition replaces the parts and reports through the LRO engine
// (202 → poll), the shape fabric-cicd drives.
func (a *API) updateDefinition(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	if _, _, ok := a.requireRole(w, wid, p, store.RoleContributor); !ok {
		return
	}
	it, err := a.Store.GetItem(wid, r.PathValue("iid"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "ItemNotFound", "The item is not available.")
		return
	}
	var env definitionEnvelope
	if err := json.NewDecoder(r.Body).Decode(&env); err != nil || len(env.Definition.Parts) == 0 {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "definition.parts is required.")
		return
	}
	if err := a.Store.SetDefinition(it.ID, env.Definition.Parts); err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	// No resultRef: like real Fabric, this LRO has no result, so the poll
	// response carries no Location and clients stop at Succeeded.
	a.startOperation(w, r, "UpdateItemDefinition", "")
}

// typedCollections maps the typed REST collections to the item type they
// alias — one generic implementation covers every item kind.
var typedCollections = map[string]string{
	"notebooks":           "Notebook",
	"lakehouses":          "Lakehouse",
	"warehouses":          "Warehouse",
	"dataPipelines":       "DataPipeline",
	"semanticModels":      "SemanticModel",
	"reports":             "Report",
	"environments":        "Environment",
	"eventhouses":         "Eventhouse",
	"kqlDatabases":        "KQLDatabase",
	"sparkJobDefinitions": "SparkJobDefinition",
	"mirroredDatabases":   "MirroredDatabase",
	"eventstreams":        "Eventstream",
}

// registerTyped mounts the typed collections as thin aliases: list/create on
// the collection, get/patch/delete on members — all forcing the mapped type.
func (a *API) registerTyped(mux *http.ServeMux) {
	for collection, itemType := range typedCollections {
		mux.HandleFunc("GET /v1/workspaces/{wid}/"+collection, a.withAuth(a.typedList(itemType)))
		mux.HandleFunc("POST /v1/workspaces/{wid}/"+collection, a.withAuth(a.typedCreate(itemType)))
		mux.HandleFunc("GET /v1/workspaces/{wid}/"+collection+"/{iid}", a.withAuth(a.typedGet(itemType)))
		mux.HandleFunc("PATCH /v1/workspaces/{wid}/"+collection+"/{iid}", a.withAuth(a.updateItem))
		mux.HandleFunc("DELETE /v1/workspaces/{wid}/"+collection+"/{iid}", a.withAuth(a.deleteItem))
	}
}

func (a *API) typedList(itemType string) handler {
	return func(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
		q := r.URL.Query()
		q.Set("type", itemType)
		r.URL.RawQuery = q.Encode()
		a.listItems(w, r, p)
	}
}

// typedCreate rewrites the body to the generic create with the type forced.
func (a *API) typedCreate(itemType string) handler {
	return func(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "InvalidRequest", "Malformed JSON body.")
			return
		}
		t, _ := json.Marshal(itemType)
		body["type"] = t
		raw, _ := json.Marshal(body)
		r2 := r.Clone(r.Context())
		r2.Body = readCloser(raw)
		a.createItem(w, r2, p)
	}
}

// typedGet 404s items of a different type than the collection (a notebook is
// not addressable under /lakehouses).
func (a *API) typedGet(itemType string) handler {
	return func(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
		wid := r.PathValue("wid")
		if _, _, ok := a.requireRole(w, wid, p, store.RoleViewer); !ok {
			return
		}
		it, err := a.Store.GetItem(wid, r.PathValue("iid"))
		if err != nil || it.Type != itemType {
			writeErr(w, http.StatusNotFound, "ItemNotFound", "The item is not available.")
			return
		}
		writeJSON(w, http.StatusOK, it)
	}
}

// listFolders returns the workspace's folders (fabric-cicd lists these on
// every publish to map folder paths to ids).
func (a *API) listFolders(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	if _, _, ok := a.requireRole(w, wid, p, store.RoleViewer); !ok {
		return
	}
	fs, err := a.Store.ListFolders(wid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	if fs == nil {
		fs = []*store.Folder{}
	}
	writePage(w, r, fs)
}

// createFolder creates a folder (201, sync).
func (a *API) createFolder(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	if _, _, ok := a.requireRole(w, wid, p, store.RoleContributor); !ok {
		return
	}
	var body struct {
		DisplayName    string `json:"displayName"`
		ParentFolderID string `json:"parentFolderId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.DisplayName == "" {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "displayName is required.")
		return
	}
	f := &store.Folder{WorkspaceID: wid, DisplayName: body.DisplayName, ParentFolderID: body.ParentFolderID}
	if err := a.Store.CreateFolder(f); err != nil {
		writeErr(w, http.StatusConflict, "FolderAlreadyExists", "A folder with this name already exists here.")
		return
	}
	writeJSON(w, http.StatusCreated, f)
}
