package api

// The Power BI `executeQueries` DAX endpoint — the real query surface over a
// semantic model, conforming to the vendored golden OpenAPI
// (third_party/powerbi-rest-swagger). It is the tractable alternative to XMLA
// (which needs a native ADOMD.NET client): plain HTTP+JSON, so a real REST
// client works and the swagger is a live golden reference.
//
//   POST /v1.0/myorg/datasets/{datasetId}/executeQueries
//   POST /v1.0/myorg/groups/{groupId}/datasets/{datasetId}/executeQueries
//
// datasetId = a SemanticModel item; groupId = its workspace. The model.bim +
// data.json definition parts feed the bounded DAX evaluator (internal/semanticmodel).

import (
	"encoding/json"
	"net/http"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/semanticmodel"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// PowerBIAudience is the Entra resource a Power BI REST token carries.
var PowerBIAudience = []string{
	"https://analysis.windows.net/powerbi/api",
	"https://analysis.windows.net/powerbi/api/",
}

// registerExecuteQueries mounts both path variants on the Power BI REST prefix.
func (a *API) registerExecuteQueries(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1.0/myorg/datasets/{datasetId}/executeQueries",
		a.withPBIAuth(a.executeQueries))
	mux.HandleFunc("POST /v1.0/myorg/groups/{groupId}/datasets/{datasetId}/executeQueries",
		a.withPBIAuth(a.executeQueries))
}

// withPBIAuth validates a Power BI-audience bearer token (nil validator → 501).
func (a *API) withPBIAuth(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.PBIAuth == nil {
			writeErr(w, http.StatusNotImplemented, "PowerBINotConfigured",
				"The Power BI query endpoint is not configured.")
			return
		}
		p, err := a.PBIAuth.ValidateRequest(r)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer authorization_uri="`+a.PBIAuth.Issuer+`"`)
			writeErr(w, http.StatusUnauthorized, "TokenInvalid", err.Error())
			return
		}
		h(w, r, p)
	}
}

// executeQueries resolves the semantic model, evaluates each DAX query, and
// returns the executeQueries response shape.
func (a *API) executeQueries(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	dsID := r.PathValue("datasetId")
	it, err := a.Store.GetItemByID(dsID)
	if err != nil || it.Type != "SemanticModel" {
		writeErr(w, http.StatusNotFound, "DatasetNotFound", "The dataset was not found.")
		return
	}
	// groupId, if given, must be the item's workspace; either way authorize the
	// caller as at least Viewer there (querying is a read).
	wid := it.WorkspaceID
	if g := r.PathValue("groupId"); g != "" && g != wid {
		writeErr(w, http.StatusNotFound, "DatasetNotFound", "The dataset is not in this workspace.")
		return
	}
	if _, _, ok := a.requireRole(w, wid, p, store.RoleViewer); !ok {
		return
	}

	model, data, err := a.loadSemanticModel(it.ID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "InvalidDataset", err.Error())
		return
	}

	var body struct {
		Queries []struct {
			Query string `json:"query"`
		} `json:"queries"`
		SerializerSettings struct {
			IncludeNulls bool `json:"includeNulls"`
		} `json:"serializerSettings"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Queries) == 0 {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "queries is required.")
		return
	}

	results := make([]map[string]any, 0, len(body.Queries))
	for _, q := range body.Queries {
		res, err := semanticmodel.Evaluate(model, data, q.Query)
		if err != nil {
			// A bad DAX query is a client error, per the Power BI contract.
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": map[string]string{"code": "DAXQueryError", "message": err.Error()},
			})
			return
		}
		results = append(results, map[string]any{
			"tables": []map[string]any{{"rows": rowsToJSON(res, body.SerializerSettings.IncludeNulls)}},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// loadSemanticModel parses the item's model.bim + optional data.json parts.
func (a *API) loadSemanticModel(itemID string) (*semanticmodel.Model, semanticmodel.Data, error) {
	bim, err := a.definitionPart(itemID, "model.bim")
	if err != nil {
		return nil, nil, err
	}
	m, err := semanticmodel.ParseTMSL(bim)
	if err != nil {
		return nil, nil, err
	}
	data := semanticmodel.Data{}
	if raw, err := a.definitionPart(itemID, "data.json"); err == nil {
		if d, err := semanticmodel.ParseData(raw); err == nil {
			data = d
		}
	}
	return m, data, nil
}

// rowsToJSON renders result rows, dropping null (blank) cells unless includeNulls.
func rowsToJSON(res *semanticmodel.Result, includeNulls bool) []map[string]any {
	out := make([]map[string]any, 0, len(res.Rows))
	for _, r := range res.Rows {
		row := map[string]any{}
		for _, c := range res.Columns {
			v := r[c]
			if v == nil && !includeNulls {
				continue
			}
			row[c] = v
		}
		out = append(out, row)
	}
	return out
}
