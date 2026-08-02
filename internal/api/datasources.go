package api

// The Power BI dataset datasources surface.
//
//	GET /v1.0/myorg/datasets/{datasetId}/datasources
//	GET /v1.0/myorg/groups/{groupId}/datasets/{datasetId}/datasources
//
// This turns lineage from something a publishing script knows into something a
// client can ASK. Until now, "this semantic model reads that lakehouse" lived
// only inside the model's TMSL expression and the emulator's own resolver; a
// governance tool, a catalog crawler, or anyone auditing where a number came
// from had no way to obtain it.
//
// What is reported is exactly what the model DECLARES and this emulator can
// resolve — Direct Lake partitions, whose shared expression names a OneLake
// workspace and lakehouse. A model carrying its rows as an inline `data.json`
// part reports NO datasources, and that empty list is the truth rather than a
// gap: such a model genuinely reads nothing. (Contrast the My-workspace list in
// datasets.go, which refuses instead of returning empty, because there the
// empty answer would be indistinguishable from an empty personal workspace.)

import (
	"fmt"
	"net/http"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/semanticmodel"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// datasourceConnectionDetails is the swagger's DatasourceConnectionDetails,
// restricted to the fields a OneLake source can fill honestly.
type datasourceConnectionDetails struct {
	URL  string `json:"url,omitempty"`
	Path string `json:"path,omitempty"`
}

// datasource is the swagger's Datasource. gatewayId and datasourceId are
// omitted rather than blank: the swagger says they are "empty when not bound to
// a gateway", and nothing here is gateway-bound, so absent says that without
// inventing an identifier a caller might try to use.
type datasource struct {
	// "OneLake" is descriptive, not an enum value — the swagger types this as a
	// free string. Real Fabric reports Direct Lake sources by the transport it
	// actually uses; naming the storage the model names keeps the answer
	// checkable against the TMSL rather than guessing at a service-internal
	// vocabulary we cannot verify.
	DatasourceType    string                      `json:"datasourceType"`
	ConnectionDetails datasourceConnectionDetails `json:"connectionDetails"`
}

func (a *API) registerDatasources(mux *http.ServeMux) {
	for _, prefix := range []string{
		"/v1.0/myorg/datasets/{datasetId}",
		"/v1.0/myorg/groups/{groupId}/datasets/{datasetId}",
	} {
		mux.HandleFunc("GET "+prefix+"/datasources", a.withPBIAuth(a.listDatasources))
	}
}

func (a *API) listDatasources(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	it, ok := a.resolveDataset(w, r, p, store.RoleViewer)
	if !ok {
		return
	}
	m, err := a.parseModelDefinition(it.ID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "InvalidDataset", err.Error())
		return
	}
	out, err := a.modelDatasources(m)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "InvalidDataset", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"@odata.context": odataContext(r, fmt.Sprintf("datasets/%s/datasources", it.ID)),
		"value":          out,
	})
}

// modelDatasources resolves a parsed model's declared sources. Shared with the
// admin scanner, which needs the same answer for its datasourceInstances.
//
// Returns what it resolved AND the error, so the two callers can differ: the
// datasources endpoint refuses a broken model outright, while a scan reports
// the problem against that dataset and carries on — one unreadable model must
// not cost a crawler the whole tenant.
func (a *API) modelDatasources(m *semanticmodel.Model) ([]datasource, error) {
	out := []datasource{}
	seen := map[string]bool{}
	for _, t := range m.Tables {
		if t.DirectLake == nil {
			continue
		}
		expr, found := m.Expressions[t.DirectLake.ExpressionSource]
		if !found {
			// A table pointing at an expression that is not there is a broken
			// model. Saying so beats omitting the row and reporting a shorter
			// list as though the model were smaller.
			return out, fmt.Errorf("table %q references missing expression %q",
				t.Name, t.DirectLake.ExpressionSource)
		}
		wsRef, lakeRef, err := parseDirectLakeLocation(expr)
		if err != nil {
			return out, fmt.Errorf("table %q: %w", t.Name, err)
		}
		// Resolve to ids where possible so the answer is stable under rename,
		// falling back to what the model said when it names something that no
		// longer exists — reporting the declared source is more useful than
		// dropping it, and the caller can see it does not resolve.
		wsID, lakeID := wsRef, lakeRef
		if ws, err := a.resolveDirectLakeWorkspace(wsRef); err == nil {
			wsID = ws.ID
			if lake, err := a.resolveDirectLakeLakehouse(ws.ID, lakeRef); err == nil {
				lakeID = lake.ID
			}
		}
		url := fmt.Sprintf("https://onelake.dfs.fabric.microsoft.com/%s/%s", wsID, lakeID)
		if seen[url] {
			// Several tables normally share one expression; the DATASOURCE is
			// the lakehouse, not the table, so it appears once.
			continue
		}
		seen[url] = true
		out = append(out, datasource{
			DatasourceType: "OneLake",
			ConnectionDetails: datasourceConnectionDetails{
				URL:  url,
				Path: fmt.Sprintf("%s/%s", wsID, lakeID),
			},
		})
	}
	return out, nil
}
