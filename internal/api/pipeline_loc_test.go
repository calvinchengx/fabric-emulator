package api

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// resolveLoc was the worst function in this file at cognitive complexity 48:
// flattening Fabric's nested shapes, reading keys out of them, refusing
// unsupported options and building the address were one 141-line body, and the
// first three were closures nothing outside could reach.
//
// Splitting them out is what makes these tests possible at all. locScopes is
// now a PURE function and locFields is constructible directly, so the error
// paths below no longer need a pipeline, a store or a job to reach — they were
// the uncovered remainder precisely because reaching them used to require one.

func TestLocScopesPrefersTheInnermostValue(t *testing.T) {
	obj := map[string]json.RawMessage{
		"workspaceId": json.RawMessage(`"outer"`),
		"datasetSettings": json.RawMessage(`{
			"typeProperties": {"location": {"workspaceId": "inner"}}
		}`),
	}
	f := locFields{scopes: locScopes(obj), resolve: keepRaw}
	got, err := f.field("workspaceId")
	if err != nil {
		t.Fatal(err)
	}
	// An explicit nested value must win: the outer one is the fallback a side
	// carries when the dataset does not name its own workspace.
	if got != "inner" {
		t.Errorf("workspaceId = %q, want the innermost scope's %q", got, "inner")
	}
}

func TestLocScopesIgnoresANestedKeyThatIsNotAnObject(t *testing.T) {
	// `datasetSettings` is a scalar here, which is malformed rather than
	// absent. descend must decline it instead of failing the whole side: a
	// Copy carrying one junk sub-object still addresses correctly through the
	// shapes that ARE well formed.
	obj := map[string]json.RawMessage{
		"itemId":          json.RawMessage(`"lh-1"`),
		"datasetSettings": json.RawMessage(`5`),
	}
	scopes := locScopes(obj)
	if len(scopes) != 1 {
		t.Fatalf("a non-object datasetSettings produced %d scopes, want only the "+
			"top level", len(scopes))
	}
	f := locFields{scopes: scopes, resolve: keepRaw}
	if got, _ := f.field("itemId"); got != "lh-1" {
		t.Errorf("itemId = %q; the well-formed part of the side must still read", got)
	}
}

func TestLocFieldsReportsACompositeAsAbsent(t *testing.T) {
	// `schema` is a COLUMN LIST in Fabric's dataset model, not a namespace.
	// fmt.Sprint would render it "[]", which is how a Copy once landed bytes at
	// `Tables/[]/bronze_customers` and reported Succeeded.
	obj := map[string]json.RawMessage{"schema": json.RawMessage(`["a","b"]`)}
	f := locFields{scopes: locScopes(obj), resolve: func(raw json.RawMessage) (any, error) {
		var v any
		_ = json.Unmarshal(raw, &v)
		return v, nil
	}}
	got, err := f.field("schema")
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("a composite resolved to %q; it must read as absent so the "+
			"caller does not address a path with it", got)
	}
}

func TestLocFieldsPropagatesAResolveFailure(t *testing.T) {
	obj := map[string]json.RawMessage{"workspaceId": json.RawMessage(`"@pipeline().x"`)}
	f := locFields{scopes: locScopes(obj), resolve: boomResolver}
	if _, err := f.field("workspaceId"); err == nil {
		t.Error("an unresolvable field was swallowed; the Copy would then address " +
			"the pipeline's own workspace as though none had been supplied")
	}
}

func TestRefuseUnsupportedCopyNamesWhatItRefused(t *testing.T) {
	for _, tc := range []struct {
		name, sideType string
		obj            string
		want           string
	}{
		{"unknown side type", "SqlServerSource", `{}`, "is not supported"},
		{"unimplemented option", "", `{"keyColumns":["id"]}`, "keyColumns"},
		{"value outside what we honour", "", `{"tableActionOption":"Upsert"}`, "tableActionOption"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var obj map[string]json.RawMessage
			if err := json.Unmarshal([]byte(tc.obj), &obj); err != nil {
				t.Fatal(err)
			}
			f := locFields{scopes: locScopes(obj), resolve: keepRaw}
			err := refuseUnsupportedCopy("source", tc.sideType, f)
			if err == nil {
				t.Fatalf("accepted a side it cannot honour; a Copy would report "+
					"Succeeded having ignored %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to name %q so the author can fix the "+
					"pipeline without bisecting it", err, tc.want)
			}
		})
	}
}

func TestRefuseUnsupportedCopyPropagatesAResolveFailure(t *testing.T) {
	// The allowed-values gate resolves each value before comparing it. An
	// unresolvable one must fail rather than compare as empty and be skipped,
	// which would wave through the very option the gate exists to check.
	obj := map[string]json.RawMessage{"tableActionOption": json.RawMessage(`"@pipeline().x"`)}
	f := locFields{scopes: locScopes(obj), resolve: boomResolver}
	if err := refuseUnsupportedCopy("sink", "", f); err == nil {
		t.Error("an unresolvable tableActionOption was skipped rather than refused")
	}
}

func TestResolveLocPropagatesFieldFailures(t *testing.T) {
	e := &pipelineExecutor{}
	for _, key := range []string{"workspaceId", "artifactId"} {
		t.Run(key, func(t *testing.T) {
			raw := json.RawMessage(fmt.Sprintf(`{"%s":"@pipeline().x"}`, key))
			resolve := func(r json.RawMessage) (any, error) {
				if strings.Contains(string(r), "@pipeline().x") {
					return nil, fmt.Errorf("unresolved")
				}
				var v any
				_ = json.Unmarshal(r, &v)
				return v, nil
			}
			if _, err := e.resolveLoc("source", raw, resolve); err == nil {
				t.Errorf("an unresolvable %s was swallowed", key)
			}
		})
	}
}

func TestResolveLocRefusesAnEmptySide(t *testing.T) {
	e := &pipelineExecutor{}
	for name, raw := range map[string]json.RawMessage{
		"absent":    nil,
		"malformed": json.RawMessage(`not json`),
	} {
		if _, err := e.resolveLoc("sink", raw, keepRaw); err == nil {
			t.Errorf("%s side was accepted", name)
		}
	}
}

func keepRaw(raw json.RawMessage) (any, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw), nil
	}
	return v, nil
}

func boomResolver(json.RawMessage) (any, error) { return nil, fmt.Errorf("unresolved") }

func TestResolveLocPropagatesASideTypeFailure(t *testing.T) {
	// The discriminator is an expression like any other. An unresolvable one
	// must fail rather than fall through as "", which is a VALID side type
	// (the emulator's own simplified shape) — so swallowing it would turn an
	// unreadable side into an accepted one.
	e := &pipelineExecutor{}
	raw := json.RawMessage(`{"type":"@pipeline().x","itemId":"lh","path":"Files/a"}`)
	if _, err := e.resolveLoc("source", raw, boomResolver); err == nil {
		t.Error("an unresolvable side type was treated as the empty type, which " +
			"this emulator accepts")
	}
}

// TestCopyPathPropagatesEveryFieldFailure.
//
// copyPath consults six keys and each read can fail. Every one of those
// `if err != nil` lines was uncovered: reaching them used to need a pipeline
// whose expression failed at exactly one key, and now needs a two-line stub —
// which is the whole reason resolveLoc's closures became a named type.
//
// A swallowed failure here is not cosmetic: copyPath returning ("", nil) reads
// as "this side names no path", and resolveLoc then reports a missing location
// rather than the expression that could not be read.
func TestCopyPathPropagatesEveryFieldFailure(t *testing.T) {
	e := &pipelineExecutor{}
	// Answers enough to reach each later key, then fails on the one under test.
	stub := func(failOn string, answers map[string]string) func(string) (string, error) {
		return func(k string) (string, error) {
			if k == failOn {
				return "", fmt.Errorf("unresolved %s", k)
			}
			return answers[k], nil
		}
	}
	for _, tc := range []struct {
		key     string
		answers map[string]string
	}{
		{"path", nil},
		{"rootFolder", nil},
		{"table", map[string]string{"rootFolder": "Tables"}},
		{"schema", map[string]string{"rootFolder": "Tables", "table": "t"}},
		{"folderPath", map[string]string{"rootFolder": "Files"}},
		{"fileName", map[string]string{"rootFolder": "Files", "folderPath": "d"}},
	} {
		t.Run(tc.key, func(t *testing.T) {
			if _, err := e.copyPath(stub(tc.key, tc.answers)); err == nil {
				t.Errorf("a failing %q read was swallowed; the Copy would report a "+
					"missing location instead of the expression it could not read", tc.key)
			}
		})
	}
}

func TestCopyPathAddressesNothingWhenNoKeyNamesAFile(t *testing.T) {
	// Neither folderPath nor fileName: the side addresses no path at all. That
	// is not an error here — resolveLoc turns it into one, with a message that
	// names both halves it needs.
	e := &pipelineExecutor{}
	got, err := e.copyPath(func(string) (string, error) { return "", nil })
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("copyPath = %q for a side naming no file, want empty", got)
	}
}
