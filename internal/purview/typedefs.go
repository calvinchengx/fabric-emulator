package purview

// The Atlas type system.
//
// This is the half that makes the entity half worth anything. Without a
// registry, `typeName` is a free-text label and every entity is valid — which
// is precisely the permissive default that turns each gap into a silent
// success. With one, an entity naming a type nobody defined is refused, and a
// definition's `attributeDefs` decide which attributes an instance must carry.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// AtlasTypesDef is the envelope every typedef route takes and returns —
// definitions grouped by category, exactly as the spec models it.
type AtlasTypesDef struct {
	EntityDefs           []json.RawMessage `json:"entityDefs,omitempty"`
	ClassificationDefs   []json.RawMessage `json:"classificationDefs,omitempty"`
	EnumDefs             []json.RawMessage `json:"enumDefs,omitempty"`
	StructDefs           []json.RawMessage `json:"structDefs,omitempty"`
	RelationshipDefs     []json.RawMessage `json:"relationshipDefs,omitempty"`
	BusinessMetadataDefs []json.RawMessage `json:"businessMetadataDefs,omitempty"`
	TermTemplateDefs     []json.RawMessage `json:"termTemplateDefs,omitempty"`
}

// attributeDef is the subset of AtlasAttributeDef this increment enforces.
// The rest of the definition is round-tripped verbatim rather than dropped:
// Atlas types are open, and a field we do not read is still a field the client
// sent and expects back.
type attributeDef struct {
	Name       string `json:"name"`
	TypeName   string `json:"typeName"`
	IsOptional *bool  `json:"isOptional"`
	IsUnique   bool   `json:"isUnique"`
}

// typeDefBody is the part of any definition this code reasons about.
type typeDefBody struct {
	Name          string         `json:"name"`
	Category      string         `json:"category"`
	GUID          string         `json:"guid"`
	SuperTypes    []string       `json:"superTypes"`
	AttributeDefs []attributeDef `json:"attributeDefs"`
}

// byCategory returns the slice of an AtlasTypesDef for a category, so a route
// and a store row agree on where a definition belongs. Pointer-to-slice so the
// caller can append.
func (t *AtlasTypesDef) byCategory(category string) *[]json.RawMessage {
	switch category {
	case "ENTITY":
		return &t.EntityDefs
	case "CLASSIFICATION":
		return &t.ClassificationDefs
	case "ENUM":
		return &t.EnumDefs
	case "STRUCT":
		return &t.StructDefs
	case "RELATIONSHIP":
		return &t.RelationshipDefs
	case "BUSINESS_METADATA":
		return &t.BusinessMetadataDefs
	case "TERM_TEMPLATE":
		return &t.TermTemplateDefs
	}
	return nil
}

// each walks every definition in the envelope with the category its position
// implies. The category travels with the FIELD, not with a `category` value
// inside the body: a client may omit it (pyapacheatlas does), and inferring it
// from the envelope is both what Atlas does and the only thing that works.
func (t *AtlasTypesDef) each(fn func(category string, raw json.RawMessage) error) error {
	for _, group := range []struct {
		category string
		defs     []json.RawMessage
	}{
		{"ENTITY", t.EntityDefs},
		{"CLASSIFICATION", t.ClassificationDefs},
		{"ENUM", t.EnumDefs},
		{"STRUCT", t.StructDefs},
		{"RELATIONSHIP", t.RelationshipDefs},
		{"BUSINESS_METADATA", t.BusinessMetadataDefs},
		{"TERM_TEMPLATE", t.TermTemplateDefs},
	} {
		for _, raw := range group.defs {
			if err := fn(group.category, raw); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) createTypeDefs(w http.ResponseWriter, r *http.Request, _ *auth.Principal) {
	var in AtlasTypesDef
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeAtlasErr(w, http.StatusBadRequest, "ATLAS-400-00-001", "Malformed request body: "+err.Error())
		return
	}
	var out AtlasTypesDef
	err := in.each(func(category string, raw json.RawMessage) error {
		var body typeDefBody
		if err := json.Unmarshal(raw, &body); err != nil {
			return &clientError{"ATLAS-400-00-001", "Malformed type definition: " + err.Error()}
		}
		if strings.TrimSpace(body.Name) == "" {
			return &clientError{"ATLAS-400-00-00B", "A type definition requires a name."}
		}
		// A superType must already exist. Atlas resolves inheritance at
		// registration; accepting a dangling parent would defer the failure to
		// every later entity create, where the cause is no longer visible.
		for _, super := range body.SuperTypes {
			if _, err := s.Store.GetTypeDefByName(super); err != nil {
				return &clientError{"ATLAS-404-00-001",
					"Referenced supertype " + super + " does not exist."}
			}
		}
		stored, err := s.registerTypeDef(category, body, raw)
		if err != nil {
			return err
		}
		if slot := out.byCategory(category); slot != nil {
			*slot = append(*slot, stored)
		}
		return nil
	})
	if err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// registerTypeDef stores one definition and returns the body as persisted —
// with the server-assigned guid and category folded in, which is what the
// client gets back and what a subsequent read must return.
func (s *Service) registerTypeDef(category string, body typeDefBody, raw json.RawMessage) (json.RawMessage, error) {
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, &clientError{"ATLAS-400-00-001", err.Error()}
	}
	td := &store.AtlasTypeDef{Name: body.Name, Category: category, GUID: body.GUID}
	if td.GUID == "" {
		td.GUID = store.NewID()
	}
	generic["guid"] = td.GUID
	generic["category"] = category
	enriched, err := json.Marshal(generic)
	if err != nil {
		return nil, err
	}
	td.Body = enriched
	if err := s.Store.CreateTypeDef(td); err != nil {
		if errors.Is(err, store.ErrTypeNameTaken) {
			return nil, &clientError{"ATLAS-409-00-001",
				"Given type " + body.Name + " already exists."}
		}
		return nil, err
	}
	return enriched, nil
}

func (s *Service) updateTypeDefs(w http.ResponseWriter, r *http.Request, _ *auth.Principal) {
	var in AtlasTypesDef
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeAtlasErr(w, http.StatusBadRequest, "ATLAS-400-00-001", "Malformed request body: "+err.Error())
		return
	}
	var out AtlasTypesDef
	err := in.each(func(category string, raw json.RawMessage) error {
		var body typeDefBody
		if err := json.Unmarshal(raw, &body); err != nil {
			return &clientError{"ATLAS-400-00-001", err.Error()}
		}
		existing, err := s.Store.GetTypeDefByName(body.Name)
		if errors.Is(err, store.ErrNotFound) {
			return &clientError{"ATLAS-404-00-001",
				"Given typename " + body.Name + " was invalid."}
		} else if err != nil {
			return err
		}
		var generic map[string]any
		if err := json.Unmarshal(raw, &generic); err != nil {
			return &clientError{"ATLAS-400-00-001", err.Error()}
		}
		// The GUID is the server's, not the client's: an update that could
		// change it would break every reference already handed out.
		generic["guid"] = existing.GUID
		generic["category"] = existing.Category
		enriched, err := json.Marshal(generic)
		if err != nil {
			return err
		}
		if err := s.Store.UpdateTypeDef(&store.AtlasTypeDef{
			Name: body.Name, Category: existing.Category, Body: enriched}); err != nil {
			return err
		}
		if slot := out.byCategory(existing.Category); slot != nil {
			*slot = append(*slot, enriched)
		}
		return nil
	})
	if err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Service) deleteTypeDefs(w http.ResponseWriter, r *http.Request, _ *auth.Principal) {
	var in AtlasTypesDef
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeAtlasErr(w, http.StatusBadRequest, "ATLAS-400-00-001", "Malformed request body: "+err.Error())
		return
	}
	err := in.each(func(_ string, raw json.RawMessage) error {
		var body typeDefBody
		if err := json.Unmarshal(raw, &body); err != nil {
			return &clientError{"ATLAS-400-00-001", err.Error()}
		}
		return s.removeTypeDef(body.Name)
	})
	if err != nil {
		writeClientError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) deleteTypeDefByName(w http.ResponseWriter, r *http.Request, _ *auth.Principal) {
	if err := s.removeTypeDef(r.PathValue("name")); err != nil {
		writeClientError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// removeTypeDef refuses to drop a type that instances still reference.
// Deleting it would leave entities whose typeName resolves to nothing — every
// later read would have to special-case them, and the type system would be
// enforcing a rule it had already broken itself.
func (s *Service) removeTypeDef(name string) error {
	if _, err := s.Store.GetTypeDefByName(name); errors.Is(err, store.ErrNotFound) {
		return &clientError{"ATLAS-404-00-001", "Given typename " + name + " was invalid."}
	} else if err != nil {
		return err
	}
	inUse, err := s.Store.TypeDefInUse(name)
	if err != nil {
		return err
	}
	if inUse {
		return &clientError{"ATLAS-409-00-001",
			"Given type " + name + " has references and cannot be deleted."}
	}
	return s.Store.DeleteTypeDef(name)
}

func (s *Service) listTypeDefs(w http.ResponseWriter, r *http.Request, _ *auth.Principal) {
	// `type` narrows to one category, as the spec's query parameter does.
	category := strings.ToUpper(r.URL.Query().Get("type"))
	defs, err := s.Store.ListTypeDefs(category)
	if err != nil {
		writeAtlasErr(w, http.StatusInternalServerError, "ATLAS-500-00-001", err.Error())
		return
	}
	var out AtlasTypesDef
	for _, td := range defs {
		if slot := out.byCategory(td.Category); slot != nil {
			*slot = append(*slot, td.Body)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// typeDefHeader is the lightweight listing shape: guid, name, category.
type typeDefHeader struct {
	GUID     string `json:"guid"`
	Name     string `json:"name"`
	Category string `json:"category"`
}

func (s *Service) listTypeDefHeaders(w http.ResponseWriter, r *http.Request, _ *auth.Principal) {
	defs, err := s.Store.ListTypeDefs(strings.ToUpper(r.URL.Query().Get("type")))
	if err != nil {
		writeAtlasErr(w, http.StatusInternalServerError, "ATLAS-500-00-001", err.Error())
		return
	}
	out := make([]typeDefHeader, 0, len(defs))
	for _, td := range defs {
		out = append(out, typeDefHeader{GUID: td.GUID, Name: td.Name, Category: td.Category})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Service) getTypeDefByName(w http.ResponseWriter, r *http.Request, _ *auth.Principal) {
	s.serveTypeDef(w, func() (*store.AtlasTypeDef, error) {
		return s.Store.GetTypeDefByName(r.PathValue("name"))
	}, "")
}

func (s *Service) getTypeDefByGUID(w http.ResponseWriter, r *http.Request, _ *auth.Principal) {
	s.serveTypeDef(w, func() (*store.AtlasTypeDef, error) {
		return s.Store.GetTypeDefByGUID(r.PathValue("guid"))
	}, "")
}

// getTypeDefOfCategoryByName serves /types/{category}def/name/{name}. A name
// that resolves to a DIFFERENT category is a 404, not a cast: asking for an
// entitydef and getting a classification back would let a client build on a
// type that cannot hold what it is about to send.
func (s *Service) getTypeDefOfCategoryByName(category string) handler {
	return func(w http.ResponseWriter, r *http.Request, _ *auth.Principal) {
		s.serveTypeDef(w, func() (*store.AtlasTypeDef, error) {
			return s.Store.GetTypeDefByName(r.PathValue("name"))
		}, category)
	}
}

func (s *Service) getTypeDefOfCategoryByGUID(category string) handler {
	return func(w http.ResponseWriter, r *http.Request, _ *auth.Principal) {
		s.serveTypeDef(w, func() (*store.AtlasTypeDef, error) {
			return s.Store.GetTypeDefByGUID(r.PathValue("guid"))
		}, category)
	}
}

func (s *Service) serveTypeDef(w http.ResponseWriter, lookup func() (*store.AtlasTypeDef, error), wantCategory string) {
	td, err := lookup()
	if errors.Is(err, store.ErrNotFound) {
		writeAtlasErr(w, http.StatusNotFound, "ATLAS-404-00-001", "Given typename was invalid.")
		return
	}
	if err != nil {
		writeAtlasErr(w, http.StatusInternalServerError, "ATLAS-500-00-001", err.Error())
		return
	}
	if wantCategory != "" && td.Category != wantCategory {
		writeAtlasErr(w, http.StatusNotFound, "ATLAS-404-00-001",
			"Given typename "+td.Name+" is a "+td.Category+", not a "+wantCategory+".")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(td.Body)
}

// clientError carries an Atlas error code with the status it implies, so a
// handler can return one error value rather than writing a response mid-loop.
type clientError struct{ code, message string }

func (e *clientError) Error() string { return e.code + ": " + e.message }

// statusOf reads the HTTP status out of the Atlas code (`ATLAS-<status>-…`),
// so the code and the status can never disagree — they are one fact.
func (e *clientError) status() int {
	parts := strings.Split(e.code, "-")
	if len(parts) > 1 {
		switch parts[1] {
		case "400":
			return http.StatusBadRequest
		case "404":
			return http.StatusNotFound
		case "409":
			return http.StatusConflict
		}
	}
	return http.StatusBadRequest
}

func writeClientError(w http.ResponseWriter, err error) {
	var ce *clientError
	if errors.As(err, &ce) {
		writeAtlasErr(w, ce.status(), ce.code, ce.message)
		return
	}
	writeAtlasErr(w, http.StatusInternalServerError, "ATLAS-500-00-001", err.Error())
}
