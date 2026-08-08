package purview

// The base type hierarchy every Purview account already has.
//
// A real Data Map is never empty: Atlas ships `Referenceable`, `Asset`,
// `DataSet` and `Process`, and Purview seeds them before you touch it. A client
// that creates its own type with `superTypes: ["DataSet"]` — which is what the
// documentation, the SDK samples and pyapacheatlas's own quickstart all do —
// fails immediately against an empty registry.
//
// Seeding them is therefore fidelity, not convenience. And it is what makes the
// `qualifiedName` rule real rather than nominal: the attribute is declared ONCE
// here, on `Referenceable`, and every type inherits it. An entity type defined
// by a client that never mentions qualifiedName still requires one, because its
// supertype does — which is exactly how Purview behaves, and the reason
// validateAttributes walks the supertype chain instead of one type's own list.

import (
	"encoding/json"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// baseTypes are seeded at startup if absent. Deliberately the four Atlas base
// types and nothing more: Purview also seeds ~200 source-specific types
// (azure_sql_table, azure_datalake_gen2_path, …) whose attribute sets are not
// published in the REST spec, and inventing them would be a fabrication the
// parity row could not honestly describe.
var baseTypes = []struct {
	name string
	body map[string]any
}{
	{"Referenceable", map[string]any{
		"category":    "ENTITY",
		"name":        "Referenceable",
		"description": "Referenceable: the root type, carrying the unique attribute qualifiedName.",
		"typeVersion": "1.0",
		"serviceType": "atlas_core",
		"attributeDefs": []map[string]any{{
			"name":           "qualifiedName",
			"typeName":       "string",
			"isOptional":     false,
			"isUnique":       true,
			"isIndexable":    true,
			"cardinality":    "SINGLE",
			"valuesMinCount": 1,
			"valuesMaxCount": 1,
		}},
	}},
	{"Asset", map[string]any{
		"category":    "ENTITY",
		"name":        "Asset",
		"description": "Asset: a named, describable thing.",
		"typeVersion": "1.0",
		"serviceType": "atlas_core",
		"superTypes":  []string{"Referenceable"},
		"attributeDefs": []map[string]any{
			{"name": "name", "typeName": "string", "isOptional": false, "cardinality": "SINGLE",
				"isIndexable": true, "valuesMinCount": 1, "valuesMaxCount": 1},
			{"name": "description", "typeName": "string", "isOptional": true, "cardinality": "SINGLE"},
			{"name": "owner", "typeName": "string", "isOptional": true, "cardinality": "SINGLE"},
		},
	}},
	{"DataSet", map[string]any{
		"category":    "ENTITY",
		"name":        "DataSet",
		"description": "DataSet: data an asset holds. The supertype most source types extend.",
		"typeVersion": "1.0",
		"serviceType": "atlas_core",
		"superTypes":  []string{"Asset"},
	}},
	{"Process", map[string]any{
		"category":    "ENTITY",
		"name":        "Process",
		"description": "Process: something that reads inputs and writes outputs. Lineage hangs off this.",
		"typeVersion": "1.0",
		"serviceType": "atlas_core",
		"superTypes":  []string{"Asset"},
	}},
}

// Seed registers the base types that are missing. Idempotent, and ordered so a
// supertype is always registered before the types that extend it — the
// registration path refuses a dangling parent, so the order is load-bearing
// rather than tidy.
func (s *Service) Seed() error {
	for _, bt := range baseTypes {
		if _, err := s.Store.GetTypeDefByName(bt.name); err == nil {
			continue
		}
		body, err := json.Marshal(bt.body)
		if err != nil {
			return err
		}
		td := &store.AtlasTypeDef{Name: bt.name, Category: "ENTITY", GUID: store.NewID()}
		var generic map[string]any
		if err := json.Unmarshal(body, &generic); err != nil {
			return err
		}
		generic["guid"] = td.GUID
		if td.Body, err = json.Marshal(generic); err != nil {
			return err
		}
		if err := s.Store.CreateTypeDef(td); err != nil {
			return err
		}
	}
	return nil
}
