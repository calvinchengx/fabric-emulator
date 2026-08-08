package purview

// The routes the first increment mounted but never exercised.
//
// `datamap_test.go` covers registration, createOrUpdate and the refusals that
// prove the type system is real. It stops there: the read, update and delete
// halves of both the type system and the entity store shipped with no test at
// all, which is how a mounted route can 500 on its first real client.
//
// The theme here is the same as next door — an assertion only earns its place
// if a stub would fail it. So each handler is checked for the thing that makes
// it *that* handler rather than a near neighbour: a header read must return a
// header and not the whole entity, a bulk read must answer for every GUID it
// was given, a delete must soft-delete, and an update must keep the server's
// GUID rather than take the client's.

import (
	"encoding/json"
	"net/http"
	"testing"
)

// createEntity posts one entity and returns its assigned GUID, so the read and
// delete tests below start from something that genuinely exists.
func createEntity(t *testing.T, mux *http.ServeMux, typeName, qname string) string {
	t.Helper()
	body := `{"entity":{"typeName":"` + typeName + `","attributes":{` +
		`"qualifiedName":"` + qname + `","name":"` + qname + `"}}}`
	w := do(t, mux, "POST", BasePath+"/atlas/v2/entity", body)
	if w.Code != http.StatusOK {
		t.Fatalf("create %s = %d %s", qname, w.Code, w.Body)
	}
	var resp EntityMutationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.MutatedEntities[opCreate]) != 1 {
		t.Fatalf("create %s did not report a CREATE: %s", qname, w.Body)
	}
	return resp.MutatedEntities[opCreate][0].GUID
}

// --- the type system: read, update, delete -----------------------------------

// PUT /types/typedefs. The GUID is the server's: an update that adopted the
// client's would invalidate every reference already handed out, and the only
// way to see it is to read the definition back after the write.
func TestUpdatingATypeDefKeepsTheServersGUID(t *testing.T) {
	_, mux := newService(t)
	seedType(t, mux, "my_table")

	get := do(t, mux, "GET", BasePath+"/atlas/v2/types/typedef/name/my_table", "")
	var before typeDefBody
	if err := json.Unmarshal(get.Body.Bytes(), &before); err != nil {
		t.Fatal(err)
	}
	if before.GUID == "" {
		t.Fatal("the seeded type has no guid to preserve")
	}

	// A different guid and a changed description, both from the client.
	body := `{"entityDefs":[{"name":"my_table","guid":"client-supplied","superTypes":["DataSet"],` +
		`"description":"second version","attributeDefs":[]}]}`
	w := do(t, mux, "PUT", BasePath+"/atlas/v2/types/typedefs", body)
	if w.Code != http.StatusOK {
		t.Fatalf("update = %d %s", w.Code, w.Body)
	}

	after := do(t, mux, "GET", BasePath+"/atlas/v2/types/typedef/name/my_table", "")
	var got map[string]any
	if err := json.Unmarshal(after.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["guid"] != before.GUID {
		t.Fatalf("guid = %v, want the server's %s — an update reassigned identity", got["guid"], before.GUID)
	}
	if got["description"] != "second version" {
		t.Fatalf("description = %v — the update did not take", got["description"])
	}
}

// Updating a type that was never registered is a 404, not a create. Silently
// upserting would let a typo register a second type that no entity can use.
func TestUpdatingAnUnregisteredTypeIsRefused(t *testing.T) {
	_, mux := newService(t)
	body := `{"entityDefs":[{"name":"never_registered","superTypes":["DataSet"],"attributeDefs":[]}]}`
	w := do(t, mux, "PUT", BasePath+"/atlas/v2/types/typedefs", body)
	if w.Code != http.StatusNotFound {
		t.Fatalf("update of an unknown type = %d, want 404 (%s)", w.Code, w.Body)
	}
	if code := errorCode(t, w); code != "ATLAS-404-00-001" {
		t.Fatalf("errorCode = %s, want ATLAS-404-00-001", code)
	}
}

// DELETE /types/typedefs is the bulk form of the single-name delete, and it
// inherits the same in-use guard. If it did not, the bulk route would be a way
// around a rule the single route enforces.
func TestBulkTypeDefDeleteHonoursTheInUseGuard(t *testing.T) {
	_, mux := newService(t)
	seedType(t, mux, "my_table")
	createEntity(t, mux, "my_table", "mssql://s/db/in_use")

	body := `{"entityDefs":[{"name":"my_table"}]}`
	w := do(t, mux, "DELETE", BasePath+"/atlas/v2/types/typedefs", body)
	if w.Code != http.StatusConflict {
		t.Fatalf("bulk delete of a type in use = %d, want 409 (%s)", w.Code, w.Body)
	}

	// Unreferenced, it goes — otherwise the guard would be indistinguishable
	// from the route simply never deleting anything.
	seedType(t, mux, "unused_table")
	free := do(t, mux, "DELETE", BasePath+"/atlas/v2/types/typedefs",
		`{"entityDefs":[{"name":"unused_table"}]}`)
	if free.Code != http.StatusNoContent {
		t.Fatalf("bulk delete of an unused type = %d, want 204 (%s)", free.Code, free.Body)
	}
	if g := do(t, mux, "GET", BasePath+"/atlas/v2/types/typedef/name/unused_table", ""); g.Code != http.StatusNotFound {
		t.Fatalf("the deleted type still resolves: %d", g.Code)
	}
}

// GET /types/typedefs, with and without ?type=. The narrowing matters: a client
// asking for entity definitions and receiving the seeded classifications and
// enums as well has to filter them itself, which is the bug the parameter exists
// to prevent.
func TestListingTypeDefsNarrowsByCategory(t *testing.T) {
	_, mux := newService(t)
	seedType(t, mux, "my_table")

	all := do(t, mux, "GET", BasePath+"/atlas/v2/types/typedefs", "")
	if all.Code != http.StatusOK {
		t.Fatalf("list = %d %s", all.Code, all.Body)
	}
	var full AtlasTypesDef
	if err := json.Unmarshal(all.Body.Bytes(), &full); err != nil {
		t.Fatal(err)
	}
	if len(full.EntityDefs) == 0 {
		t.Fatalf("no entityDefs listed, but the base hierarchy is seeded: %s", all.Body)
	}

	narrowed := do(t, mux, "GET", BasePath+"/atlas/v2/types/typedefs?type=entity", "")
	var only AtlasTypesDef
	if err := json.Unmarshal(narrowed.Body.Bytes(), &only); err != nil {
		t.Fatal(err)
	}
	if len(only.EntityDefs) != len(full.EntityDefs) {
		t.Fatalf("?type=entity returned %d entityDefs, unfiltered returned %d",
			len(only.EntityDefs), len(full.EntityDefs))
	}
	// The query is case-insensitive (`entity` above, uppercased server-side),
	// and it must actually exclude the other categories.
	if len(only.ClassificationDefs)+len(only.EnumDefs)+len(only.StructDefs) != 0 {
		t.Fatalf("?type=entity leaked other categories: %s", narrowed.Body)
	}
}

// GET /types/typedefs/headers is the listing a client pages through before
// fetching anything in full, so it must carry the three fields that make a
// definition addressable — and not silently omit the guid.
func TestTypeDefHeadersCarryGUIDNameAndCategory(t *testing.T) {
	_, mux := newService(t)
	w := do(t, mux, "GET", BasePath+"/atlas/v2/types/typedefs/headers", "")
	if w.Code != http.StatusOK {
		t.Fatalf("headers = %d %s", w.Code, w.Body)
	}
	var headers []typeDefHeader
	if err := json.Unmarshal(w.Body.Bytes(), &headers); err != nil {
		t.Fatal(err)
	}
	if len(headers) == 0 {
		t.Fatalf("no headers returned, but the base hierarchy is seeded: %s", w.Body)
	}
	var dataset *typeDefHeader
	for i := range headers {
		if headers[i].Name == "DataSet" {
			dataset = &headers[i]
		}
	}
	if dataset == nil {
		t.Fatalf("DataSet missing from the headers listing: %s", w.Body)
	}
	if dataset.GUID == "" || dataset.Category != "ENTITY" {
		t.Fatalf("DataSet header = %+v, want a guid and category ENTITY", *dataset)
	}
	// The guid a header advertises must be the one the guid route resolves,
	// or paging the headers gives a client identifiers it cannot use.
	if g := do(t, mux, "GET", BasePath+"/atlas/v2/types/typedef/guid/"+dataset.GUID, ""); g.Code != http.StatusOK {
		t.Fatalf("the guid from the header listing does not resolve: %d %s", g.Code, g.Body)
	}
}

func TestGetTypeDefByGUIDMissesOnAnUnknownGUID(t *testing.T) {
	_, mux := newService(t)
	w := do(t, mux, "GET", BasePath+"/atlas/v2/types/typedef/guid/00000000-0000-0000-0000-000000000000", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown typedef guid = %d, want 404 (%s)", w.Code, w.Body)
	}
}

// The per-category read has two spellings, by name and by guid, and only the
// name form was tested. They must agree: a guid that resolves to a
// classification is not an entitydef, however it was looked up.
func TestAPerCategoryGUIDReadDoesNotCastAcrossCategories(t *testing.T) {
	_, mux := newService(t)
	body := `{"classificationDefs":[{"name":"PII","superTypes":[],"attributeDefs":[]}]}`
	if w := do(t, mux, "POST", BasePath+"/atlas/v2/types/typedefs", body); w.Code != http.StatusOK {
		t.Fatalf("register classification = %d %s", w.Code, w.Body)
	}
	get := do(t, mux, "GET", BasePath+"/atlas/v2/types/typedef/name/PII", "")
	var def typeDefBody
	if err := json.Unmarshal(get.Body.Bytes(), &def); err != nil {
		t.Fatal(err)
	}
	if def.GUID == "" {
		t.Fatalf("the registered classification has no guid: %s", get.Body)
	}

	hit := do(t, mux, "GET", BasePath+"/atlas/v2/types/classificationdef/guid/"+def.GUID, "")
	if hit.Code != http.StatusOK {
		t.Fatalf("classificationdef by guid = %d, want 200 (%s)", hit.Code, hit.Body)
	}
	miss := do(t, mux, "GET", BasePath+"/atlas/v2/types/entitydef/guid/"+def.GUID, "")
	if miss.Code != http.StatusNotFound {
		t.Fatalf("a classification read as an entitydef = %d, want 404 (%s)", miss.Code, miss.Body)
	}
}

// --- entities: header, bulk read, bulk delete, delete by attribute -----------

// GET /entity/guid/{guid}/header returns a header, not the entity. The
// distinction is the whole point of the route — a client that paged headers and
// received full bodies would lose the bandwidth saving it asked for.
func TestEntityHeaderIsAHeaderNotTheEntity(t *testing.T) {
	_, mux := newService(t)
	seedType(t, mux, "my_table")
	guid := createEntity(t, mux, "my_table", "mssql://s/db/orders")

	w := do(t, mux, "GET", BasePath+"/atlas/v2/entity/guid/"+guid+"/header", "")
	if w.Code != http.StatusOK {
		t.Fatalf("header = %d %s", w.Code, w.Body)
	}
	var h entityHeader
	if err := json.Unmarshal(w.Body.Bytes(), &h); err != nil {
		t.Fatal(err)
	}
	if h.GUID != guid || h.TypeName != "my_table" {
		t.Fatalf("header = %+v, want guid %s of my_table", h, guid)
	}
	if h.Status != statusActive {
		t.Fatalf("header status = %q, want %s", h.Status, statusActive)
	}
	// displayText is what a UI lists; falling back to qualifiedName when there
	// is no name is the documented behaviour, and here `name` is set.
	if h.DisplayText != "mssql://s/db/orders" {
		t.Fatalf("displayText = %q, want the entity's name", h.DisplayText)
	}
	// The full read is a different shape — if these were the same response the
	// route would be redundant rather than lighter.
	full := do(t, mux, "GET", BasePath+"/atlas/v2/entity/guid/"+guid, "")
	if full.Body.String() == w.Body.String() {
		t.Fatal("the header route returned the same payload as the full entity read")
	}
}

func TestEntityHeaderMissesOnAnUnknownGUID(t *testing.T) {
	_, mux := newService(t)
	w := do(t, mux, "GET", BasePath+"/atlas/v2/entity/guid/does-not-exist/header", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("header of an unknown guid = %d, want 404 (%s)", w.Code, w.Body)
	}
	if code := errorCode(t, w); code != "ATLAS-404-00-005" {
		t.Fatalf("errorCode = %s, want ATLAS-404-00-005", code)
	}
}

// GET /entity/bulk?guid=…&guid=… — the read a client uses after a bulk create.
// Answering for only the first GUID would look correct against a single-item
// call and silently truncate every real one.
func TestBulkEntityReadAnswersForEveryGUID(t *testing.T) {
	_, mux := newService(t)
	seedType(t, mux, "my_table")
	a := createEntity(t, mux, "my_table", "mssql://s/db/a")
	b := createEntity(t, mux, "my_table", "mssql://s/db/b")

	w := do(t, mux, "GET", BasePath+"/atlas/v2/entity/bulk?guid="+a+"&guid="+b, "")
	if w.Code != http.StatusOK {
		t.Fatalf("bulk read = %d %s", w.Code, w.Body)
	}
	var out AtlasEntitiesWithExtInfo
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Entities) != 2 {
		t.Fatalf("bulk read returned %d entities for 2 guids: %s", len(out.Entities), w.Body)
	}
}

// No `guid` at all is a client error, not an empty 200. An empty list would
// read as "these entities do not exist", which is a different fact.
func TestBulkEntityReadRequiresAtLeastOneGUID(t *testing.T) {
	_, mux := newService(t)
	w := do(t, mux, "GET", BasePath+"/atlas/v2/entity/bulk", "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bulk read with no guid = %d, want 400 (%s)", w.Code, w.Body)
	}
	if code := errorCode(t, w); code != "ATLAS-400-00-001" {
		t.Fatalf("errorCode = %s, want ATLAS-400-00-001", code)
	}
}

// DELETE /entity/bulk reports one mutated header per entity and soft-deletes
// each. Atlas keeps deleted entities readable; a hard delete here would break
// the lineage and audit reads later increments add.
func TestBulkDeleteSoftDeletesEveryEntity(t *testing.T) {
	_, mux := newService(t)
	seedType(t, mux, "my_table")
	a := createEntity(t, mux, "my_table", "mssql://s/db/a")
	b := createEntity(t, mux, "my_table", "mssql://s/db/b")

	w := do(t, mux, "DELETE", BasePath+"/atlas/v2/entity/bulk?guid="+a+"&guid="+b, "")
	if w.Code != http.StatusOK {
		t.Fatalf("bulk delete = %d %s", w.Code, w.Body)
	}
	var resp EntityMutationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.MutatedEntities[opDelete]) != 2 {
		t.Fatalf("bulk delete reported %d DELETEs for 2 guids: %s",
			len(resp.MutatedEntities[opDelete]), w.Body)
	}
	for _, guid := range []string{a, b} {
		got := do(t, mux, "GET", BasePath+"/atlas/v2/entity/guid/"+guid, "")
		if got.Code != http.StatusOK {
			t.Fatalf("entity %s is gone after delete (%d) — the delete was not soft", guid, got.Code)
		}
		var ext AtlasEntityWithExtInfo
		if err := json.Unmarshal(got.Body.Bytes(), &ext); err != nil {
			t.Fatal(err)
		}
		var generic map[string]any
		if err := json.Unmarshal(ext.Entity, &generic); err != nil {
			t.Fatal(err)
		}
		if generic["status"] != statusDeleted {
			t.Fatalf("entity %s status = %v, want %s", guid, generic["status"], statusDeleted)
		}
	}
}

// DELETE /entity/uniqueAttribute/type/{typeName}?attr:qualifiedName=… — the
// delete a client issues when it has a name and no GUID.
func TestDeleteByUniqueAttributeSoftDeletesTheRightEntity(t *testing.T) {
	_, mux := newService(t)
	seedType(t, mux, "my_table")
	target := createEntity(t, mux, "my_table", "mssql://s/db/target")
	bystander := createEntity(t, mux, "my_table", "mssql://s/db/bystander")

	w := do(t, mux, "DELETE", BasePath+
		"/atlas/v2/entity/uniqueAttribute/type/my_table?attr:qualifiedName=mssql://s/db/target", "")
	if w.Code != http.StatusOK {
		t.Fatalf("delete by unique attribute = %d %s", w.Code, w.Body)
	}
	var resp EntityMutationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if n := len(resp.MutatedEntities[opDelete]); n != 1 {
		t.Fatalf("reported %d DELETEs, want 1: %s", n, w.Body)
	}
	if got := resp.MutatedEntities[opDelete][0].GUID; got != target {
		t.Fatalf("deleted %s, want the entity matching the qualifiedName (%s)", got, target)
	}
	// The other entity of the same type is untouched — a delete keyed on the
	// type alone would take both, and only a second entity reveals it.
	other := do(t, mux, "GET", BasePath+"/atlas/v2/entity/guid/"+bystander, "")
	var ext AtlasEntityWithExtInfo
	if err := json.Unmarshal(other.Body.Bytes(), &ext); err != nil {
		t.Fatal(err)
	}
	var generic map[string]any
	if err := json.Unmarshal(ext.Entity, &generic); err != nil {
		t.Fatal(err)
	}
	if generic["status"] == statusDeleted {
		t.Fatal("deleting by qualifiedName also deleted a different entity of the same type")
	}
}

// The single-entity routes each have a miss branch, and a 500 there would send
// a client chasing a server fault for what is really "no such entity".
func TestSingleEntityRoutesMissCleanlyOnAnUnknownGUID(t *testing.T) {
	_, mux := newService(t)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/atlas/v2/entity/guid/does-not-exist"},
		{"DELETE", "/atlas/v2/entity/guid/does-not-exist"},
	} {
		w := do(t, mux, tc.method, BasePath+tc.path, "")
		if w.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404 (%s)", tc.method, tc.path, w.Code, w.Body)
			continue
		}
		if code := errorCode(t, w); code != "ATLAS-404-00-005" {
			t.Errorf("%s %s errorCode = %s, want ATLAS-404-00-005", tc.method, tc.path, code)
		}
	}
}

func TestDeleteByUniqueAttributeMissesOnAnUnknownName(t *testing.T) {
	_, mux := newService(t)
	seedType(t, mux, "my_table")
	w := do(t, mux, "DELETE", BasePath+
		"/atlas/v2/entity/uniqueAttribute/type/my_table?attr:qualifiedName=mssql://s/db/nope", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("delete of an unknown qualifiedName = %d, want 404 (%s)", w.Code, w.Body)
	}
	if code := errorCode(t, w); code != "ATLAS-404-00-00A" {
		t.Fatalf("errorCode = %s, want ATLAS-404-00-00A", code)
	}
}
