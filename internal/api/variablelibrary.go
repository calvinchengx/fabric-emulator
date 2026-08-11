package api

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/pipeline"
	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/calvinchengx/fabric-emulator/internal/varlib"
)

// propActiveValueSet is the Variable Library's active value set, stored as an
// item property because that is what it is on real Fabric: `activeValueSetName`
// under the item's `properties`, set by PATCH on the item, NOT part of the
// definition. Keeping it out of the definition is the whole point — it is what
// lets one git branch deploy to dev and prod and resolve differently.
const propActiveValueSet = "activeValueSetName"

// libraryDefinition loads and parses a Variable Library item's definition.
func (a *API) libraryDefinition(itemID string) (*varlib.Library, error) {
	parts, err := a.Store.GetDefinition(itemID)
	if err != nil {
		return nil, err
	}
	files := make(map[string][]byte, len(parts))
	for _, p := range parts {
		raw, err := base64.StdEncoding.DecodeString(p.Payload)
		if err != nil {
			return nil, fmt.Errorf("part %q: %w", p.Path, err)
		}
		files[p.Path] = raw
	}
	return varlib.Parse(files)
}

// findLibrary returns the workspace's Variable Library with the given display
// name. Fabric documents the library name as NOT case sensitive, so the match
// is too — a pipeline that says `myLib` must find `MyLib`.
func (a *API) findLibrary(wid, name string) (*store.Item, error) {
	items, err := a.Store.ListItems(wid, "VariableLibrary")
	if err != nil {
		return nil, err
	}
	for i := range items {
		if strings.EqualFold(items[i].DisplayName, name) {
			return items[i], nil
		}
	}
	return nil, fmt.Errorf("no variable library named %q in this workspace", name)
}

// resolveLibraryVariables turns a pipeline's `libraryVariables` declarations
// into the values `@pipeline().libraryVariables.<alias>` reads, keyed by alias.
//
// Every failure here is returned rather than skipped. A reference that cannot
// be resolved is a broken definition, and the alternative — resolving it to
// blank — is precisely the failure mode this whole feature exists to prevent:
// a path that silently becomes "" and writes to the wrong place.
func (a *API) resolveLibraryVariables(wid string, refs map[string]pipeline.LibraryVariableRef) (map[string]any, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	// Cache per library name: a pipeline typically references several
	// variables from one library, and each lookup would otherwise re-read and
	// re-parse the same definition.
	type resolved struct {
		vars map[string]varlib.Variable
	}
	cache := map[string]*resolved{}

	out := make(map[string]any, len(refs))
	for alias, ref := range refs {
		key := strings.ToLower(ref.LibraryName)
		got, ok := cache[key]
		if !ok {
			it, err := a.findLibrary(wid, ref.LibraryName)
			if err != nil {
				return nil, fmt.Errorf("library variable %q: %w", alias, err)
			}
			lib, err := a.libraryDefinition(it.ID)
			if err != nil {
				return nil, fmt.Errorf("library variable %q: library %q: %w", alias, ref.LibraryName, err)
			}
			props, err := a.Store.ItemProperties(it.ID)
			if err != nil {
				return nil, fmt.Errorf("library variable %q: %w", alias, err)
			}
			got = &resolved{vars: lib.Resolve(props[propActiveValueSet])}
			cache[key] = got
		}
		v, ok := got.vars[ref.VariableName]
		if !ok {
			return nil, fmt.Errorf("library variable %q: library %q has no variable %q",
				alias, ref.LibraryName, ref.VariableName)
		}
		out[alias] = v.Value
	}
	return out, nil
}

// definitionRejection reports the error code and message a real tenant answers
// when a definition is structurally fine but semantically wrong, or "" to
// accept.
//
// MEASURED 2026-08-11. Creating a VariableLibrary on a trial tenant, two
// separate rejections came back that the emulator had no opinion about:
//
//	InvalidVariableType
//	  The variable type 'TotallyMadeUpType' is not supported.
//
//	InvalidContent (issue: InvalidValueOrTypeMismatch)
//	  Item content cannot be used (ReferencedEntityNotFoundOrAccessDenied)
//	  — a ConnectionReference naming a connection id that does not exist.
//
// Both are the direction that ships: the emulator accepted either payload
// silently, so a library written against it deploys and fails. The second one
// matters more than it looks, because a ConnectionReference's value is a GUID
// (docs/48) — promoting a library between workspaces carries an id that must
// exist on the far side, and nothing local said so.
func (a *API) definitionRejection(itemType string, parts []store.DefinitionPart) (string, string) {
	if itemType != "VariableLibrary" || len(parts) == 0 {
		return "", ""
	}
	files := make(map[string][]byte, len(parts))
	for _, p := range parts {
		raw, err := base64.StdEncoding.DecodeString(p.Payload)
		if err != nil {
			// Not this check's business: an undecodable part is caught where
			// definitions are read, and reporting it as a variable-type problem
			// would send the reader to the wrong file.
			return "", ""
		}
		files[p.Path] = raw
	}
	lib, err := varlib.Parse(files)
	if err != nil {
		return "", ""
	}
	if err := lib.ValidateTypes(); err != nil {
		return "InvalidVariableType", capitalise(err.Error()) + "."
	}
	for _, v := range lib.Variables {
		if v.Type != "ConnectionReference" {
			continue
		}
		val, _ := v.Value.(map[string]any)
		id, _ := val["connectionId"].(string)
		if _, err := a.Store.GetConnection(id); err != nil {
			return "InvalidContent",
				"Item content cannot be used (ReferencedEntityNotFoundOrAccessDenied)."
		}
	}
	return "", ""
}

// capitalise upper-cases the first letter, so a Go error reads as a sentence.
func capitalise(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
