package purview

// Atlas lineage, DERIVED rather than stored.
//
// There is no lineage table and there must not be one. Atlas computes lineage
// by walking `Process` entities' `inputs` and `outputs`: a process that reads A
// and writes B *is* the edge A -> B. Storing edges separately would drift from
// the entities that justify them and would pass its own tests forever, which is
// the failure this package's other tests exist to prevent.
//
// Confirmed before implementing, three ways: our own seed describes Process as
// "something that reads inputs and writes outputs, lineage hangs off this";
// `pyapacheatlas.core.entity.AtlasProcess` "forces you to include the inputs
// and outputs of the process"; and the read route is a graph walk with depth,
// width and direction rather than a fetch.

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
)

// AtlasLineageInfo is the documented reply shape. `guidEntityMap` carries the
// entities the relations refer to, so a client can render a graph from one call
// rather than fetching every node.
type AtlasLineageInfo struct {
	BaseEntityGUID   string                     `json:"baseEntityGuid"`
	LineageDirection string                     `json:"lineageDirection"`
	LineageDepth     int                        `json:"lineageDepth"`
	GUIDEntityMap    map[string]json.RawMessage `json:"guidEntityMap"`
	Relations        []AtlasLineageRelation     `json:"relations"`
}

// AtlasLineageRelation is one edge. `relationshipId` is the PROCESS's guid: the
// edge exists because that process declared the two assets, so naming it is how
// a client asks "why is this connected?".
type AtlasLineageRelation struct {
	FromEntityID   string `json:"fromEntityId"`
	ToEntityID     string `json:"toEntityId"`
	RelationshipID string `json:"relationshipId"`
}

// edge is one derived connection plus the process that justifies it.
type edge struct{ from, to, via string }

// processEdges reads every process reachable through the Process supertype and
// expands it into the cross-product of its inputs and outputs. A process with
// two inputs and one output contributes two edges, which is what Atlas shows.
func (s *Service) processEdges() ([]edge, error) {
	procs, err := s.Store.ListEntitiesBySuperType("Process")
	if err != nil {
		return nil, err
	}
	out := []edge{}
	for _, p := range procs {
		ins, outs := referencedGUIDs(p.Body, "inputs"), referencedGUIDs(p.Body, "outputs")
		for _, i := range ins {
			for _, o := range outs {
				out = append(out, edge{from: i, to: o, via: p.GUID})
			}
		}
	}
	return out, nil
}

// referencedGUIDs pulls guids out of one attribute, tolerating BOTH shapes a
// real client sends: a bare guid string, and an object reference carrying
// `guid` (what `AtlasProcess` emits). Accepting only one shape would reject
// half of the clients for a reason the error would not name.
func referencedGUIDs(body json.RawMessage, attr string) []string {
	var e struct {
		Attributes map[string]json.RawMessage `json:"attributes"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		return nil
	}
	raw, ok := e.Attributes[attr]
	if !ok {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	out := []string{}
	for _, it := range items {
		var str string
		if err := json.Unmarshal(it, &str); err == nil {
			if str != "" {
				out = append(out, str)
			}
			continue
		}
		var ref struct {
			GUID string `json:"guid"`
		}
		if err := json.Unmarshal(it, &ref); err == nil && ref.GUID != "" {
			out = append(out, ref.GUID)
		}
	}
	return out
}

// getLineage serves GET /atlas/v2/lineage/{guid}.
func (s *Service) getLineage(w http.ResponseWriter, r *http.Request, _ *auth.Principal) {
	guid := r.PathValue("guid")
	if _, err := s.Store.GetEntity(guid); err != nil {
		writeAtlasErr(w, http.StatusNotFound, "ATLAS-404-00-005",
			"Given instance guid "+guid+" is invalid/not found.")
		return
	}

	direction := strings.ToUpper(r.URL.Query().Get("direction"))
	if direction == "" {
		direction = "BOTH"
	}
	// The client asserts this set before sending, so an unknown value is a
	// caller error rather than something to silently coerce to BOTH.
	if direction != "BOTH" && direction != "INPUT" && direction != "OUTPUT" {
		writeAtlasErr(w, http.StatusBadRequest, "ATLAS-400-00-06A",
			"Invalid lineage direction "+direction+". Valid values: BOTH, INPUT, OUTPUT.")
		return
	}
	depth := intParam(r, "depth", 3)
	width := intParam(r, "width", 10)

	edges, err := s.processEdges()
	if err != nil {
		writeAtlasErr(w, http.StatusInternalServerError, "ATLAS-500-00-001", err.Error())
		return
	}

	info := AtlasLineageInfo{
		BaseEntityGUID:   guid,
		LineageDirection: direction,
		LineageDepth:     depth,
		GUIDEntityMap:    map[string]json.RawMessage{},
		Relations:        []AtlasLineageRelation{},
	}
	seen := map[string]bool{guid: true}
	collected := map[edge]bool{}

	// Breadth-first, one hop per level, so `depth` means hops as documented
	// rather than "nodes visited". INPUT walks backwards, OUTPUT forwards, and
	// BOTH does each — a node reached by both directions is still one node.
	frontier := []string{guid}
	for hop := 0; hop < depth && len(frontier) > 0; hop++ {
		next := []string{}
		for _, cur := range frontier {
			fanout := 0
			for _, e := range edges {
				var other string
				switch {
				case (direction == "OUTPUT" || direction == "BOTH") && e.from == cur:
					other = e.to
				case (direction == "INPUT" || direction == "BOTH") && e.to == cur:
					other = e.from
				default:
					continue
				}
				// `width` bounds fan-out PER NODE, which is what stops one
				// heavily-shared asset returning the whole graph.
				if fanout >= width {
					break
				}
				fanout++
				if !collected[e] {
					collected[e] = true
					info.Relations = append(info.Relations, AtlasLineageRelation{
						FromEntityID: e.from, ToEntityID: e.to, RelationshipID: e.via,
					})
				}
				if !seen[other] {
					seen[other] = true
					next = append(next, other)
				}
			}
		}
		frontier = next
	}

	for g := range seen {
		row, err := s.Store.GetEntity(g)
		if err != nil {
			// A dangling reference is a fact about the data, not an error: a
			// process may name an asset that was hard-deleted elsewhere. Skip
			// it rather than failing the whole walk.
			continue
		}
		var generic map[string]any
		_ = json.Unmarshal(row.Body, &generic)
		b, err := json.Marshal(headerOf(row, generic))
		if err != nil {
			continue
		}
		info.GUIDEntityMap[g] = b
	}
	writeJSON(w, http.StatusOK, info)
}

func intParam(r *http.Request, name string, def int) int {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	// A negative or unparseable bound would make the walk return nothing, which
	// reads as "no lineage" rather than "bad request", so fall back instead.
	if err != nil || n <= 0 {
		return def
	}
	return n
}
