package api

// The Power BI `datasets` discovery surface — how a Power BI REST client FINDS
// a semantic model, as opposed to querying one it already knows the id of.
//
//	GET /v1.0/myorg/groups/{groupId}/datasets
//	GET /v1.0/myorg/groups/{groupId}/datasets/{datasetId}
//	GET /v1.0/myorg/datasets/{datasetId}
//
// This existed as a gap rather than a decision. executeQueries was mounted
// first because it is the interesting surface, but it takes a datasetId the
// caller must already have — and the only way to obtain one was to watch the
// Fabric item API's long-running operation. So a client that started where real
// clients start, by listing the workspace's datasets, got a 404 from the mux
// and no indication that the model it wanted was sitting right there.
//
// Shapes follow the vendored golden swagger (third_party/powerbi-rest-swagger):
// a `Datasets` OData wrapper — {"@odata.context", "value": [...]} — over
// `Dataset`, whose only required property is `id`. The subset returned is the
// swagger's own caveat: "The API returns a subset of the following list of
// dataset properties."

import (
	"fmt"
	"net/http"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// registerDatasets mounts the dataset discovery paths on the Power BI prefix.
func (a *API) registerDatasets(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1.0/myorg/groups/{groupId}/datasets",
		a.withPBIAuth(a.listDatasetsInGroup))
	mux.HandleFunc("GET /v1.0/myorg/groups/{groupId}/datasets/{datasetId}",
		a.withPBIAuth(a.getDataset))
	mux.HandleFunc("GET /v1.0/myorg/datasets/{datasetId}",
		a.withPBIAuth(a.getDataset))
	// The My-workspace LIST deliberately answers with an error rather than an
	// empty array. This emulator models workspaces and has no personal
	// workspace, so there is no set of datasets for this route to describe —
	// and `{"value": []}` would be a well-formed lie, indistinguishable from a
	// personal workspace that happens to be empty. Naming the reason costs one
	// handler and saves someone believing their model failed to publish.
	mux.HandleFunc("GET /v1.0/myorg/datasets", a.withPBIAuth(a.listDatasetsMyWorkspace))
}

// dataset is the swagger's Dataset, restricted to what this emulator can answer
// truthfully. Fields it cannot source are omitted rather than defaulted: the
// swagger explicitly allows a subset, so an absent field means "not provided"
// while a zero value would assert something false.
type dataset struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Description  string `json:"description,omitempty"`
	ConfiguredBy string `json:"configuredBy,omitempty"`
	// Computed per model, not hardcoded: true when the model names a source
	// this emulator can go and read again (Direct Lake today), false when its
	// rows are an inline `data.json` snapshot with nothing behind them.
	//
	// The SAME predicate gates POST /refreshes, so a client that trusts this
	// flag is never then refused — see modelHasReadableSource in refreshes.go.
	// Stated rather than omitted, because absent and false mean different
	// things to a client deciding whether to offer a refresh button.
	IsRefreshable bool `json:"isRefreshable"`
	// Likewise definite: the push-rows API is not implemented.
	AddRowsAPIEnabled bool `json:"addRowsAPIEnabled"`
}

func (a *API) toDataset(it *store.Item, configuredBy string) dataset {
	_, refreshable := a.refreshableModel(it.ID)
	return dataset{
		ID:            it.ID,
		Name:          it.DisplayName,
		Description:   it.Description,
		ConfiguredBy:  configuredBy,
		IsRefreshable: refreshable,
		// Definite: the push-rows API is not implemented.
		AddRowsAPIEnabled: false,
	}
}

// listDatasetsInGroup returns the workspace's semantic models.
//
// SemanticModel is the only item type that maps to a Power BI dataset. A
// lakehouse or warehouse also has a default semantic model in real Fabric;
// this does not synthesise one, because a client would then get an id that
// executeQueries cannot answer for — a dataset that lists but cannot be
// queried is worse than one that does not list.
func (a *API) listDatasetsInGroup(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("groupId")
	if _, _, ok := a.requireRole(w, wid, p, store.RoleViewer); !ok {
		return
	}
	items, err := a.Store.ListItems(wid, "SemanticModel")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	out := make([]dataset, 0, len(items))
	for _, it := range items {
		out = append(out, a.toDataset(it, p.ID))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"@odata.context": odataContext(r, fmt.Sprintf("groups/%s/datasets", wid)),
		"value":          out,
	})
}

// getDataset returns one dataset, by id, with or without the group segment.
// The group form must agree with the item's workspace — the same rule
// executeQueries applies, so a client cannot discover an id in one workspace
// and then read it through another.
func (a *API) getDataset(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	it, err := a.Store.GetItemByID(r.PathValue("datasetId"))
	if err != nil || it.Type != "SemanticModel" {
		writeErr(w, http.StatusNotFound, "DatasetNotFound", "The dataset was not found.")
		return
	}
	if g := r.PathValue("groupId"); g != "" && g != it.WorkspaceID {
		writeErr(w, http.StatusNotFound, "DatasetNotFound", "The dataset is not in this workspace.")
		return
	}
	if _, _, ok := a.requireRole(w, it.WorkspaceID, p, store.RoleViewer); !ok {
		return
	}
	writeJSON(w, http.StatusOK, a.toDataset(it, p.ID))
}

// listDatasetsMyWorkspace refuses rather than returning an empty list. See the
// note on the route registration.
func (a *API) listDatasetsMyWorkspace(w http.ResponseWriter, r *http.Request, _ *auth.Principal) {
	writeErr(w, http.StatusNotFound, "PersonalWorkspaceNotSupported",
		"This emulator has no personal workspace. List datasets in a workspace "+
			"instead: GET /v1.0/myorg/groups/{groupId}/datasets.")
}

// odataContext builds the @odata.context the swagger's list wrappers carry.
// Real Power BI returns an absolute URL rooted at the service; mirroring that
// off the request keeps it correct behind the emulator's own scheme and host.
func odataContext(r *http.Request, rel string) string {
	scheme := "https"
	if r.TLS == nil {
		scheme = "http"
	}
	return fmt.Sprintf("%s://%s/v1.0/myorg/$metadata#%s", scheme, r.Host, rel)
}
