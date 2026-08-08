package purview

// What these tests are for.
//
// A type system that accepts everything is indistinguishable from no type
// system, and it would pass any test that only asserts the happy path. So the
// assertions that carry weight here are REFUSALS: an unregistered type, a
// missing required attribute, a supertype that does not exist, a type deleted
// while instances reference it. Each is a thing a stub does not do.
//
// The second theme is IDENTITY. `qualifiedName` is Atlas's unique attribute,
// which is why the endpoint is createOrUpdate; and negative GUIDs are
// placeholders a client expects to be replaced. Both are invisible on a single
// happy-path create and obvious the moment you post twice.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/clock"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// newService returns a seeded Data Map with authentication stubbed out — these
// tests are about Atlas semantics, and the token path is exercised separately
// (TestEveryRouteRequiresAToken) and end-to-end by the server tests.
func newService(t *testing.T) (*Service, *http.ServeMux) {
	t.Helper()
	st, err := store.Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := New(st, nil)
	if err := s.Seed(); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	mux := http.NewServeMux()
	// Register without the auth wrapper: s.Auth is nil here, and a nil
	// validator would panic before any assertion ran.
	s.registerForTest(mux)
	return s, mux
}

func do(t *testing.T, mux *http.ServeMux, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, bytes.NewReader([]byte(body)))
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func errorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var e atlasError
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatalf("response is not an AtlasErrorResponse: %s", w.Body)
	}
	return e.ErrorCode
}

// --- the seeded hierarchy ----------------------------------------------------

// A real Purview account is never empty. A client whose type extends DataSet —
// which the SDK samples and pyapacheatlas's quickstart both do — fails outright
// against an empty registry, so seeding is fidelity rather than convenience.
func TestTheBaseTypeHierarchyIsSeeded(t *testing.T) {
	_, mux := newService(t)
	for _, name := range []string{"Referenceable", "Asset", "DataSet", "Process"} {
		w := do(t, mux, "GET", BasePath+"/atlas/v2/types/typedef/name/"+name, "")
		if w.Code != http.StatusOK {
			t.Fatalf("%s = %d, want 200; a client extending it cannot register its own type", name, w.Code)
		}
	}
	// And the inheritance is real, not decorative: DataSet must reach
	// Referenceable, or qualifiedName is not required where it matters.
	w := do(t, mux, "GET", BasePath+"/atlas/v2/types/entitydef/name/DataSet", "")
	var def typeDefBody
	if err := json.Unmarshal(w.Body.Bytes(), &def); err != nil {
		t.Fatal(err)
	}
	if len(def.SuperTypes) == 0 || def.SuperTypes[0] != "Asset" {
		t.Fatalf("DataSet superTypes = %v, want [Asset]", def.SuperTypes)
	}
}

// --- the type system refuses ------------------------------------------------

func TestAnEntityOfAnUnregisteredTypeIsRefused(t *testing.T) {
	_, mux := newService(t)
	body := `{"entity":{"typeName":"nonesuch_table","attributes":{"qualifiedName":"x://a","name":"a"}}}`
	w := do(t, mux, "POST", BasePath+"/atlas/v2/entity", body)
	if w.Code != http.StatusNotFound {
		t.Fatalf("create with an unknown type = %d, want 404; without this the type "+
			"system is decorative and every entity is valid", w.Code)
	}
	if got := errorCode(t, w); got != "ATLAS-404-00-001" {
		t.Fatalf("errorCode = %q, want ATLAS-404-00-001", got)
	}
}

func TestAnEntityMissingARequiredAttributeIsRefused(t *testing.T) {
	_, mux := newService(t)
	// `name` is required on Asset; DataSet inherits it. Supplying only
	// qualifiedName must fail, which is what proves the check walks supertypes.
	body := `{"entity":{"typeName":"DataSet","attributes":{"qualifiedName":"x://a"}}}`
	w := do(t, mux, "POST", BasePath+"/atlas/v2/entity", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing required attribute = %d, want 400", w.Code)
	}
	if got := errorCode(t, w); got != "ATLAS-400-00-01B" {
		t.Fatalf("errorCode = %q, want ATLAS-400-00-01B", got)
	}
}

// qualifiedName is enforced EXPLICITLY, not only through the inherited
// attributeDef — it is the storage identity, so a missing one has nowhere to go
// regardless of what the type system says.
//
// The name of this test used to claim it proved the inheritance walk. It did
// not: severing the supertype walk leaves this passing, because the explicit
// check catches it first. The inheritance claim is carried by
// TestAnEntityMissingARequiredAttributeIsRefused, where `name` is declared on
// Asset and required of a DataSet — and that one does die when the walk is cut.
// Found by mutating the walk and reading WHICH tests failed rather than that
// some did.
func TestQualifiedNameIsAlwaysRequired(t *testing.T) {
	_, mux := newService(t)
	body := `{"entity":{"typeName":"DataSet","attributes":{"name":"a"}}}`
	w := do(t, mux, "POST", BasePath+"/atlas/v2/entity", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("entity with no qualifiedName = %d, want 400 — it is the identity "+
			"an entity is stored and looked up by", w.Code)
	}
}

func TestATypeWhoseSupertypeDoesNotExistIsRefused(t *testing.T) {
	_, mux := newService(t)
	body := `{"entityDefs":[{"name":"my_table","superTypes":["NoSuchBase"],"attributeDefs":[]}]}`
	w := do(t, mux, "POST", BasePath+"/atlas/v2/types/typedefs", body)
	if w.Code != http.StatusNotFound {
		t.Fatalf("dangling supertype = %d, want 404; accepting it defers the failure "+
			"to every later entity create, where the cause is invisible", w.Code)
	}
}

func TestRegisteringTheSameTypeNameTwiceIsRefused(t *testing.T) {
	_, mux := newService(t)
	body := `{"entityDefs":[{"name":"my_table","superTypes":["DataSet"],"attributeDefs":[]}]}`
	if w := do(t, mux, "POST", BasePath+"/atlas/v2/types/typedefs", body); w.Code != http.StatusOK {
		t.Fatalf("first create = %d %s", w.Code, w.Body)
	}
	w := do(t, mux, "POST", BasePath+"/atlas/v2/types/typedefs", body)
	if w.Code != http.StatusConflict {
		t.Fatalf("duplicate type = %d, want 409", w.Code)
	}
}

// A per-category read must not answer with a type of a DIFFERENT category.
// Handing back a classification when an entitydef was asked for would let a
// client build on a type that cannot hold what it is about to send.
func TestAPerCategoryReadDoesNotCastAcrossCategories(t *testing.T) {
	_, mux := newService(t)
	body := `{"classificationDefs":[{"name":"PII","attributeDefs":[]}]}`
	if w := do(t, mux, "POST", BasePath+"/atlas/v2/types/typedefs", body); w.Code != http.StatusOK {
		t.Fatalf("create classification = %d %s", w.Code, w.Body)
	}
	if w := do(t, mux, "GET", BasePath+"/atlas/v2/types/classificationdef/name/PII", ""); w.Code != http.StatusOK {
		t.Fatalf("classificationdef read = %d, want 200", w.Code)
	}
	w := do(t, mux, "GET", BasePath+"/atlas/v2/types/entitydef/name/PII", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("PII as an entitydef = %d, want 404 — it is a CLASSIFICATION", w.Code)
	}
}

// Deleting a type that instances still use would leave entities whose typeName
// resolves to nothing — the type system breaking its own rule.
func TestATypeInUseCannotBeDeleted(t *testing.T) {
	_, mux := newService(t)
	seedType(t, mux, "my_table")
	body := `{"entity":{"typeName":"my_table","attributes":{"qualifiedName":"x://t1","name":"t1"}}}`
	if w := do(t, mux, "POST", BasePath+"/atlas/v2/entity", body); w.Code != http.StatusOK {
		t.Fatalf("create entity = %d %s", w.Code, w.Body)
	}
	w := do(t, mux, "DELETE", BasePath+"/atlas/v2/types/typedef/name/my_table", "")
	if w.Code != http.StatusConflict {
		t.Fatalf("delete type in use = %d, want 409", w.Code)
	}
}

// --- identity ----------------------------------------------------------------

// THE BEHAVIOUR THE ENDPOINT IS NAMED FOR. Posting the same qualifiedName twice
// must update one entity, not create two. A stub that always inserted looks
// correct until someone counts, and the count is the only thing that shows it.
func TestTheSameQualifiedNameUpdatesRatherThanDuplicating(t *testing.T) {
	_, mux := newService(t)
	seedType(t, mux, "my_table")
	post := func(desc string) *httptest.ResponseRecorder {
		body := `{"entity":{"typeName":"my_table","attributes":{` +
			`"qualifiedName":"mssql://server/db/orders","name":"orders","description":"` + desc + `"}}}`
		return do(t, mux, "POST", BasePath+"/atlas/v2/entity", body)
	}
	first := post("v1")
	var r1 EntityMutationResponse
	_ = json.Unmarshal(first.Body.Bytes(), &r1)
	if len(r1.MutatedEntities[opCreate]) != 1 {
		t.Fatalf("first post did not report a CREATE: %s", first.Body)
	}
	guid := r1.MutatedEntities[opCreate][0].GUID

	second := post("v2")
	var r2 EntityMutationResponse
	_ = json.Unmarshal(second.Body.Bytes(), &r2)
	if len(r2.MutatedEntities[opUpdate]) != 1 {
		t.Fatalf("second post with the same qualifiedName must report UPDATE, got %s", second.Body)
	}
	if got := r2.MutatedEntities[opUpdate][0].GUID; got != guid {
		t.Fatalf("update produced a NEW guid %s (was %s) — the entity was duplicated", got, guid)
	}
	// And the read reflects the second write, or "update" meant nothing.
	w := do(t, mux, "GET", BasePath+"/atlas/v2/entity/guid/"+guid, "")
	if !bytes.Contains(w.Body.Bytes(), []byte(`"v2"`)) {
		t.Fatalf("entity still holds the first version: %s", w.Body)
	}
}

// Clients post placeholder negative GUIDs and read the real ones out of
// guidAssignments. Ignoring that map leaves a client unable to reference what
// it just created — and nothing in a single create would reveal it.
func TestNegativeGUIDsAreReplacedAndReported(t *testing.T) {
	_, mux := newService(t)
	seedType(t, mux, "my_table")
	body := `{"entity":{"typeName":"my_table","guid":"-1","attributes":` +
		`{"qualifiedName":"mssql://server/db/t","name":"t"}}}`
	w := do(t, mux, "POST", BasePath+"/atlas/v2/entity", body)
	if w.Code != http.StatusOK {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	var resp EntityMutationResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	assigned, ok := resp.GUIDAssignments["-1"]
	if !ok {
		t.Fatalf("no guidAssignments entry for the placeholder -1: %s", w.Body)
	}
	if assigned == "-1" || assigned == "" {
		t.Fatalf("guidAssignments[-1] = %q — the placeholder was stored as an identifier", assigned)
	}
	// The assigned GUID must actually resolve, or the map is cosmetic.
	if g := do(t, mux, "GET", BasePath+"/atlas/v2/entity/guid/"+assigned, ""); g.Code != http.StatusOK {
		t.Fatalf("the assigned guid does not resolve: %d", g.Code)
	}
}

// The lookup a client uses when it has no GUID — the normal case for anything
// discovered rather than authored.
func TestLookupByUniqueAttribute(t *testing.T) {
	_, mux := newService(t)
	seedType(t, mux, "my_table")
	body := `{"entity":{"typeName":"my_table","attributes":{"qualifiedName":"mssql://s/db/o","name":"o"}}}`
	if w := do(t, mux, "POST", BasePath+"/atlas/v2/entity", body); w.Code != http.StatusOK {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	w := do(t, mux, "GET", BasePath+
		"/atlas/v2/entity/uniqueAttribute/type/my_table?attr:qualifiedName=mssql://s/db/o", "")
	if w.Code != http.StatusOK {
		t.Fatalf("unique-attribute lookup = %d %s", w.Code, w.Body)
	}
	// A qualifiedName that exists under a DIFFERENT type must miss: the
	// attribute is unique per type, not globally.
	miss := do(t, mux, "GET", BasePath+
		"/atlas/v2/entity/uniqueAttribute/type/DataSet?attr:qualifiedName=mssql://s/db/o", "")
	if miss.Code != http.StatusNotFound {
		t.Fatalf("lookup under the wrong type = %d, want 404", miss.Code)
	}
}

// Atlas soft-deletes: "Deleted entities are not removed". A hard delete would
// break the lineage and audit reads later increments add.
func TestDeleteIsASoftDelete(t *testing.T) {
	_, mux := newService(t)
	seedType(t, mux, "my_table")
	body := `{"entity":{"typeName":"my_table","attributes":{"qualifiedName":"x://gone","name":"gone"}}}`
	w := do(t, mux, "POST", BasePath+"/atlas/v2/entity", body)
	var resp EntityMutationResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	guid := resp.MutatedEntities[opCreate][0].GUID

	if d := do(t, mux, "DELETE", BasePath+"/atlas/v2/entity/guid/"+guid, ""); d.Code != http.StatusOK {
		t.Fatalf("delete = %d %s", d.Code, d.Body)
	}
	got := do(t, mux, "GET", BasePath+"/atlas/v2/entity/guid/"+guid, "")
	if got.Code != http.StatusOK {
		t.Fatalf("deleted entity = %d, want 200: Atlas does not remove them", got.Code)
	}
	if !bytes.Contains(got.Body.Bytes(), []byte(`"DELETED"`)) {
		t.Fatalf("deleted entity is not marked DELETED: %s", got.Body)
	}
}

// A batch must not half-commit. The client sees one error and has no way to
// learn what landed, so anything that landed is invisible state.
func TestABatchWithOneBadEntityCommitsNothing(t *testing.T) {
	_, mux := newService(t)
	seedType(t, mux, "my_table")
	body := `{"entities":[
	  {"typeName":"my_table","attributes":{"qualifiedName":"x://good","name":"good"}},
	  {"typeName":"nonesuch","attributes":{"qualifiedName":"x://bad","name":"bad"}}
	]}`
	if w := do(t, mux, "POST", BasePath+"/atlas/v2/entity/bulk", body); w.Code != http.StatusNotFound {
		t.Fatalf("bulk with a bad type = %d, want 404 %s", w.Code, w.Body)
	}
	w := do(t, mux, "GET", BasePath+
		"/atlas/v2/entity/uniqueAttribute/type/my_table?attr:qualifiedName=x://good", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("the valid entity of a rejected batch was committed (%d) — validation "+
			"must complete before any write", w.Code)
	}
}

// seedType registers a client-defined type extending DataSet, the shape every
// SDK sample uses.
func seedType(t *testing.T, mux *http.ServeMux, name string) {
	t.Helper()
	body := `{"entityDefs":[{"name":"` + name + `","superTypes":["DataSet"],"attributeDefs":[]}]}`
	if w := do(t, mux, "POST", BasePath+"/atlas/v2/types/typedefs", body); w.Code != http.StatusOK {
		t.Fatalf("seed type %s = %d %s", name, w.Code, w.Body)
	}
}

// THE ROUTE-LEVEL AUTH HALF. Every test above registers with authentication
// bypassed, which is right for exercising Atlas semantics and proves nothing
// about the production mount. This drives the REAL Register — the same route
// table — and asserts an unauthenticated request is refused on each one. A
// route added to the table and reachable without a token fails here.
func TestEveryRouteRequiresAToken(t *testing.T) {
	st, err := store.Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := New(st, nil) // a nil validator is safe: the empty-token check comes first
	mux := http.NewServeMux()
	s.Register(mux)

	for _, c := range []struct{ method, target string }{
		{"GET", "/atlas/v2/types/typedefs"},
		{"POST", "/atlas/v2/types/typedefs"},
		{"PUT", "/atlas/v2/types/typedefs"},
		{"DELETE", "/atlas/v2/types/typedefs"},
		{"GET", "/atlas/v2/types/typedefs/headers"},
		{"GET", "/atlas/v2/types/typedef/name/DataSet"},
		{"GET", "/atlas/v2/types/typedef/guid/x"},
		{"DELETE", "/atlas/v2/types/typedef/name/DataSet"},
		{"GET", "/atlas/v2/types/entitydef/name/DataSet"},
		{"GET", "/atlas/v2/types/classificationdef/name/PII"},
		{"POST", "/atlas/v2/entity"},
		{"POST", "/atlas/v2/entity/bulk"},
		{"GET", "/atlas/v2/entity/bulk?guid=x"},
		{"DELETE", "/atlas/v2/entity/bulk?guid=x"},
		{"GET", "/atlas/v2/entity/guid/x"},
		{"DELETE", "/atlas/v2/entity/guid/x"},
		{"GET", "/atlas/v2/entity/guid/x/header"},
		{"GET", "/atlas/v2/entity/uniqueAttribute/type/DataSet?attr:qualifiedName=q"},
		{"DELETE", "/atlas/v2/entity/uniqueAttribute/type/DataSet?attr:qualifiedName=q"},
	} {
		w := do(t, mux, c.method, BasePath+c.target, "{}")
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d for an unauthenticated caller, want 401 — is the route "+
				"registered outside the shared table?", c.method, c.target, w.Code)
			continue
		}
		if got := errorCode(t, w); got != "ATLAS-401-00-001" {
			t.Errorf("%s %s errorCode = %q, want ATLAS-401-00-001", c.method, c.target, got)
		}
	}
}
