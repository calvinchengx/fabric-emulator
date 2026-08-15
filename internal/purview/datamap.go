// Package purview serves the Microsoft Purview **Data Map** API.
//
// WHAT THIS IS. The Data Map is Apache Atlas v2. That is not an analogy: the
// Azure REST spec's own TypeSpec declares `@route("/atlas/v2/entity")`,
// `/atlas/v2/types`, `/atlas/v2/relationship`, `/atlas/v2/glossary`,
// `/atlas/v2/lineage`, and annotates them "This is Atlas API, which does not
// require api version". The service is mounted at `{endpoint}/datamap/api`
// (spec: Azure.Analytics.Purview.DataMap, api-version 2023-09-01) and takes a
// token for `https://purview.azure.net`.
//
// It matters that these are Atlas routes, because it means the emulator can be
// driven by third-party clients that were never written for it — pyapacheatlas
// and Microsoft's own azure-purview-datamap SDK both speak exactly this. Under
// this repo's rule (docs/24-parity-completion.md: "Every 🟢 needs a real-client
// witness in CI") that is the difference between a row that can go green and
// one that cannot.
//
// WHAT THIS INCREMENT COVERS, stated plainly because the spec has 96 routes and
// this has far fewer: the **type system** and **entities**. Typedef CRUD,
// per-category typedef reads, entity createOrUpdate/read/delete, bulk, and
// lookup by the unique attribute. NOT glossary, lineage, relationships,
// classifications, business metadata, or search — those are later increments,
// and docs/parity.md says so rather than implying whole-API support.
//
// WHAT IS REAL HERE rather than shaped. A type system is not decoration: an
// entity whose typeName is unregistered is refused, a missing required
// attribute is refused, `qualifiedName` is enforced as the per-type identity so
// createOrUpdate genuinely updates, and Atlas's negative-GUID assignment
// protocol is honoured so a client that posts placeholder GUIDs gets a real
// `guidAssignments` map back. Those are the behaviours a client actually
// depends on, and each one is a way a stub would be caught.
package purview

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// DataMapAudience is the Entra resource a Purview Data Map token carries.
// The spec's OAuth2 flow names `https://purview.azure.net/.default`; the
// audience in the issued token is the resource without the scope suffix, and
// both spellings (with and without a trailing slash) are accepted because
// Entra issues either depending on the flow.
var DataMapAudience = []string{
	"https://purview.azure.net",
	"https://purview.azure.net/",
}

// Service is the Data Map endpoint.
type Service struct {
	Store *store.Store
	Auth  *auth.Validator
}

// New builds the service. The validator must carry DataMapAudience — a Data
// Map token is not a Fabric control-plane token, and accepting one would make
// the emulator laxer than the product in the direction that hides bugs.
func New(st *store.Store, v *auth.Validator) *Service {
	return &Service{Store: st, Auth: v}
}

// BasePath is the prefix every Data Map route sits under, from the spec's
// `@server("{endpoint}/datamap/api", …)`.
const BasePath = "/datamap/api"

// Register mounts the Data Map routes on mux.
func (s *Service) Register(mux *http.ServeMux) { s.register(mux, s.withAuth) }

// registerForTest mounts the same routes with authentication bypassed, so unit
// tests exercise Atlas semantics without minting a token per request. The route
// TABLE is shared with Register — a route reachable in tests and unmounted in
// production is the map-vs-route gap, and one list is how it stays impossible.
// Token handling has its own test and the server suite drives it end to end.
func (s *Service) registerForTest(mux *http.ServeMux) {
	s.register(mux, func(h handler) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { h(w, r, nil) }
	})
}

func (s *Service) register(mux *http.ServeMux, wrap func(handler) http.HandlerFunc) {
	h := func(p string, fn handler) {
		method, rest, _ := strings.Cut(p, " ")
		mux.HandleFunc(method+" "+BasePath+rest, wrap(fn))
	}
	// --- type system -------------------------------------------------------
	// --- lineage: DERIVED from Process inputs/outputs, never stored ---------
	h("GET /atlas/v2/lineage/{guid}", s.getLineage)
	h("GET /atlas/v2/types/typedefs", s.listTypeDefs)
	h("POST /atlas/v2/types/typedefs", s.createTypeDefs)
	h("PUT /atlas/v2/types/typedefs", s.updateTypeDefs)
	h("DELETE /atlas/v2/types/typedefs", s.deleteTypeDefs)
	h("GET /atlas/v2/types/typedefs/headers", s.listTypeDefHeaders)
	h("GET /atlas/v2/types/typedef/name/{name}", s.getTypeDefByName)
	h("GET /atlas/v2/types/typedef/guid/{guid}", s.getTypeDefByGUID)
	h("DELETE /atlas/v2/types/typedef/name/{name}", s.deleteTypeDefByName)
	// Per-category reads. Real Atlas serves each category on its own path and
	// 404s when the name resolves to a DIFFERENT category — asking for an
	// entitydef by the name of a classification is a miss, not a cast.
	for _, c := range typeCategoryPaths {
		h("GET /atlas/v2/types/"+c.path+"/name/{name}", s.getTypeDefOfCategoryByName(c.category))
		h("GET /atlas/v2/types/"+c.path+"/guid/{guid}", s.getTypeDefOfCategoryByGUID(c.category))
	}
	// --- entities ----------------------------------------------------------
	h("POST /atlas/v2/entity", s.createOrUpdateEntity)
	h("POST /atlas/v2/entity/bulk", s.createOrUpdateEntities)
	h("GET /atlas/v2/entity/bulk", s.getEntitiesByGUIDs)
	h("DELETE /atlas/v2/entity/bulk", s.deleteEntitiesByGUIDs)
	h("GET /atlas/v2/entity/guid/{guid}", s.getEntityByGUID)
	h("DELETE /atlas/v2/entity/guid/{guid}", s.deleteEntityByGUID)
	h("GET /atlas/v2/entity/guid/{guid}/header", s.getEntityHeaderByGUID)
	h("GET /atlas/v2/entity/uniqueAttribute/type/{typeName}", s.getEntityByUniqueAttr)
	h("DELETE /atlas/v2/entity/uniqueAttribute/type/{typeName}", s.deleteEntityByUniqueAttr)
}

type handler func(http.ResponseWriter, *http.Request, *auth.Principal)

// typeCategoryPaths maps the spec's per-category route segments to the Atlas
// TypeCategory they serve. One list, so a category cannot be routable and
// unresolvable (or the reverse) — the failure that would otherwise 404 a type
// that exists.
var typeCategoryPaths = []struct{ path, category string }{
	{"entitydef", "ENTITY"},
	{"classificationdef", "CLASSIFICATION"},
	{"enumdef", "ENUM"},
	{"structdef", "STRUCT"},
	{"relationshipdef", "RELATIONSHIP"},
	{"businessmetadatadef", "BUSINESS_METADATA"},
}

func (s *Service) withAuth(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" || token == r.Header.Get("Authorization") {
			writeAtlasErr(w, http.StatusUnauthorized, "ATLAS-401-00-001",
				"Authentication required: a bearer token for https://purview.azure.net.")
			return
		}
		p, err := s.Auth.Validate(token)
		if err != nil {
			writeAtlasErr(w, http.StatusUnauthorized, "ATLAS-401-00-001", err.Error())
			return
		}
		h(w, r, p)
	}
}

// atlasError is the documented AtlasErrorResponse: requestId, errorCode,
// errorMessage. Atlas's own error codes are `ATLAS-<status>-00-<n>`, and
// clients match on them, so they are not invented per call site.
type atlasError struct {
	RequestID    string `json:"requestId,omitempty"`
	ErrorCode    string `json:"errorCode,omitempty"`
	ErrorMessage string `json:"errorMessage,omitempty"`
}

func writeAtlasErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(atlasError{RequestID: store.NewID(), ErrorCode: code, ErrorMessage: msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
