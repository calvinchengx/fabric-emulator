package api

// The Power BI dataset refresh surface.
//
//	POST /v1.0/myorg/groups/{groupId}/datasets/{datasetId}/refreshes
//	GET  /v1.0/myorg/groups/{groupId}/datasets/{datasetId}/refreshes
//	GET  /v1.0/myorg/groups/{groupId}/datasets/{datasetId}/refreshes/{refreshId}
//
// WHAT A REFRESH MEANS HERE, because it is not the same for both model kinds
// and answering uniformly would be the interesting kind of wrong.
//
// A DIRECT LAKE model reads its Delta at QUERY time — loadDirectLakeData runs
// inside executeQueries, not at publish. It is therefore already current before
// any refresh is asked for, and TestExecuteQueriesDirectLakeReadsCurrentDelta
// is the witness: change the Delta, query again, get the new numbers, with no
// refresh call anywhere. So a refresh of such a model genuinely has nothing to
// reload, and reporting Completed is the truth rather than a shortcut. Real
// Fabric does reframe Direct Lake models; reading per query makes framing
// implicit here, which is a documented divergence in the executor, not in the
// observable answer.
//
// An IMPORT model in this emulator carries its rows as a `data.json` definition
// part — a snapshot the publisher embedded. There is no datasource to re-read,
// so a refresh CANNOT do anything, and this refuses instead of returning a
// Completed it did not earn. A caller polling until Completed and then trusting
// stale numbers is exactly the failure the endpoint would otherwise invent.
//
// Shapes follow the vendored golden swagger: the `Refreshes` OData wrapper over
// `Refresh` (refreshType, startTime, endTime, status, requestId,
// serviceExceptionJson).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/semanticmodel"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// refresh is the swagger's Refresh object.
type refresh struct {
	RequestID            string `json:"requestId"`
	RefreshType          string `json:"refreshType"`
	StartTime            string `json:"startTime"`
	EndTime              string `json:"endTime,omitempty"`
	Status               string `json:"status"`
	ServiceExceptionJSON string `json:"serviceExceptionJson,omitempty"`
}

// refreshLog is per-dataset refresh history.
//
// In memory, not in the store, and deliberately: history is operational
// telemetry about this process, not modelled state. An emulator restarted mid
// tutorial should not claim refreshes that happened to a previous process, and
// nothing downstream reads it back after a restart.
type refreshLog struct {
	mu sync.Mutex
	by map[string][]refresh // datasetId -> newest last
}

func (l *refreshLog) append(datasetID string, r refresh) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.by == nil {
		l.by = map[string][]refresh{}
	}
	l.by[datasetID] = append(l.by[datasetID], r)
}

// history returns newest-first, which is the order the real API documents.
func (l *refreshLog) history(datasetID string) []refresh {
	l.mu.Lock()
	defer l.mu.Unlock()
	src := l.by[datasetID]
	out := make([]refresh, 0, len(src))
	for i := len(src) - 1; i >= 0; i-- {
		out = append(out, src[i])
	}
	return out
}

func (l *refreshLog) get(datasetID, requestID string) (refresh, bool) {
	for _, r := range l.history(datasetID) {
		if r.RequestID == requestID {
			return r, true
		}
	}
	return refresh{}, false
}

func (a *API) registerRefreshes(mux *http.ServeMux) {
	for _, prefix := range []string{
		"/v1.0/myorg/datasets/{datasetId}",
		"/v1.0/myorg/groups/{groupId}/datasets/{datasetId}",
	} {
		mux.HandleFunc("POST "+prefix+"/refreshes", a.withPBIAuth(a.postRefresh))
		mux.HandleFunc("GET "+prefix+"/refreshes", a.withPBIAuth(a.listRefreshes))
		mux.HandleFunc("GET "+prefix+"/refreshes/{refreshId}", a.withPBIAuth(a.getRefresh))
	}
}

// refreshableModel resolves the dataset and reports whether its data can
// actually be re-read. The bool is the honest answer to `isRefreshable`, and
// the same predicate decides whether POST /refreshes is accepted — one rule, so
// a client that trusts the flag is not then refused.
func (a *API) refreshableModel(itemID string) (*semanticmodel.Model, bool) {
	m, err := a.parseModelDefinition(itemID)
	if err != nil {
		return nil, false
	}
	return m, modelHasReadableSource(m)
}

// modelHasReadableSource is true when at least one table names a source this
// emulator can go and read again. Today that means Direct Lake: an import table
// carries its rows inline and has nowhere to refresh from.
func modelHasReadableSource(m *semanticmodel.Model) bool {
	if m == nil {
		return false
	}
	for _, t := range m.Tables {
		if t.DirectLake != nil {
			return true
		}
	}
	return false
}

// resolveDataset is the shared lookup: the item must be a semantic model, the
// group (if given) must be its workspace, and the caller needs Viewer.
func (a *API) resolveDataset(w http.ResponseWriter, r *http.Request, p *auth.Principal, role string) (*store.Item, bool) {
	it, err := a.Store.GetItemByID(r.PathValue("datasetId"))
	if err != nil || it.Type != "SemanticModel" {
		writeErr(w, http.StatusNotFound, "DatasetNotFound", "The dataset was not found.")
		return nil, false
	}
	if g := r.PathValue("groupId"); g != "" && g != it.WorkspaceID {
		writeErr(w, http.StatusNotFound, "DatasetNotFound", "The dataset is not in this workspace.")
		return nil, false
	}
	if _, _, ok := a.requireRole(w, it.WorkspaceID, p, role); !ok {
		return nil, false
	}
	return it, true
}

// postRefresh triggers a refresh. 202 with a RequestId header on success, which
// is the contract a polling client depends on.
func (a *API) postRefresh(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	// Refreshing writes, so Contributor — reading history does not.
	it, ok := a.resolveDataset(w, r, p, store.RoleContributor)
	if !ok {
		return
	}
	if _, refreshable := a.refreshableModel(it.ID); !refreshable {
		// Refusing is the point. This model's rows are a definition part with
		// no datasource behind them, so a Completed here would tell a caller
		// their numbers had been brought up to date when nothing was re-read.
		writeErr(w, http.StatusBadRequest, "DatasetNotRefreshable",
			"This dataset carries its rows as a `data.json` definition part and "+
				"names no datasource to re-read, so a refresh would do nothing. "+
				"Publish the model with Direct Lake partitions to make it refreshable.")
		return
	}

	// notifyOption is the only field the basic RefreshRequest carries; an
	// enhanced refresh sends more. Accept and ignore rather than reject: the
	// body is advisory here, and no notification is sent either way.
	var body struct {
		NotifyOption string `json:"notifyOption"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	// Store.Now is the emulator's controllable clock (virtual seconds), so a
	// test that advances time sees it here rather than wall time.
	now := time.Unix(a.Store.Now(), 0).UTC()
	rec := refresh{
		RequestID: store.NewID(),
		// "ViaApi" is what the real service records for a REST-triggered
		// refresh, as opposed to Scheduled or ViaXmlaEndpoint.
		RefreshType: "ViaApi",
		StartTime:   now.Format(time.RFC3339),
		// Completed, and immediately: a Direct Lake model reads its Delta at
		// query time, so there is no reload to wait for. Manufacturing an
		// InProgress phase would make every client poll for a state change that
		// exists only to look plausible.
		EndTime: now.Format(time.RFC3339),
		Status:  "Completed",
	}
	a.refreshes.append(it.ID, rec)

	w.Header().Set("RequestId", rec.RequestID)
	w.WriteHeader(http.StatusAccepted)
}

func (a *API) listRefreshes(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	it, ok := a.resolveDataset(w, r, p, store.RoleViewer)
	if !ok {
		return
	}
	hist := a.refreshes.history(it.ID)
	// $top is the swagger's paging control on this endpoint.
	if raw := r.URL.Query().Get("$top"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, "InvalidRequest", "$top must be a non-negative integer.")
			return
		}
		if n < len(hist) {
			hist = hist[:n]
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"@odata.context": odataContext(r, fmt.Sprintf("datasets/%s/refreshes", it.ID)),
		"value":          hist,
	})
}

func (a *API) getRefresh(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	it, ok := a.resolveDataset(w, r, p, store.RoleViewer)
	if !ok {
		return
	}
	rec, found := a.refreshes.get(it.ID, r.PathValue("refreshId"))
	if !found {
		writeErr(w, http.StatusNotFound, "RefreshNotFound",
			"No refresh with that request id was found for this dataset.")
		return
	}
	writeJSON(w, http.StatusOK, rec)
}
