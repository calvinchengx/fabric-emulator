package purview

// Atlas entities.
//
// THE THREE BEHAVIOURS A CLIENT ACTUALLY DEPENDS ON, and each is a way a stub
// would be caught:
//
//  1. **The type must exist.** An entity naming an unregistered type is
//     refused. Without this, `typeName` is free text and every entity is valid
//     — the permissive default that turns each gap into a silent success.
//  2. **`qualifiedName` is the identity.** Atlas's unique attribute per type,
//     which is why the endpoint is createOrUpdate and not create: posting the
//     same qualifiedName twice updates one entity rather than making two. A
//     stub that always inserted would look right until someone counted.
//  3. **Negative GUIDs are placeholders.** Clients (pyapacheatlas among them)
//     post entities with client-assigned negative GUIDs and read the real ones
//     out of `guidAssignments` in the response. Ignoring that map leaves the
//     client unable to reference what it just created.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// AtlasEntityWithExtInfo is the single-entity envelope: the entity plus any
// referred entities. `referredEntities` is round-tripped rather than resolved
// in this increment — relationships are a later one, and inventing resolution
// would claim more than is built.
type AtlasEntityWithExtInfo struct {
	Entity           json.RawMessage            `json:"entity,omitempty"`
	ReferredEntities map[string]json.RawMessage `json:"referredEntities,omitempty"`
}

// AtlasEntitiesWithExtInfo is the bulk envelope.
type AtlasEntitiesWithExtInfo struct {
	Entities         []json.RawMessage          `json:"entities,omitempty"`
	ReferredEntities map[string]json.RawMessage `json:"referredEntities,omitempty"`
}

// EntityMutationResponse is the documented reply to any create/update/delete:
// which GUIDs were assigned, and what was mutated under which operation.
type EntityMutationResponse struct {
	GUIDAssignments        map[string]string         `json:"guidAssignments,omitempty"`
	MutatedEntities        map[string][]entityHeader `json:"mutatedEntities,omitempty"`
	PartialUpdatedEntities []entityHeader            `json:"partialUpdatedEntities,omitempty"`
}

// entityHeader is AtlasEntityHeader reduced to what this increment fills.
type entityHeader struct {
	GUID          string         `json:"guid"`
	TypeName      string         `json:"typeName"`
	Status        string         `json:"status,omitempty"`
	Attributes    map[string]any `json:"attributes,omitempty"`
	DisplayText   string         `json:"displayText,omitempty"`
	ClassNames    []string       `json:"classificationNames,omitempty"`
	MeaningNames  []string       `json:"meaningNames,omitempty"`
	LastModifedTS string         `json:"lastModifiedTS,omitempty"`
}

const (
	statusActive  = "ACTIVE"
	statusDeleted = "DELETED"
	opCreate      = "CREATE"
	opUpdate      = "UPDATE"
	opDelete      = "DELETE"
)

// entityBody is the part of an entity this code reasons about; everything else
// round-trips untouched.
type entityBody struct {
	TypeName   string         `json:"typeName"`
	GUID       string         `json:"guid"`
	Status     string         `json:"status"`
	Attributes map[string]any `json:"attributes"`
}

func (s *Service) createOrUpdateEntity(w http.ResponseWriter, r *http.Request, _ *auth.Principal) {
	var in AtlasEntityWithExtInfo
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || len(in.Entity) == 0 {
		writeAtlasErr(w, http.StatusBadRequest, "ATLAS-400-00-001",
			"Request body must carry an `entity`.")
		return
	}
	resp, err := s.mutate([]json.RawMessage{in.Entity})
	if err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Service) createOrUpdateEntities(w http.ResponseWriter, r *http.Request, _ *auth.Principal) {
	var in AtlasEntitiesWithExtInfo
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeAtlasErr(w, http.StatusBadRequest, "ATLAS-400-00-001", "Malformed request body.")
		return
	}
	resp, err := s.mutate(in.Entities)
	if err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// mutate applies a batch. It is ALL-OR-NOTHING on validation: every entity is
// checked before any is written, so a batch whose third entity names a bad type
// does not leave the first two committed. A partial write here would be
// invisible — the client sees one error and has no way to learn what landed.
func (s *Service) mutate(raws []json.RawMessage) (*EntityMutationResponse, error) {
	if len(raws) == 0 {
		return &EntityMutationResponse{}, nil
	}
	type pending struct {
		body     entityBody
		enriched map[string]any
		qname    string
	}
	prepared := make([]pending, 0, len(raws))
	for i, raw := range raws {
		var body entityBody
		if err := json.Unmarshal(raw, &body); err != nil {
			return nil, &clientError{"ATLAS-400-00-001",
				fmt.Sprintf("entity[%d] is malformed: %v", i, err)}
		}
		def, err := s.entityDefFor(body.TypeName)
		if err != nil {
			return nil, err
		}
		qname, err := s.validateAttributes(def, body)
		if err != nil {
			return nil, err
		}
		var generic map[string]any
		if err := json.Unmarshal(raw, &generic); err != nil {
			return nil, &clientError{"ATLAS-400-00-001", err.Error()}
		}
		prepared = append(prepared, pending{body: body, enriched: generic, qname: qname})
	}

	resp := &EntityMutationResponse{
		GUIDAssignments: map[string]string{},
		MutatedEntities: map[string][]entityHeader{},
	}
	for _, p := range prepared {
		status := p.body.Status
		if status == "" {
			status = statusActive
		}
		row := &store.AtlasEntityRow{
			TypeName: p.body.TypeName, QualifiedName: p.qname, Status: status,
		}
		// A client-supplied GUID is honoured only when it is a real one. A
		// negative GUID is Atlas's placeholder convention, not an identifier,
		// and storing it would hand the client back the very string it invented.
		if !isPlaceholderGUID(p.body.GUID) {
			row.GUID = p.body.GUID
		}
		bodyJSON, err := json.Marshal(p.enriched)
		if err != nil {
			return nil, err
		}
		row.Body = bodyJSON
		created, err := s.Store.PutEntity(row)
		if err != nil {
			return nil, err
		}
		// Re-marshal with the assigned guid and status so a read returns what
		// the server decided rather than what the client proposed.
		p.enriched["guid"] = row.GUID
		p.enriched["status"] = row.Status
		if bodyJSON, err = json.Marshal(p.enriched); err == nil {
			row.Body = bodyJSON
			if _, err := s.Store.PutEntity(row); err != nil {
				return nil, err
			}
		}
		if isPlaceholderGUID(p.body.GUID) {
			resp.GUIDAssignments[p.body.GUID] = row.GUID
		}
		op := opUpdate
		if created {
			op = opCreate
		}
		resp.MutatedEntities[op] = append(resp.MutatedEntities[op], headerOf(row, p.enriched))
	}
	return resp, nil
}

// isPlaceholderGUID reports whether a GUID is one of Atlas's client-side
// placeholders. The convention is a NEGATIVE integer — "-1", "-2", … — which
// the server replaces and reports back in guidAssignments.
func isPlaceholderGUID(guid string) bool {
	if guid == "" {
		return false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(guid), 10, 64)
	return err == nil && n < 0
}

// entityDefFor resolves the type an entity claims, refusing anything that is
// not a registered ENTITY definition.
func (s *Service) entityDefFor(typeName string) (*typeDefBody, error) {
	if strings.TrimSpace(typeName) == "" {
		return nil, &clientError{"ATLAS-400-00-00B", "An entity requires a typeName."}
	}
	td, err := s.Store.GetTypeDefByName(typeName)
	if errors.Is(err, store.ErrNotFound) {
		return nil, &clientError{"ATLAS-404-00-001",
			"Given typename " + typeName + " was invalid."}
	}
	if err != nil {
		return nil, err
	}
	if td.Category != "ENTITY" {
		return nil, &clientError{"ATLAS-400-00-001",
			"Given typename " + typeName + " is a " + td.Category + ", not an entity type."}
	}
	var body typeDefBody
	if err := json.Unmarshal(td.Body, &body); err != nil {
		return nil, err
	}
	return &body, nil
}

// validateAttributes enforces the definition against the instance and returns
// the entity's qualifiedName.
//
// Inherited attributes count: an entity type's supertypes contribute their
// attributeDefs, which is how `qualifiedName` reaches almost every type in
// Purview — it is declared once on `Referenceable` and inherited everywhere.
// Walking only the type's own attributeDefs would make the check pass for
// exactly the types that matter most.
func (s *Service) validateAttributes(def *typeDefBody, e entityBody) (string, error) {
	attrs, err := s.effectiveAttributeDefs(def, map[string]bool{})
	if err != nil {
		return "", err
	}
	for _, ad := range attrs {
		optional := ad.IsOptional == nil || *ad.IsOptional
		if optional {
			continue
		}
		v, ok := e.Attributes[ad.Name]
		if !ok || v == nil || v == "" {
			return "", &clientError{"ATLAS-400-00-01B",
				"Required attribute " + ad.Name + " is missing for type " + e.TypeName + "."}
		}
	}
	qname, _ := e.Attributes["qualifiedName"].(string)
	if strings.TrimSpace(qname) == "" {
		return "", &clientError{"ATLAS-400-00-01B",
			"Attribute qualifiedName is required: it is the unique attribute Atlas identifies an entity by."}
	}
	return qname, nil
}

// effectiveAttributeDefs collects a type's own attributes plus every
// supertype's. The `seen` set makes a cyclic supertype graph terminate rather
// than recurse forever — Atlas rejects cycles at registration, but a store
// written by an older build should not be able to hang the server.
func (s *Service) effectiveAttributeDefs(def *typeDefBody, seen map[string]bool) ([]attributeDef, error) {
	if seen[def.Name] {
		return nil, nil
	}
	seen[def.Name] = true
	out := append([]attributeDef(nil), def.AttributeDefs...)
	for _, super := range def.SuperTypes {
		td, err := s.Store.GetTypeDefByName(super)
		if errors.Is(err, store.ErrNotFound) {
			continue // registration refused dangling parents; tolerate old data
		}
		if err != nil {
			return nil, err
		}
		var parent typeDefBody
		if err := json.Unmarshal(td.Body, &parent); err != nil {
			return nil, err
		}
		inherited, err := s.effectiveAttributeDefs(&parent, seen)
		if err != nil {
			return nil, err
		}
		out = append(out, inherited...)
	}
	return out, nil
}

func headerOf(row *store.AtlasEntityRow, generic map[string]any) entityHeader {
	h := entityHeader{GUID: row.GUID, TypeName: row.TypeName, Status: row.Status}
	if attrs, ok := generic["attributes"].(map[string]any); ok {
		h.Attributes = attrs
		if name, ok := attrs["name"].(string); ok {
			h.DisplayText = name
		} else {
			h.DisplayText = row.QualifiedName
		}
	}
	return h
}

// --- reads -------------------------------------------------------------------

func (s *Service) getEntityByGUID(w http.ResponseWriter, r *http.Request, _ *auth.Principal) {
	row, err := s.Store.GetEntity(r.PathValue("guid"))
	if errors.Is(err, store.ErrNotFound) {
		writeAtlasErr(w, http.StatusNotFound, "ATLAS-404-00-005",
			"Given instance guid "+r.PathValue("guid")+" is invalid/not found.")
		return
	}
	if err != nil {
		writeAtlasErr(w, http.StatusInternalServerError, "ATLAS-500-00-001", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, AtlasEntityWithExtInfo{Entity: row.Body})
}

func (s *Service) getEntityHeaderByGUID(w http.ResponseWriter, r *http.Request, _ *auth.Principal) {
	row, err := s.Store.GetEntity(r.PathValue("guid"))
	if errors.Is(err, store.ErrNotFound) {
		writeAtlasErr(w, http.StatusNotFound, "ATLAS-404-00-005",
			"Given instance guid "+r.PathValue("guid")+" is invalid/not found.")
		return
	}
	if err != nil {
		writeAtlasErr(w, http.StatusInternalServerError, "ATLAS-500-00-001", err.Error())
		return
	}
	var generic map[string]any
	_ = json.Unmarshal(row.Body, &generic)
	writeJSON(w, http.StatusOK, headerOf(row, generic))
}

func (s *Service) getEntitiesByGUIDs(w http.ResponseWriter, r *http.Request, _ *auth.Principal) {
	guids := r.URL.Query()["guid"]
	if len(guids) == 0 {
		writeAtlasErr(w, http.StatusBadRequest, "ATLAS-400-00-001",
			"At least one `guid` query parameter is required.")
		return
	}
	rows, err := s.Store.ListEntitiesByGUIDs(guids)
	if err != nil {
		writeAtlasErr(w, http.StatusInternalServerError, "ATLAS-500-00-001", err.Error())
		return
	}
	out := AtlasEntitiesWithExtInfo{Entities: make([]json.RawMessage, 0, len(rows))}
	for _, row := range rows {
		out.Entities = append(out.Entities, row.Body)
	}
	writeJSON(w, http.StatusOK, out)
}

// getEntityByUniqueAttr serves the `attr:qualifiedName=…` lookup. This is how
// a client finds an entity it did not create and has no GUID for, which is the
// normal case for anything discovered rather than authored.
func (s *Service) getEntityByUniqueAttr(w http.ResponseWriter, r *http.Request, _ *auth.Principal) {
	typeName := r.PathValue("typeName")
	qname := uniqueAttrValue(r)
	if qname == "" {
		writeAtlasErr(w, http.StatusBadRequest, "ATLAS-400-00-001",
			"A unique attribute is required, e.g. ?attr:qualifiedName=…")
		return
	}
	row, err := s.Store.GetEntityByUniqueAttr(typeName, qname)
	if errors.Is(err, store.ErrNotFound) {
		writeAtlasErr(w, http.StatusNotFound, "ATLAS-404-00-00A",
			"Instance "+typeName+" with unique attribute {qualifiedName="+qname+"} does not exist.")
		return
	}
	if err != nil {
		writeAtlasErr(w, http.StatusInternalServerError, "ATLAS-500-00-001", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, AtlasEntityWithExtInfo{Entity: row.Body})
}

// uniqueAttrValue pulls the value out of an `attr:<name>=<value>` query
// parameter. Only qualifiedName is a unique attribute in this increment; any
// other attr: key is ignored rather than silently treated as qualifiedName,
// which would answer a question the client did not ask.
func uniqueAttrValue(r *http.Request) string {
	return r.URL.Query().Get("attr:qualifiedName")
}

// --- deletes -----------------------------------------------------------------

func (s *Service) deleteEntityByGUID(w http.ResponseWriter, r *http.Request, _ *auth.Principal) {
	row, err := s.Store.GetEntity(r.PathValue("guid"))
	if errors.Is(err, store.ErrNotFound) {
		writeAtlasErr(w, http.StatusNotFound, "ATLAS-404-00-005",
			"Given instance guid "+r.PathValue("guid")+" is invalid/not found.")
		return
	}
	if err != nil {
		writeAtlasErr(w, http.StatusInternalServerError, "ATLAS-500-00-001", err.Error())
		return
	}
	resp, err := s.softDelete(row)
	if err != nil {
		writeAtlasErr(w, http.StatusInternalServerError, "ATLAS-500-00-001", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Service) deleteEntitiesByGUIDs(w http.ResponseWriter, r *http.Request, _ *auth.Principal) {
	rows, err := s.Store.ListEntitiesByGUIDs(r.URL.Query()["guid"])
	if err != nil {
		writeAtlasErr(w, http.StatusInternalServerError, "ATLAS-500-00-001", err.Error())
		return
	}
	out := &EntityMutationResponse{MutatedEntities: map[string][]entityHeader{}}
	for _, row := range rows {
		resp, err := s.softDelete(row)
		if err != nil {
			writeAtlasErr(w, http.StatusInternalServerError, "ATLAS-500-00-001", err.Error())
			return
		}
		out.MutatedEntities[opDelete] = append(out.MutatedEntities[opDelete], resp.MutatedEntities[opDelete]...)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Service) deleteEntityByUniqueAttr(w http.ResponseWriter, r *http.Request, _ *auth.Principal) {
	row, err := s.Store.GetEntityByUniqueAttr(r.PathValue("typeName"), uniqueAttrValue(r))
	if errors.Is(err, store.ErrNotFound) {
		writeAtlasErr(w, http.StatusNotFound, "ATLAS-404-00-00A", "Instance does not exist.")
		return
	}
	if err != nil {
		writeAtlasErr(w, http.StatusInternalServerError, "ATLAS-500-00-001", err.Error())
		return
	}
	resp, err := s.softDelete(row)
	if err != nil {
		writeAtlasErr(w, http.StatusInternalServerError, "ATLAS-500-00-001", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// softDelete marks an entity DELETED without removing it. The spec's own
// EntityStatus doc says so — "Deleted entities are not removed" — and a hard
// delete would break the lineage and audit reads that later increments add.
func (s *Service) softDelete(row *store.AtlasEntityRow) (*EntityMutationResponse, error) {
	// Status must be set on the in-memory row BEFORE PutEntity rewrites the
	// body: PutEntity persists e.Status, and leaving it ACTIVE here would
	// undo SetEntityStatus. Lineage filters on the column, so that undo
	// would keep a deleted Process in the graph.
	row.Status = statusDeleted
	if err := s.Store.SetEntityStatus(row.GUID, statusDeleted); err != nil {
		return nil, err
	}
	var generic map[string]any
	if err := json.Unmarshal(row.Body, &generic); err == nil {
		generic["status"] = statusDeleted
		if b, err := json.Marshal(generic); err == nil {
			row.Body = b
			if _, err := s.Store.PutEntity(row); err != nil {
				return nil, err
			}
		}
	}
	return &EntityMutationResponse{MutatedEntities: map[string][]entityHeader{
		opDelete: {headerOf(row, generic)},
	}}, nil
}
