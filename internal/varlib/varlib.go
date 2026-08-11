// Package varlib reads Microsoft Fabric Variable Library item definitions and
// resolves a variable to the value the active value set selects.
//
// A Variable Library is Fabric's own environment-abstraction mechanism: the
// library holds one declaration per variable with a default value, plus any
// number of *alternative value sets* that override some of those values. One
// value set is active per workspace, so the same consumer definition yields a
// different value in dev, test and prod without the consumer changing.
//
// The definition is a part-per-file payload:
//
//	variables.json          the declarations and their default values
//	settings.json           value-set ordering (presentation only)
//	valueSets/<name>.json   one alternative set, overriding a subset
//
// Each file carries a `$schema` naming a public schema under
// developer.microsoft.com/json-schemas/fabric/item/variableLibrary/, which is
// where the field names below are taken from rather than guessed.
//
// THE ACTIVE VALUE SET IS NOT IN THE DEFINITION. settings.json carries only
// `valueSetsOrder`; the active set is the item property `activeValueSetName`
// (PATCH /v1/workspaces/{id}/variableLibraries/{id}), which is what makes it
// per-workspace state rather than something a git branch carries. Resolve
// therefore takes the name as an argument instead of reading it from a file.
package varlib

import (
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"
)

// Variable is one declaration in variables.json. `value` is the default, used
// whenever the active value set does not override it.
type Variable struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Note  string `json:"note,omitempty"`
	Value any    `json:"value"`
}

// VariableTypes is the set a real tenant accepts, MEASURED 2026-08-11 by
// creating a VariableLibrary carrying one variable of each candidate type plus
// one deliberate nonsense type. The tenant named the nonsense one and nothing
// else:
//
//	InvalidVariableType: The variable type 'TotallyMadeUpType' is not supported.
//
// That rejection is what makes the acceptances mean something. Without a
// negative control the create could have been ignoring `type` entirely, and a
// round-trip would only have echoed our own guesses back — the emulator did
// exactly that until this list existed.
//
// `Number` is accepted by the LIBRARY even though the pipeline integration
// article says pipelines cannot consume it; the two vocabularies are separate
// (docs/48), so this is the library's list and not the pipeline's.
var VariableTypes = []string{
	"String", "Integer", "Number", "Boolean", "DateTime", "Guid",
	"ItemReference", "ConnectionReference",
}

// ValidateTypes reports the first unsupported variable type, tenant-style.
func (l *Library) ValidateTypes() error {
	for _, v := range l.Variables {
		if !slices.Contains(VariableTypes, v.Type) {
			return fmt.Errorf("the variable type %q is not supported", v.Type)
		}
	}
	return nil
}

// override is one entry of a value set's variableOverrides.
type override struct {
	Name  string `json:"name"`
	Value any    `json:"value"`
}

// Library is a parsed Variable Library definition.
type Library struct {
	Variables []Variable
	// ValueSets maps a value-set name to the overrides it declares. A value
	// set overrides a SUBSET; anything it omits keeps the default.
	ValueSets map[string][]override
	// Order is settings.json's valueSetsOrder, kept so a round-trip does not
	// silently reorder the sets. Fabric's own UI cannot reorder them, and the
	// docs tell users to edit the JSON to do it, so the order is meaningful.
	Order []string
}

// Parse reads a definition given its parts as path -> decoded content.
//
// Paths are matched on the trailing segment(s) so a definition rooted under a
// folder (as a Git-integration export is) parses the same as one whose parts
// are bare filenames.
func Parse(parts map[string][]byte) (*Library, error) {
	lib := &Library{ValueSets: map[string][]override{}}
	for p, content := range parts {
		clean := strings.TrimPrefix(path.Clean(strings.ReplaceAll(p, "\\", "/")), "./")
		base := path.Base(clean)
		switch {
		case base == "variables.json":
			var doc struct {
				Variables []Variable `json:"variables"`
			}
			if err := json.Unmarshal(content, &doc); err != nil {
				return nil, fmt.Errorf("variables.json: %w", err)
			}
			lib.Variables = doc.Variables
		case base == "settings.json":
			var doc struct {
				ValueSetsOrder []string `json:"valueSetsOrder"`
			}
			if err := json.Unmarshal(content, &doc); err != nil {
				return nil, fmt.Errorf("settings.json: %w", err)
			}
			lib.Order = doc.ValueSetsOrder
		case path.Dir(clean) == "valueSets" || strings.HasSuffix(path.Dir(clean), "/valueSets"):
			var doc struct {
				Name             string     `json:"name"`
				VariableOverride []override `json:"variableOverrides"`
			}
			if err := json.Unmarshal(content, &doc); err != nil {
				return nil, fmt.Errorf("%s: %w", clean, err)
			}
			// The set's identity is the `name` INSIDE the file, not the
			// filename. They normally agree, but activeValueSetName names the
			// value set, and only the file can say what a set is called.
			name := doc.Name
			if name == "" {
				name = strings.TrimSuffix(base, ".json")
			}
			lib.ValueSets[name] = doc.VariableOverride
		}
	}
	if lib.Variables == nil {
		return nil, fmt.Errorf("no variables.json in definition")
	}
	return lib, nil
}

// Resolve returns the effective value of every variable under the named active
// value set, and the declared type of each.
//
// An UNKNOWN value-set name resolves to the defaults rather than failing. That
// is deliberate and it is the one lenient rule here: the library's own default
// values are themselves a value set, and it does not appear under valueSets/ —
// only *alternative* sets get a file. So a name that matches no file is the
// ordinary case of "the default set is active", indistinguishable from a typo
// without a captured answer from real Fabric. Erring towards the declared
// defaults returns a value the author wrote; erring towards failure would
// break every library that has no alternative sets at all.
func (l *Library) Resolve(activeValueSet string) map[string]Variable {
	out := make(map[string]Variable, len(l.Variables))
	for _, v := range l.Variables {
		out[v.Name] = v
	}
	for _, o := range l.ValueSets[activeValueSet] {
		v, ok := out[o.Name]
		if !ok {
			// An override for a variable that no longer exists. Dropping it is
			// right: the declaration list is authoritative, and inventing a
			// variable from an override would let a stale value set resurrect
			// a deleted variable.
			continue
		}
		v.Value = o.Value
		out[o.Name] = v
	}
	return out
}

// Lookup returns one variable's effective value under the active value set.
func (l *Library) Lookup(activeValueSet, name string) (Variable, bool) {
	v, ok := l.Resolve(activeValueSet)[name]
	return v, ok
}
