package purview

// Lineage is DERIVED from Process inputs/outputs. A stored-edge table would
// pass its own tests forever while drifting from the entities that justify the
// graph — so every assertion here is something a stored-edge stub would get
// wrong: a Process subclass must appear, a deleted Process must vanish, a
// dangling output must not 500, and direction/depth/width must bound the walk
// rather than decorate the reply.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func createProcess(t *testing.T, mux *http.ServeMux, qname, typeName, inputsJSON, outputsJSON string) string {
	t.Helper()
	if typeName == "" {
		typeName = "Process"
	}
	body := `{"entity":{"typeName":"` + typeName + `","attributes":{` +
		`"qualifiedName":"` + qname + `","name":"` + qname + `",` +
		`"inputs":` + inputsJSON + `,"outputs":` + outputsJSON + `}}}`
	w := do(t, mux, "POST", BasePath+"/atlas/v2/entity", body)
	if w.Code != http.StatusOK {
		t.Fatalf("create process %s = %d %s", qname, w.Code, w.Body)
	}
	var resp EntityMutationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.MutatedEntities[opCreate]) != 1 {
		t.Fatalf("create process %s did not report a CREATE: %s", qname, w.Body)
	}
	return resp.MutatedEntities[opCreate][0].GUID
}

func getLineage(t *testing.T, mux *http.ServeMux, guid, query string) (*AtlasLineageInfo, *httptest.ResponseRecorder) {
	t.Helper()
	target := BasePath + "/atlas/v2/lineage/" + guid
	if query != "" {
		target += "?" + query
	}
	w := do(t, mux, "GET", target, "")
	if w.Code != http.StatusOK {
		return nil, w
	}
	var info AtlasLineageInfo
	if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil {
		t.Fatalf("lineage reply is not AtlasLineageInfo: %s", w.Body)
	}
	return &info, w
}

func refs(guids ...string) string {
	b, _ := json.Marshal(func() []map[string]string {
		out := make([]map[string]string, 0, len(guids))
		for _, g := range guids {
			out = append(out, map[string]string{"guid": g})
		}
		return out
	}())
	return string(b)
}

func stringsJSON(ss ...string) string {
	b, _ := json.Marshal(ss)
	return string(b)
}

// An unknown guid is a 404, not an empty graph. Returning 200 with no
// relations would make "this asset does not exist" indistinguishable from
// "this asset is isolated".
func TestLineageOfAnUnknownGUIDIsNotFound(t *testing.T) {
	_, mux := newService(t)
	_, w := getLineage(t, mux, "no-such-guid", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown guid = %d, want 404 (%s)", w.Code, w.Body)
	}
	if code := errorCode(t, w); code != "ATLAS-404-00-005" {
		t.Fatalf("errorCode = %s, want ATLAS-404-00-005", code)
	}
}

// The client asserts direction before sending. Silently coercing SIDEWAYS to
// BOTH would hide a caller bug behind a graph that looks complete.
func TestLineageRejectsAnUnknownDirection(t *testing.T) {
	_, mux := newService(t)
	src := createEntity(t, mux, "DataSet", "lake://src")
	_, w := getLineage(t, mux, src, "direction=SIDEWAYS")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad direction = %d, want 400 (%s)", w.Code, w.Body)
	}
	if code := errorCode(t, w); code != "ATLAS-400-00-06A" {
		t.Fatalf("errorCode = %s, want ATLAS-400-00-06A", code)
	}
}

// An asset with no Process pointing at it is isolated: 200, itself in the
// map, no relations. A stub that always invented an edge would fail this.
func TestAnIsolatedAssetReportsNoRelations(t *testing.T) {
	_, mux := newService(t)
	src := createEntity(t, mux, "DataSet", "lake://lonely")
	info, w := getLineage(t, mux, src, "")
	if w.Code != http.StatusOK {
		t.Fatalf("isolated = %d %s", w.Code, w.Body)
	}
	if info.BaseEntityGUID != src {
		t.Fatalf("baseEntityGuid = %s, want %s", info.BaseEntityGUID, src)
	}
	if info.LineageDirection != "BOTH" {
		t.Fatalf("default direction = %s, want BOTH", info.LineageDirection)
	}
	if info.LineageDepth != 3 {
		t.Fatalf("default depth = %d, want 3", info.LineageDepth)
	}
	if len(info.Relations) != 0 {
		t.Fatalf("isolated asset has relations: %+v", info.Relations)
	}
	if _, ok := info.GUIDEntityMap[src]; !ok {
		t.Fatalf("guidEntityMap is missing the base entity")
	}
}

// THE BEHAVIOUR THE ROUTE EXISTS FOR. A Process that reads A and writes B
// *is* the edge A -> B. The relationshipId is the process guid, because that
// is how a client asks "why is this connected?".
func TestLineageIsDerivedFromProcessInputsAndOutputs(t *testing.T) {
	_, mux := newService(t)
	src := createEntity(t, mux, "DataSet", "lake://bronze")
	dst := createEntity(t, mux, "DataSet", "lake://silver")
	proc := createProcess(t, mux, "job://copy", "Process", refs(src), refs(dst))

	info, w := getLineage(t, mux, src, "direction=OUTPUT")
	if w.Code != http.StatusOK {
		t.Fatalf("lineage = %d %s", w.Code, w.Body)
	}
	if len(info.Relations) != 1 {
		t.Fatalf("relations = %+v, want one A->B edge", info.Relations)
	}
	rel := info.Relations[0]
	if rel.FromEntityID != src || rel.ToEntityID != dst || rel.RelationshipID != proc {
		t.Fatalf("edge = %+v, want from=%s to=%s via=%s", rel, src, dst, proc)
	}
	for _, g := range []string{src, dst} {
		if _, ok := info.GUIDEntityMap[g]; !ok {
			t.Fatalf("guidEntityMap missing %s: %v", g, keysOf(info.GUIDEntityMap))
		}
	}
}

// pyapacheatlas emits object references; other clients send bare guid
// strings. Accepting only one shape would reject half the clients for a
// reason the error would not name.
func TestLineageAcceptsBareGUIDStringsAsWellAsObjectRefs(t *testing.T) {
	_, mux := newService(t)
	src := createEntity(t, mux, "DataSet", "lake://in")
	dst := createEntity(t, mux, "DataSet", "lake://out")
	createProcess(t, mux, "job://bare", "Process", stringsJSON(src), stringsJSON(dst))

	info, w := getLineage(t, mux, src, "direction=OUTPUT")
	if w.Code != http.StatusOK {
		t.Fatalf("lineage = %d %s", w.Code, w.Body)
	}
	if len(info.Relations) != 1 || info.Relations[0].FromEntityID != src || info.Relations[0].ToEntityID != dst {
		t.Fatalf("bare-guid process did not produce A->B: %+v", info.Relations)
	}
}

// INPUT walks backwards. Asking OUTPUT of the destination must not invent
// the same edge the other way — that would be a stored undirected graph.
func TestLineageDirectionBoundsTheWalk(t *testing.T) {
	_, mux := newService(t)
	src := createEntity(t, mux, "DataSet", "lake://a")
	dst := createEntity(t, mux, "DataSet", "lake://b")
	createProcess(t, mux, "job://oneway", "Process", refs(src), refs(dst))

	out, w := getLineage(t, mux, dst, "direction=OUTPUT")
	if w.Code != http.StatusOK {
		t.Fatalf("OUTPUT of dest = %d %s", w.Code, w.Body)
	}
	if len(out.Relations) != 0 {
		t.Fatalf("OUTPUT of the destination invented an edge: %+v", out.Relations)
	}

	in, w := getLineage(t, mux, dst, "direction=INPUT")
	if w.Code != http.StatusOK {
		t.Fatalf("INPUT of dest = %d %s", w.Code, w.Body)
	}
	if len(in.Relations) != 1 || in.Relations[0].FromEntityID != src {
		t.Fatalf("INPUT of dest = %+v, want the A->B edge", in.Relations)
	}
}

// depth is hops, not "nodes visited". A chain A->B->C with depth=1 from A
// must stop at B. Returning C would mean depth decorated the reply.
func TestLineageDepthIsHops(t *testing.T) {
	_, mux := newService(t)
	a := createEntity(t, mux, "DataSet", "lake://a")
	b := createEntity(t, mux, "DataSet", "lake://b")
	c := createEntity(t, mux, "DataSet", "lake://c")
	createProcess(t, mux, "job://ab", "Process", refs(a), refs(b))
	createProcess(t, mux, "job://bc", "Process", refs(b), refs(c))

	info, w := getLineage(t, mux, a, "direction=OUTPUT&depth=1")
	if w.Code != http.StatusOK {
		t.Fatalf("depth=1 = %d %s", w.Code, w.Body)
	}
	if len(info.Relations) != 1 || info.Relations[0].ToEntityID != b {
		t.Fatalf("depth=1 relations = %+v, want only A->B", info.Relations)
	}
	if _, ok := info.GUIDEntityMap[c]; ok {
		t.Fatalf("depth=1 reached C: %v", keysOf(info.GUIDEntityMap))
	}

	both, w := getLineage(t, mux, a, "direction=OUTPUT&depth=2")
	if w.Code != http.StatusOK {
		t.Fatalf("depth=2 = %d %s", w.Code, w.Body)
	}
	if len(both.Relations) != 2 {
		t.Fatalf("depth=2 relations = %+v, want A->B and B->C", both.Relations)
	}
}

// width bounds fan-out PER NODE. One shared source feeding many sinks must
// not dump the whole graph because the client asked for that source.
func TestLineageWidthBoundsFanout(t *testing.T) {
	_, mux := newService(t)
	src := createEntity(t, mux, "DataSet", "lake://hub")
	for _, name := range []string{"x", "y", "z"} {
		dst := createEntity(t, mux, "DataSet", "lake://"+name)
		createProcess(t, mux, "job://"+name, "Process", refs(src), refs(dst))
	}
	info, w := getLineage(t, mux, src, "direction=OUTPUT&width=1")
	if w.Code != http.StatusOK {
		t.Fatalf("width=1 = %d %s", w.Code, w.Body)
	}
	if len(info.Relations) != 1 {
		t.Fatalf("width=1 relations = %+v, want one edge", info.Relations)
	}
}

// A real model subclasses Process. Querying type_name = 'Process' would miss
// every CopyJob and return empty lineage that looks like "nothing yet".
func TestLineageFindsAProcessSubclass(t *testing.T) {
	_, mux := newService(t)
	body := `{"entityDefs":[{"name":"CopyJob","superTypes":["Process"],"attributeDefs":[]}]}`
	if w := do(t, mux, "POST", BasePath+"/atlas/v2/types/typedefs", body); w.Code != http.StatusOK {
		t.Fatalf("register CopyJob = %d %s", w.Code, w.Body)
	}
	src := createEntity(t, mux, "DataSet", "lake://in")
	dst := createEntity(t, mux, "DataSet", "lake://out")
	createProcess(t, mux, "job://copyjob", "CopyJob", refs(src), refs(dst))

	info, w := getLineage(t, mux, src, "direction=OUTPUT")
	if w.Code != http.StatusOK {
		t.Fatalf("subclass lineage = %d %s", w.Code, w.Body)
	}
	if len(info.Relations) != 1 {
		t.Fatalf("CopyJob produced no edge — ListEntitiesBySuperType missed the subclass: %+v", info.Relations)
	}
}

// Atlas soft-deletes. A deleted Process no longer connects the assets it
// used to join — otherwise lineage would outlive the job that justified it.
func TestADeletedProcessNoLongerConnects(t *testing.T) {
	_, mux := newService(t)
	src := createEntity(t, mux, "DataSet", "lake://in")
	dst := createEntity(t, mux, "DataSet", "lake://out")
	proc := createProcess(t, mux, "job://gone", "Process", refs(src), refs(dst))

	if d := do(t, mux, "DELETE", BasePath+"/atlas/v2/entity/guid/"+proc, ""); d.Code != http.StatusOK {
		t.Fatalf("delete process = %d %s", d.Code, d.Body)
	}
	info, w := getLineage(t, mux, src, "direction=OUTPUT")
	if w.Code != http.StatusOK {
		t.Fatalf("after delete = %d %s", w.Code, w.Body)
	}
	if len(info.Relations) != 0 {
		t.Fatalf("deleted process still connects: %+v", info.Relations)
	}
}

// A process may name an asset that was hard-deleted elsewhere. That is a
// fact about the data, not a 500.
func TestADanglingOutputDoesNotFailTheWalk(t *testing.T) {
	_, mux := newService(t)
	src := createEntity(t, mux, "DataSet", "lake://in")
	createProcess(t, mux, "job://dangling", "Process", refs(src), refs("never-created"))

	info, w := getLineage(t, mux, src, "direction=OUTPUT")
	if w.Code != http.StatusOK {
		t.Fatalf("dangling = %d %s", w.Code, w.Body)
	}
	if len(info.Relations) != 1 || info.Relations[0].ToEntityID != "never-created" {
		t.Fatalf("dangling relations = %+v", info.Relations)
	}
	if _, ok := info.GUIDEntityMap["never-created"]; ok {
		t.Fatalf("dangling guid was materialised in the map")
	}
}

// Two inputs and one output is two edges. A stub that stored "the process
// connected these sets" as one undirected blob would report one relation.
func TestAProcessWithTwoInputsContributesTwoEdges(t *testing.T) {
	_, mux := newService(t)
	a := createEntity(t, mux, "DataSet", "lake://a")
	b := createEntity(t, mux, "DataSet", "lake://b")
	out := createEntity(t, mux, "DataSet", "lake://out")
	createProcess(t, mux, "job://join", "Process", refs(a, b), refs(out))

	info, w := getLineage(t, mux, out, "direction=INPUT")
	if w.Code != http.StatusOK {
		t.Fatalf("join = %d %s", w.Code, w.Body)
	}
	if len(info.Relations) != 2 {
		t.Fatalf("join relations = %+v, want two edges", info.Relations)
	}
}

// Unparseable or non-positive bounds fall back rather than returning an
// empty graph that reads as "no lineage".
func TestLineageBoundsFallBackWhenUnparseable(t *testing.T) {
	_, mux := newService(t)
	src := createEntity(t, mux, "DataSet", "lake://src")
	dst := createEntity(t, mux, "DataSet", "lake://dst")
	createProcess(t, mux, "job://ok", "Process", refs(src), refs(dst))

	info, w := getLineage(t, mux, src, "direction=OUTPUT&depth=-1&width=nope")
	if w.Code != http.StatusOK {
		t.Fatalf("bad bounds = %d %s", w.Code, w.Body)
	}
	if info.LineageDepth != 3 {
		t.Fatalf("depth fallback = %d, want 3", info.LineageDepth)
	}
	if len(info.Relations) != 1 {
		t.Fatalf("fallback walk missed the edge: %+v", info.Relations)
	}
}

func TestReferencedGUIDsToleratesBothShapesAndJunk(t *testing.T) {
	if got := referencedGUIDs(json.RawMessage(`not-json`), "inputs"); got != nil {
		t.Fatalf("malformed body = %v, want nil", got)
	}
	if got := referencedGUIDs(json.RawMessage(`{"attributes":{}}`), "inputs"); got != nil {
		t.Fatalf("missing attr = %v, want nil", got)
	}
	if got := referencedGUIDs(json.RawMessage(`{"attributes":{"inputs":"x"}}`), "inputs"); got != nil {
		t.Fatalf("non-array attr = %v, want nil", got)
	}
	body := json.RawMessage(`{"attributes":{"inputs":["",{"guid":""},{"guid":"ok"},"bare",{"no":"guid"}]}}`)
	got := referencedGUIDs(body, "inputs")
	if len(got) != 2 || got[0] != "ok" || got[1] != "bare" {
		t.Fatalf("mixed = %v, want [ok bare]", got)
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
