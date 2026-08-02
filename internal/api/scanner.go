package api

// The admin metadata scanner — the WorkspaceInfo APIs.
//
//	GET  /v1.0/myorg/admin/workspaces/modified
//	POST /v1.0/myorg/admin/workspaces/getInfo
//	GET  /v1.0/myorg/admin/workspaces/scanStatus/{scanId}
//	GET  /v1.0/myorg/admin/workspaces/scanResult/{scanId}
//
// This is how a catalog crawler learns what a tenant CONTAINS, as opposed to
// querying one thing it already knows about. It is the only surface here that
// returns a model's tables, columns and measures — everything else returns
// either the model definition (which a crawler would have to parse itself) or
// query results (which say nothing about structure).
//
// Four calls because it is asynchronous by design: find what changed, ask for a
// scan, poll it, read it. The emulator completes the scan synchronously — the
// data is already in the store, so there is nothing to wait for — but keeps the
// four-call shape, because a crawler written against the real service polls
// scanStatus and would otherwise never take its own success path.
//
// WHAT IS OPTIONAL IS OPTIONAL. Schema and expressions come back only when
// `datasetSchema=true` / `datasetExpressions=true` are asked for, as in the real
// API. That is not parsimony: a crawler that forgets the flag and receives the
// schema anyway will ship, and then break against real Fabric where it does not.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/semanticmodel"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// scanRequest is the swagger's ScanRequest — what getInfo returns and what
// scanStatus reports.
type scanRequest struct {
	ID              string `json:"id"`
	CreatedDateTime string `json:"createdDateTime"`
	Status          string `json:"status"`
}

// scanColumn / scanMeasure / scanTable mirror the swagger's Column, Measure and
// Table under DatasetSchemaProperties.
type scanColumn struct {
	Name     string `json:"name"`
	DataType string `json:"dataType,omitempty"`
}

type scanMeasure struct {
	Name       string `json:"name"`
	Expression string `json:"expression,omitempty"`
}

type scanTable struct {
	Name     string        `json:"name"`
	Columns  []scanColumn  `json:"columns,omitempty"`
	Measures []scanMeasure `json:"measures,omitempty"`
}

type scanExpression struct {
	Name       string `json:"name"`
	Expression string `json:"expression,omitempty"`
}

// scanDataset is WorkspaceInfoDataset restricted to what this emulator knows.
type scanDataset struct {
	ID           string           `json:"id"`
	Name         string           `json:"name"`
	Description  string           `json:"description,omitempty"`
	ConfiguredBy string           `json:"configuredBy,omitempty"`
	Tables       []scanTable      `json:"tables,omitempty"`
	Expressions  []scanExpression `json:"expressions,omitempty"`
	// Populated only when the model could not be parsed. The swagger has this
	// field precisely so a scan can report a broken model without failing the
	// whole scan — one unreadable dataset must not cost a crawler the tenant.
	SchemaRetrievalError string `json:"schemaRetrievalError,omitempty"`
}

type scanWorkspace struct {
	ID       string        `json:"id"`
	Name     string        `json:"name"`
	Type     string        `json:"type"`
	State    string        `json:"state"`
	Datasets []scanDataset `json:"datasets"`
}

// scanResult is the swagger's WorkspaceInfoResponse.
type scanResult struct {
	Workspaces          []scanWorkspace `json:"workspaces"`
	DatasourceInstances []datasource    `json:"datasourceInstances"`
}

// scanStore holds completed scans. In memory, like refresh history and for the
// same reason: a scan is a transient result about this process, and nothing
// reads it back after a restart.
type scanStore struct {
	mu sync.Mutex
	by map[string]*scanRecord
}

type scanRecord struct {
	req    scanRequest
	result scanResult
	// owner is the principal that asked. A scan can name every dataset in a
	// workspace, so reading someone else's by guessing an id would be a
	// disclosure — the id is the only thing protecting it.
	owner string
}

func (s *scanStore) put(r *scanRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.by == nil {
		s.by = map[string]*scanRecord{}
	}
	s.by[r.req.ID] = r
}

func (s *scanStore) get(id string) (*scanRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.by[id]
	return r, ok
}

func (a *API) registerScanner(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1.0/myorg/admin/workspaces/modified", a.withAuth(a.listModifiedWorkspaces))
	mux.HandleFunc("POST /v1.0/myorg/admin/workspaces/getInfo", a.withAuth(a.postScan))
	mux.HandleFunc("GET /v1.0/myorg/admin/workspaces/scanStatus/{scanId}", a.withAuth(a.getScanStatus))
	mux.HandleFunc("GET /v1.0/myorg/admin/workspaces/scanResult/{scanId}", a.withAuth(a.getScanResult))
}

// listModifiedWorkspaces is step one of the crawl: which workspaces are worth
// scanning. The real API filters on modifiedSince; this returns every workspace
// the caller can see, and says so rather than pretending to filter — accepting
// modifiedSince and ignoring it would let a crawler believe it had done an
// incremental pass when it had done a full one.
func (a *API) listModifiedWorkspaces(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	if raw := r.URL.Query().Get("modifiedSince"); raw != "" {
		writeErr(w, http.StatusBadRequest, "ModifiedSinceNotSupported",
			"This emulator does not track workspace modification times, so it "+
				"cannot honour modifiedSince. Omit it and scan the full list; "+
				"ignoring it would report a full pass as an incremental one.")
		return
	}
	out := []map[string]string{}
	for _, ws := range a.visibleWorkspaces(p) {
		out = append(out, map[string]string{"id": ws.ID})
	}
	writeJSON(w, http.StatusOK, out)
}

// visibleWorkspaces is every workspace the principal holds any role on. The
// admin scanner is tenant-wide in real Power BI; scoping to the caller's own
// workspaces is the emulator being narrower than the real thing, which is the
// safe direction — it can never disclose more than the caller could already
// read through the item APIs.
func (a *API) visibleWorkspaces(p *auth.Principal) []*store.Workspace {
	out, err := a.Store.ListWorkspacesFor(p.ID)
	if err != nil {
		return nil
	}
	return out
}

func (a *API) postScan(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var body struct {
		Workspaces []string `json:"workspaces"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Workspaces) == 0 {
		writeErr(w, http.StatusBadRequest, "InvalidRequest",
			"A body naming at least one workspace is required: {\"workspaces\": [\"<id>\"]}.")
		return
	}
	q := r.URL.Query()
	wantSchema := q.Get("datasetSchema") == "true"
	wantExpressions := q.Get("datasetExpressions") == "true"
	wantDatasources := q.Get("datasourceDetails") == "true"

	res := scanResult{Workspaces: []scanWorkspace{}, DatasourceInstances: []datasource{}}
	seenSource := map[string]bool{}
	for _, wid := range body.Workspaces {
		ws, err := a.Store.GetWorkspace(wid)
		if err != nil {
			writeErr(w, http.StatusNotFound, "WorkspaceNotFound",
				fmt.Sprintf("workspace %q was not found", wid))
			return
		}
		// Every named workspace must be readable. Failing the whole scan beats
		// silently returning fewer workspaces than were asked for: a crawler
		// cannot tell a skipped workspace from an empty one.
		if role, err := a.Store.RoleOf(ws.ID, p.ID); err != nil || role == "" {
			writeErr(w, http.StatusForbidden, "InsufficientPermissions",
				fmt.Sprintf("no role on workspace %q", wid))
			return
		}
		sw := scanWorkspace{ID: ws.ID, Name: ws.DisplayName, Type: "Workspace",
			State: "Active", Datasets: []scanDataset{}}

		items, _ := a.Store.ListItems(ws.ID, "SemanticModel")
		for _, it := range items {
			ds := scanDataset{ID: it.ID, Name: it.DisplayName,
				Description: it.Description, ConfiguredBy: p.ID}
			m, err := a.parseModelDefinition(it.ID)
			if err != nil {
				// The swagger has schemaRetrievalError for exactly this: report
				// the dataset and say its schema could not be read, rather than
				// dropping it and shrinking the tenant.
				ds.SchemaRetrievalError = err.Error()
				sw.Datasets = append(sw.Datasets, ds)
				continue
			}
			if wantSchema {
				ds.Tables = scanTables(m)
			}
			if wantExpressions {
				for name, expr := range m.Expressions {
					ds.Expressions = append(ds.Expressions, scanExpression{Name: name, Expression: expr})
				}
				sortExpressions(ds.Expressions)
			}
			if wantDatasources {
				sources, err := a.modelDatasources(m)
				if err != nil {
					// Report it against the dataset rather than dropping the
					// lineage quietly. A crawler that sees fewer sources than
					// exist has no way to know it was shortchanged.
					ds.SchemaRetrievalError = "datasource lineage unavailable: " + err.Error()
				}
				for _, d := range sources {
					if !seenSource[d.ConnectionDetails.URL] {
						seenSource[d.ConnectionDetails.URL] = true
						res.DatasourceInstances = append(res.DatasourceInstances, d)
					}
				}
			}
			sw.Datasets = append(sw.Datasets, ds)
		}
		res.Workspaces = append(res.Workspaces, sw)
	}

	rec := &scanRecord{
		req: scanRequest{
			ID:              store.NewID(),
			CreatedDateTime: time.Unix(a.Store.Now(), 0).UTC().Format(time.RFC3339),
			// Succeeded immediately: everything scanned is already in the
			// store, so there is nothing to wait for. The four-call shape is
			// kept because a crawler written against the real service polls
			// scanStatus and must reach its own success path.
			Status: "Succeeded",
		},
		result: res,
		owner:  p.ID,
	}
	a.scans.put(rec)
	writeJSON(w, http.StatusAccepted, rec.req)
}

func (a *API) scanFor(w http.ResponseWriter, r *http.Request, p *auth.Principal) (*scanRecord, bool) {
	rec, ok := a.scans.get(r.PathValue("scanId"))
	if !ok {
		writeErr(w, http.StatusNotFound, "ScanNotFound", "No scan with that id.")
		return nil, false
	}
	if rec.owner != p.ID {
		// A scan names every dataset in the workspaces it covered, so its id is
		// the only thing protecting it. 404, not 403: confirming that an id
		// exists is itself a disclosure.
		writeErr(w, http.StatusNotFound, "ScanNotFound", "No scan with that id.")
		return nil, false
	}
	return rec, true
}

func (a *API) getScanStatus(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	if rec, ok := a.scanFor(w, r, p); ok {
		writeJSON(w, http.StatusOK, rec.req)
	}
}

func (a *API) getScanResult(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	if rec, ok := a.scanFor(w, r, p); ok {
		writeJSON(w, http.StatusOK, rec.result)
	}
}

// scanTables projects the parsed model onto the swagger's Table/Column/Measure.
func scanTables(m *semanticmodel.Model) []scanTable {
	out := make([]scanTable, 0, len(m.Tables))
	for _, t := range m.Tables {
		st := scanTable{Name: t.Name}
		for _, c := range t.Columns {
			st.Columns = append(st.Columns, scanColumn{Name: c.Name, DataType: c.DataType})
		}
		for _, ms := range t.Measures {
			st.Measures = append(st.Measures, scanMeasure{Name: ms.Name, Expression: ms.Expression})
		}
		out = append(out, st)
	}
	return out
}

// sortExpressions gives the map-backed expressions a stable order, so two scans
// of an unchanged tenant produce identical bytes. A crawler diffing scans would
// otherwise see phantom changes from Go's map iteration.
func sortExpressions(e []scanExpression) {
	for i := 1; i < len(e); i++ {
		for j := i; j > 0 && e[j].Name < e[j-1].Name; j-- {
			e[j], e[j-1] = e[j-1], e[j]
		}
	}
}
