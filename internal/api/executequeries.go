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
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

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

	model, data, err := a.loadSemanticModel(r.Context(), it.ID, p)
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
			a.publishQuery(it, len(body.Queries), true)
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
	// The Power BI hop: a read, so it is announced on the flow bus and never
	// written to lineage_edges (see modellineage.go).
	a.publishQuery(it, len(body.Queries), false)
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// loadSemanticModel parses the item's model definition + optional data.json.
//
// Two serialisations are accepted, because Fabric itself accepts both: TMSL
// (one `model.bim` JSON document, what the REST API has always taken) and TMDL
// (a folder of `.tmdl` text files, what Power BI Desktop writes and what a
// `.pbip` project on disk contains). TMSL wins when both are present — it is
// the older, fully-covered path, so a definition carrying both is not a place
// to start guessing.
func (a *API) loadSemanticModel(ctx context.Context, itemID string, p *auth.Principal) (*semanticmodel.Model, semanticmodel.Data, error) {
	m, err := a.parseModelDefinition(itemID)
	if err != nil {
		return nil, nil, err
	}
	data := semanticmodel.Data{}
	if raw, err := a.definitionPart(itemID, "data.json"); err == nil {
		if d, err := semanticmodel.ParseData(raw); err == nil {
			data = d
		}
	}
	if err := a.loadDirectLakeData(ctx, m, data, p); err != nil {
		return nil, nil, err
	}
	return m, data, nil
}

// parseModelDefinition reads the item's definition and parses whichever model
// serialisation it carries.
func (a *API) parseModelDefinition(itemID string) (*semanticmodel.Model, error) {
	// Exact: a definition whose sole part is a .tmdl must fall through to the
	// TMDL branch below, not be handed to the TMSL parser (definitionPartExact).
	if bim, err := a.definitionPartExact(itemID, "model.bim"); err == nil {
		return semanticmodel.ParseTMSL(bim)
	}
	parts, err := a.Store.GetDefinition(itemID)
	if err != nil {
		return nil, err
	}
	tmdl := map[string][]byte{}
	for _, part := range parts {
		if !strings.HasSuffix(strings.ToLower(part.Path), ".tmdl") {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(part.Payload)
		if err != nil {
			return nil, fmt.Errorf("decoding %s: %w", part.Path, err)
		}
		tmdl[part.Path] = raw
	}
	if len(tmdl) == 0 {
		return nil, fmt.Errorf("no model.bim and no .tmdl parts in definition")
	}
	return semanticmodel.ParseTMDL(tmdl)
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

// QueryModelUnauthenticated evaluates one DAX query against an IMPORT-mode
// semantic model, for the portal's query runner.
//
// WHY THIS EXISTS BESIDE executeQueries. The REST endpoint takes a Power BI
// audience token, which is right for the wire Power BI uses and wrong for the
// portal: the portal is deliberately unauthenticated, and making its query box
// mint tokens would smuggle a credential flow into a surface whose whole
// premise is "read-only over local state". Running a DAX query against an
// import model IS a read — the rows are already in the item's definition.
//
// Direct Lake models are refused rather than half-served: reading their lake
// data requires a principal (see loadDirectLakeData), and the portal has none
// by design. executeQueries with a real token is the honest route there.
//
// The query is published to the flow bus either way — the graph's Power BI hop
// lights up for a portal query exactly as for a REST one, because the model
// WAS queried and the flow view's job is what actually happened.
func (a *API) QueryModelUnauthenticated(itemID, query string) ([]map[string]any, error) {
	it, err := a.Store.GetItemByID(itemID)
	if err != nil || it.Type != "SemanticModel" {
		return nil, fmt.Errorf("no semantic model with id %q", itemID)
	}
	m, err := a.parseModelDefinition(itemID)
	if err != nil {
		return nil, err
	}
	for _, t := range m.Tables {
		if t.DirectLake != nil {
			return nil, fmt.Errorf(
				"table %q is Direct Lake; the portal runner only queries import "+
					"models — use executeQueries with a Power BI token", t.Name)
		}
	}
	data := semanticmodel.Data{}
	if raw, err := a.definitionPart(itemID, "data.json"); err == nil {
		if d, err := semanticmodel.ParseData(raw); err == nil {
			data = d
		}
	}
	res, err := semanticmodel.Evaluate(m, data, query)
	if err != nil {
		a.publishQuery(it, 1, true)
		return nil, err
	}
	a.publishQuery(it, 1, false)
	return rowsToJSON(res, true), nil
}
