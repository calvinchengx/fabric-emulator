package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

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
	// The documented alternative outcome: 202 + an operation whose /result
	// carries the definition. Both are legal for this API and a real tenant
	// answers 202, so this exists to make a client PROVE it handles the async
	// half — the 200 path alone lets a client read the 202 body, get `null`,
	// and report an empty definition instead of an error.
	if a.ForceLRO {
		a.startOperation(w, r, "GetItemDefinition", it.ID)
		return
	}
	parts, err := a.Store.GetDefinition(it.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, definitionResponse(parts))
}

// definitionResponse wraps parts in the documented envelope, normalising a nil
// part list to [] so a client always sees the array it parses.
func definitionResponse(parts []store.DefinitionPart) definitionEnvelope {
	var env definitionEnvelope
	env.Definition.Parts = parts
	if env.Definition.Parts == nil {
		env.Definition.Parts = []store.DefinitionPart{}
	}
	return env
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
	if err := a.Store.SetDefinition(
		it.ID, notebookExecutableParts(it.Type, env.Definition.Parts)); err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	if it.Type == "SemanticModel" {
		a.recordModelLineage(it, p)
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
	"sqlDatabases":        "SQLDatabase",
	"apacheAirflowJobs":   "ApacheAirflowJob",
	"dataflows":           "Dataflow",
	"mlExperiments":       "MLExperiment",
	"mlModels":            "MLModel",
	// Item types the REST reference documents but that had no typed alias.
	// The type names are taken from request/response payloads in fabric-docs
	// (`"type": "CopyJob"`, `items?type=KQLDashboard`, …); the collection
	// segment follows Fabric's camelCase-plural convention, and `copyJobs`
	// and `reflexes` appear verbatim as URL segments there. These are pure
	// aliases over the generic item surface, which already accepts the type.
	"copyJobs":           "CopyJob",
	"kqlDashboards":      "KQLDashboard",
	"kqlQuerysets":       "KQLQueryset",
	"reflexes":           "Reflex",
	"warehouseSnapshots": "WarehouseSnapshot",
	// Collection segments are NOT derivable from the type name — the REST
	// reference spells GraphQLApi's collection `GraphQLApis` (capitalised) but
	// VariableLibrary's `variableLibraries`. Each is taken from its own
	// reference page rather than generated.
	"GraphQLApis":       "GraphQLApi",
	"variableLibraries": "VariableLibrary",
}

// collectionSpellings returns the path segments a real client may send for a
// typed collection: the documented one, plus the same word with an initial
// capital.
//
// THIS IS NOT TIDINESS, IT IS A MEASURED INTEROP FIX. Go's ServeMux matches
// paths case-sensitively; ASP.NET, which real Fabric runs on, does not. So a
// casing real Fabric accepts 404s here, and the divergence is invisible until
// a real client hits it.
//
// It was found exactly that way: **fabric-cicd — Microsoft's own publishing
// tool — hardcodes `/VariableLibraries/`** (`_variablelibrary.py`), while
// Microsoft's REST reference documents `variableLibraries`. Both work against
// a tenant; only the documented one worked here, so publishing a Variable
// Library with Microsoft's own tool failed against the emulator and succeeded
// against Fabric. That is the worst direction for a fidelity bug to point,
// because the emulator looks stricter than the thing it emulates.
//
// Registering the initial-capital variant covers the observed shape without
// inventing arbitrary casings. `GraphQLApis` is already capitalised in the
// reference and is unchanged by this.
func collectionSpellings(collection string) []string {
	upper := strings.ToUpper(collection[:1]) + collection[1:]
	if upper == collection {
		return []string{collection}
	}
	return []string{collection, upper}
}

// registerTyped mounts the typed collections as thin aliases: list/create on
// the collection, get/patch/delete on members — all forcing the mapped type.
func (a *API) registerTyped(mux *http.ServeMux) {
	for collection, itemType := range typedCollections {
		for _, spelling := range collectionSpellings(collection) {
			a.registerTypedCollection(mux, spelling, itemType)
		}
	}
}

// registerTypedCollection mounts one spelling of one typed collection.
func (a *API) registerTypedCollection(mux *http.ServeMux, collection, itemType string) {
	mux.HandleFunc("GET /v1/workspaces/{wid}/"+collection, a.withAuth(a.typedList(itemType)))
	mux.HandleFunc("POST /v1/workspaces/{wid}/"+collection, a.withAuth(a.typedCreate(itemType)))
	mux.HandleFunc("GET /v1/workspaces/{wid}/"+collection+"/{iid}", a.withAuth(a.typedGet(itemType)))
	mux.HandleFunc("PATCH /v1/workspaces/{wid}/"+collection+"/{iid}", a.withAuth(a.updateItem))
	mux.HandleFunc("DELETE /v1/workspaces/{wid}/"+collection+"/{iid}", a.withAuth(a.deleteItem))
	// Definition routes belong on the typed collection too. Fabric
	// documents them there — the Copy job REST article's own samples are
	// `POST …/copyJobs/{copyJobId}/getDefinition` and `…/updateDefinition`
	// — and only the generic `/items/{iid}/` pair existed here, so a client
	// following the per-item-type reference got a 404 on the exact URL that
	// reference prints. Found by writing a witness for the Report row.
	mux.HandleFunc("POST /v1/workspaces/{wid}/"+collection+"/{iid}/getDefinition",
		a.withAuth(a.typedDefinition(itemType, a.getDefinition)))
	mux.HandleFunc("POST /v1/workspaces/{wid}/"+collection+"/{iid}/updateDefinition",
		a.withAuth(a.typedDefinition(itemType, a.updateDefinition)))
	// Job-instance READ and CANCEL are documented on the typed collection
	// too, while SUBMIT is documented on the generic one — Microsoft's Copy
	// job REST article mixes the two within a single page:
	//
	//   POST …/items/{id}/jobs/instances?jobType=Execute      (submit)
	//   GET  …/copyJobs/{id}/jobs/instances/{jid}             (read)
	//   POST …/copyJobs/{id}/jobs/instances/{jid}/cancel      (cancel)
	//
	// So the mixture is the contract, not an inconsistency to normalise.
	// Only the generic forms existed here, which is the same omission as
	// the definition pair above and was found by the same audit.
	mux.HandleFunc("GET /v1/workspaces/{wid}/"+collection+"/{iid}/jobs/instances/{jid}",
		a.withAuth(a.typedDefinition(itemType, a.getJobInstance)))
	mux.HandleFunc("POST /v1/workspaces/{wid}/"+collection+"/{iid}/jobs/instances/{jid}/cancel",
		a.withAuth(a.typedDefinition(itemType, a.cancelJobInstance)))
}

// typedDefinition guards a definition handler with the collection's type, so
// `…/reports/{id}/getDefinition` refuses a notebook id the way `typedGet`
// does. Without the guard a typed URL would happily serve another type's
// definition — a cross-type read through a route whose whole purpose is to
// name one type.
func (a *API) typedDefinition(itemType string, next handler) handler {
	return func(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
		it, err := a.Store.GetItem(r.PathValue("wid"), r.PathValue("iid"))
		if err != nil || it.Type != itemType {
			writeErr(w, http.StatusNotFound, "ItemNotFound", "The item is not available.")
			return
		}
		next(w, r, p)
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
		writeJSON(w, http.StatusOK, a.itemView(r, it))
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
	writePage(a, w, r, fs)
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
	if body.ParentFolderID != "" {
		if _, err := a.Store.GetFolder(wid, body.ParentFolderID); err != nil {
			writeErr(w, http.StatusNotFound, "FolderNotFound", "parent folder not found in this workspace")
			return
		}
	}
	f := &store.Folder{WorkspaceID: wid, DisplayName: body.DisplayName, ParentFolderID: body.ParentFolderID}
	if err := a.Store.CreateFolder(f); err != nil {
		if errors.Is(err, store.ErrNameConflict) {
			writeErr(w, http.StatusConflict, "FolderAlreadyExists", "A folder with this name already exists here.")
			return
		}
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, f)
}
