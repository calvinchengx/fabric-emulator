package store

// Purview Data Map storage: Atlas type definitions and entities.
//
// The Data Map API is Apache Atlas v2 — the Azure spec's own routes are
// `/atlas/v2/…` and its TypeSpec says so ("This is Atlas API, which does not
// require api version"). So the storage model is Atlas's, not one we invented:
// a type registry keyed by name, and entities keyed by GUID with a per-type
// unique `qualifiedName`.
//
// Definitions and entities are stored as their JSON bodies rather than being
// shredded into columns. That is deliberate: Atlas types are open (an entityDef
// declares arbitrary attributeDefs, and an entity carries whatever attributes
// its type declares), so a column-per-field schema could only ever hold the
// subset we thought of, and would silently drop the rest of a round trip. The
// columns that DO exist are exactly the ones the API queries by — name, guid,
// typeName, qualifiedName, status — and each is enforced here rather than left
// to the handler.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// AtlasTypeDef is one registered type definition.
type AtlasTypeDef struct {
	GUID     string
	Name     string
	Category string // ENTITY, CLASSIFICATION, ENUM, STRUCT, RELATIONSHIP, …
	Body     json.RawMessage
	CreateAt int64
	UpdateAt int64
}

// AtlasEntityRow is one entity instance.
type AtlasEntityRow struct {
	GUID          string
	TypeName      string
	QualifiedName string
	Status        string // ACTIVE | DELETED — Atlas soft-deletes
	Body          json.RawMessage
	CreateAt      int64
	UpdateAt      int64
}

// ErrTypeNameTaken is returned when a definition name is already registered.
// Atlas refuses a duplicate rather than overwriting, and so does this.
var ErrTypeNameTaken = errors.New("type name already registered")

// CreateTypeDef registers a definition. The name is unique across ALL
// categories, not per category: Atlas resolves a type by bare name, so an
// entityDef and a classificationDef sharing one name would make `typeName`
// ambiguous at entity-create time.
func (s *Store) CreateTypeDef(td *AtlasTypeDef) error {
	if _, err := s.GetTypeDefByName(td.Name); err == nil {
		return fmt.Errorf("%w: %s", ErrTypeNameTaken, td.Name)
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	if td.GUID == "" {
		td.GUID = NewID()
	}
	td.CreateAt = s.Now()
	td.UpdateAt = td.CreateAt
	_, err := s.db.Exec(`
INSERT INTO purview_typedefs (guid, name, category, body, created_at, updated_at)
VALUES (?,?,?,?,?,?)`, td.GUID, td.Name, td.Category, string(td.Body), td.CreateAt, td.UpdateAt)
	return err
}

// UpdateTypeDef replaces a registered definition's body in place.
func (s *Store) UpdateTypeDef(td *AtlasTypeDef) error {
	td.UpdateAt = s.Now()
	res, err := s.db.Exec(`
UPDATE purview_typedefs SET category = ?, body = ?, updated_at = ? WHERE name = ?`,
		td.Category, string(td.Body), td.UpdateAt, td.Name)
	if err != nil {
		return err
	}
	return oneRow(res)
}

// GetTypeDefByName resolves a type by the name entities reference.
func (s *Store) GetTypeDefByName(name string) (*AtlasTypeDef, error) {
	return s.typeDefWhere("name = ?", name)
}

// GetTypeDefByGUID resolves a type by its assigned GUID.
func (s *Store) GetTypeDefByGUID(guid string) (*AtlasTypeDef, error) {
	return s.typeDefWhere("guid = ?", guid)
}

func (s *Store) typeDefWhere(clause string, arg any) (*AtlasTypeDef, error) {
	td := &AtlasTypeDef{}
	var body string
	err := s.db.QueryRow(`
SELECT guid, name, category, body, created_at, updated_at FROM purview_typedefs WHERE `+clause, arg).
		Scan(&td.GUID, &td.Name, &td.Category, &body, &td.CreateAt, &td.UpdateAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	td.Body = json.RawMessage(body)
	return td, err
}

// ListTypeDefs returns every definition, or those of one category when
// category is non-empty.
func (s *Store) ListTypeDefs(category string) ([]*AtlasTypeDef, error) {
	query := `SELECT guid, name, category, body, created_at, updated_at FROM purview_typedefs`
	var args []any
	if category != "" {
		query += ` WHERE category = ?`
		args = append(args, category)
	}
	query += ` ORDER BY name`
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*AtlasTypeDef
	for rows.Next() {
		td := &AtlasTypeDef{}
		var body string
		if err := rows.Scan(&td.GUID, &td.Name, &td.Category, &body, &td.CreateAt, &td.UpdateAt); err != nil {
			return nil, err
		}
		td.Body = json.RawMessage(body)
		out = append(out, td)
	}
	return out, rows.Err()
}

// DeleteTypeDef removes a definition by name.
func (s *Store) DeleteTypeDef(name string) error {
	res, err := s.db.Exec(`DELETE FROM purview_typedefs WHERE name = ?`, name)
	if err != nil {
		return err
	}
	return oneRow(res)
}

// TypeDefInUse reports whether any entity references this type. Atlas refuses
// to delete a type that instances still use, and so must we: dropping it would
// leave entities whose typeName resolves to nothing, which every later read has
// to special-case.
func (s *Store) TypeDefInUse(name string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM purview_entities WHERE type_name = ?`, name).Scan(&n)
	return n > 0, err
}

// PutEntity creates or replaces an entity. The (type_name, qualified_name) pair
// is the identity Atlas uses — `qualifiedName` is the unique attribute — so a
// second create for the same pair UPDATES rather than duplicating, which is the
// documented createOrUpdate behaviour and the reason the endpoint is named that.
//
// Returns true when the row was newly created, so the caller can report the
// mutation under CREATE or UPDATE rather than guessing.
func (s *Store) PutEntity(e *AtlasEntityRow) (created bool, err error) {
	now := s.Now()
	existing, err := s.GetEntityByUniqueAttr(e.TypeName, e.QualifiedName)
	switch {
	case err == nil:
		e.GUID = existing.GUID // identity follows qualifiedName, not the client's guid
		e.CreateAt = existing.CreateAt
		e.UpdateAt = now
		_, err = s.db.Exec(`
UPDATE purview_entities SET body = ?, status = ?, updated_at = ? WHERE guid = ?`,
			string(e.Body), e.Status, e.UpdateAt, e.GUID)
		return false, err
	case errors.Is(err, ErrNotFound):
		if e.GUID == "" {
			e.GUID = NewID()
		}
		e.CreateAt, e.UpdateAt = now, now
		_, err = s.db.Exec(`
INSERT INTO purview_entities (guid, type_name, qualified_name, status, body, created_at, updated_at)
VALUES (?,?,?,?,?,?,?)`,
			e.GUID, e.TypeName, e.QualifiedName, e.Status, string(e.Body), e.CreateAt, e.UpdateAt)
		return true, err
	default:
		return false, err
	}
}

// GetEntity fetches one entity by GUID, deleted ones included: Atlas soft-
// deletes ("Deleted entities are not removed") and a client may still read one.
func (s *Store) GetEntity(guid string) (*AtlasEntityRow, error) {
	return s.entityWhere("guid = ?", guid)
}

// GetEntityByUniqueAttr fetches by the (typeName, qualifiedName) identity.
func (s *Store) GetEntityByUniqueAttr(typeName, qualifiedName string) (*AtlasEntityRow, error) {
	e := &AtlasEntityRow{}
	var body string
	err := s.db.QueryRow(`
SELECT guid, type_name, qualified_name, status, body, created_at, updated_at
FROM purview_entities WHERE type_name = ? AND qualified_name = ?`, typeName, qualifiedName).
		Scan(&e.GUID, &e.TypeName, &e.QualifiedName, &e.Status, &body, &e.CreateAt, &e.UpdateAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	e.Body = json.RawMessage(body)
	return e, err
}

func (s *Store) entityWhere(clause string, args ...any) (*AtlasEntityRow, error) {
	e := &AtlasEntityRow{}
	var body string
	err := s.db.QueryRow(`
SELECT guid, type_name, qualified_name, status, body, created_at, updated_at
FROM purview_entities WHERE `+clause, args...).
		Scan(&e.GUID, &e.TypeName, &e.QualifiedName, &e.Status, &body, &e.CreateAt, &e.UpdateAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	e.Body = json.RawMessage(body)
	return e, err
}

// SetEntityStatus soft-deletes (or revives) an entity.
func (s *Store) SetEntityStatus(guid, status string) error {
	res, err := s.db.Exec(`UPDATE purview_entities SET status = ?, updated_at = ? WHERE guid = ?`,
		status, s.Now(), guid)
	if err != nil {
		return err
	}
	return oneRow(res)
}

// ListEntitiesByGUIDs returns the entities for the given GUIDs, skipping any
// that do not exist — the bulk read is a lookup, not an assertion.
// ListEntitiesBySuperType returns every ACTIVE entity whose type is `name` or
// descends from it, resolved through the supertype chain.
//
// Lineage needs this because it is DERIVED rather than stored: Atlas computes
// it by walking `Process` entities' inputs/outputs, and a real model subclasses
// Process (`CopyJob`, `Notebook`, …) rather than instantiating it directly. A
// query for type_name = 'Process' would therefore find the base type only, and
// return empty lineage for every model anyone actually builds — a wrong answer
// that looks like "no lineage yet" rather than like a bug.
//
// DELETED entities are excluded: Atlas soft-deletes, and a deleted process no
// longer connects the assets it used to join.
func (s *Store) ListEntitiesBySuperType(name string) ([]*AtlasEntityRow, error) {
	names, err := s.typeAndDescendants(name)
	if err != nil {
		return nil, err
	}
	out := []*AtlasEntityRow{}
	for _, tn := range names {
		rows, err := s.db.Query(`
SELECT guid, type_name, qualified_name, status, body, created_at, updated_at
FROM purview_entities WHERE type_name = ? AND status != 'DELETED'`, tn)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			e := &AtlasEntityRow{}
			var body string
			if err := rows.Scan(&e.GUID, &e.TypeName, &e.QualifiedName, &e.Status,
				&body, &e.CreateAt, &e.UpdateAt); err != nil {
				rows.Close()
				return nil, err
			}
			e.Body = json.RawMessage(body)
			out = append(out, e)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// typeAndDescendants is `name` plus every entity type reaching it through
// superTypes. Bounded by the number of registered types, and visited-guarded,
// so a cyclic registration cannot spin here even though the registration path
// should refuse one.
func (s *Store) typeAndDescendants(name string) ([]string, error) {
	defs, err := s.ListTypeDefs("ENTITY")
	if err != nil {
		return nil, err
	}
	parents := map[string][]string{}
	for _, d := range defs {
		var body struct {
			SuperTypes []string `json:"superTypes"`
		}
		_ = json.Unmarshal(d.Body, &body)
		parents[d.Name] = body.SuperTypes
	}
	var descends func(string, map[string]bool) bool
	descends = func(t string, seen map[string]bool) bool {
		if t == name {
			return true
		}
		if seen[t] {
			return false
		}
		seen[t] = true
		for _, p := range parents[t] {
			if descends(p, seen) {
				return true
			}
		}
		return false
	}
	out := []string{}
	for _, d := range defs {
		if descends(d.Name, map[string]bool{}) {
			out = append(out, d.Name)
		}
	}
	return out, nil
}

func (s *Store) ListEntitiesByGUIDs(guids []string) ([]*AtlasEntityRow, error) {
	out := make([]*AtlasEntityRow, 0, len(guids))
	for _, g := range guids {
		e, err := s.GetEntity(g)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}
