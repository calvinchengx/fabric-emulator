package pipeline

import (
	"encoding/json"
	"testing"
)

// The expression form and the declaration shape below were both captured from
// a live Fabric tenant on 2026-08-10 (the designer wrote them, they were read
// back through View > Edit JSON code). Notably the lookup key is the
// declaration's ALIAS — `<libraryName>_<variableName>` by default — and not
// the variable's own name. See docs/48-variable-libraries.md.

func TestLibraryVariableResolvesByAlias(t *testing.T) {
	ctx := &evalContext{LibraryVariables: map[string]value{
		"envLib_bronzePath": "Files/bronze",
		"envLib_batchSize":  float64(100),
	}}
	got, err := evalString("@pipeline().libraryVariables.envLib_bronzePath", ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != "Files/bronze" {
		t.Errorf("= %v, want Files/bronze", got)
	}
	// Interpolated into surrounding text, the way a path is actually built.
	got, err = evalString("@{pipeline().libraryVariables.envLib_bronzePath}/orders", ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != "Files/bronze/orders" {
		t.Errorf("interpolated = %v", got)
	}
}

// An alias is free text in the designer, so it is used exactly as given and
// never reconstructed from libraryName + variableName. Resolving the VARIABLE
// name must fail when the alias differs — this is the precise mistake that
// made the first API-only probe fail against real Fabric.
func TestLibraryVariableDoesNotResolveByVariableName(t *testing.T) {
	ctx := &evalContext{LibraryVariables: map[string]value{"envLib_bronzePath": "Files/bronze"}}
	if _, err := evalString("@pipeline().libraryVariables.bronzePath", ctx); err == nil {
		t.Fatal("resolving by variable name succeeded; the key is the alias")
	}
}

// Real Fabric's failure for an undeclared reference names the property and
// then lists the available ones — and that list comes back EMPTY, which means
// it did a member lookup against an empty object rather than failing to find
// `libraryVariables`. So the object must be present even when the pipeline
// declares none, and the failure must land on the member.
func TestLibraryVariablesObjectExistsWhenNoneDeclared(t *testing.T) {
	ctx := &evalContext{}
	obj, err := evalString("@pipeline().libraryVariables", ctx)
	if err != nil {
		t.Fatalf("pipeline().libraryVariables absent with no declarations: %v", err)
	}
	m, ok := obj.(map[string]value)
	if !ok || len(m) != 0 {
		t.Fatalf("= %#v, want an empty object", obj)
	}
	if _, err := evalString("@pipeline().libraryVariables.anything", ctx); err == nil {
		t.Fatal("an undeclared alias resolved instead of failing")
	}
}

// A renamed alias resolves under the name the author gave it.
func TestLibraryVariableCustomAlias(t *testing.T) {
	ctx := &evalContext{LibraryVariables: map[string]value{"bronze": "Files/bronze"}}
	got, err := evalString("@pipeline().libraryVariables.bronze", ctx)
	if err != nil || got != "Files/bronze" {
		t.Fatalf("= %v, %v", got, err)
	}
}

// The declaration block parses off the definition, keyed by alias, binding by
// NAME with no GUID anywhere — which is what makes a pipeline that uses
// library variables portable across workspaces unchanged.
func TestParseLibraryVariableDeclarations(t *testing.T) {
	def := []byte(`{"properties":{
		"activities":[],
		"variables":{"probe":{"type":"String"}},
		"libraryVariables":{
			"emuProbeVarLib_bronzePath":{
				"type":"String",
				"variableName":"bronzePath",
				"libraryName":"emuProbeVarLib"
			}
		}
	}}`)
	p, err := Parse(def)
	if err != nil {
		t.Fatal(err)
	}
	refs := p.LibraryVariableRefs()
	ref, ok := refs["emuProbeVarLib_bronzePath"]
	if !ok {
		t.Fatalf("declaration not keyed by alias: %+v", refs)
	}
	if ref.VariableName != "bronzePath" || ref.LibraryName != "emuProbeVarLib" || ref.Type != "String" {
		t.Errorf("ref = %+v", ref)
	}
	// Nothing GUID-shaped is carried, so nothing needs rewriting on deploy.
	blob, _ := json.Marshal(ref)
	var fields map[string]any
	_ = json.Unmarshal(blob, &fields)
	for k := range fields {
		switch k {
		case "type", "variableName", "libraryName":
		default:
			t.Errorf("unexpected field %q in the declaration", k)
		}
	}
}

// The declaration's `type` is the PIPELINE vocabulary, and two of its values
// are counter-intuitive enough to pin. Both captured from a live tenant
// 2026-08-10, from the saved JSON rather than the designer's grid — the grid
// DISPLAYS "ItemReference" for the third one while the wire says "Object", so
// the label and the wire name genuinely differ. See docs/48-variable-libraries.md.
func TestLibraryVariableDeclaredTypesAsCaptured(t *testing.T) {
	def := []byte(`{"properties":{"activities":[],"libraryVariables":{
		"emuProbeVarLib_bronzePath":{"type":"String","variableName":"bronzePath","libraryName":"emuProbeVarLib"},
		"emuProbeVarLib_runId":{"type":"String","variableName":"runId","libraryName":"emuProbeVarLib"},
		"emuProbeVarLib_silverNotebook":{"type":"Object","variableName":"silverNotebook","libraryName":"emuProbeVarLib"}
	}}}`)
	p, err := Parse(def)
	if err != nil {
		t.Fatal(err)
	}
	refs := p.LibraryVariableRefs()
	for alias, want := range map[string]string{
		"emuProbeVarLib_bronzePath":     "String", // library String
		"emuProbeVarLib_runId":          "String", // library Guid, NOT "Guid"
		"emuProbeVarLib_silverNotebook": "Object", // library Item reference, NOT "ItemReference"
	} {
		if got := refs[alias].Type; got != want {
			t.Errorf("%s type = %q, want %q", alias, got, want)
		}
	}
}

// A reference-typed variable resolves to an object, and its sub-properties are
// reachable through further member access. The emulator does not synthesise
// those sub-properties — whatever the library holds is what the expression
// sees — so this pins the plumbing, NOT any claim about what real Fabric
// stores for an item reference, which is not captured.
func TestLibraryVariableObjectValuePassesThrough(t *testing.T) {
	ctx := &evalContext{LibraryVariables: map[string]value{
		"envLib_silverNotebook": map[string]value{"itemId": "abc-123", "workspaceId": "ws-9"},
	}}
	got, err := evalString("@pipeline().libraryVariables.envLib_silverNotebook.itemId", ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != "abc-123" {
		t.Errorf("= %v, want abc-123", got)
	}
	// A sub-property the library does not carry must fail rather than blank.
	if _, err := evalString("@pipeline().libraryVariables.envLib_silverNotebook.nope", ctx); err == nil {
		t.Error("a missing sub-property resolved instead of failing")
	}
}

func TestPipelineWithoutLibraryVariablesHasNoRefs(t *testing.T) {
	p, err := Parse([]byte(`{"properties":{"activities":[]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.LibraryVariableRefs()) != 0 {
		t.Errorf("refs = %+v, want none", p.LibraryVariableRefs())
	}
}

// End to end through the interpreter: a resolved library variable drives real
// control flow, and the run reports the branch that the value selected.
func TestRunUsesLibraryVariableInControlFlow(t *testing.T) {
	def := []byte(`{"properties":{
		"libraryVariables":{"envLib_bronzePath":{"type":"String","variableName":"bronzePath","libraryName":"envLib"}},
		"activities":[
			{"name":"Check","type":"IfCondition","typeProperties":{
				"expression":{"value":"@equals(pipeline().libraryVariables.envLib_bronzePath,'Files/bronze-prod')","type":"Expression"},
				"ifTrueActivities":[{"name":"Prod","type":"Wait","typeProperties":{"waitTimeInSeconds":0}}],
				"ifFalseActivities":[{"name":"NotProd","type":"Wait","typeProperties":{"waitTimeInSeconds":0}}]
			}}
		]}}`)
	p, err := Parse(def)
	if err != nil {
		t.Fatal(err)
	}
	ran := func(vals map[string]value) map[string]string {
		res := p.RunWith(nil, nil, Options{LibraryVariables: vals})
		if res.Status != StatusSucceeded {
			t.Fatalf("run failed: %s", res.Error)
		}
		out := map[string]string{}
		for _, a := range res.Activities {
			out[a.Name] = a.Status
		}
		return out
	}
	if got := ran(map[string]value{"envLib_bronzePath": "Files/bronze-prod"}); got["Prod"] != StatusSucceeded {
		t.Errorf("prod value took the wrong branch: %+v", got)
	}
	if got := ran(map[string]value{"envLib_bronzePath": "Files/bronze"}); got["NotProd"] != StatusSucceeded {
		t.Errorf("default value took the wrong branch: %+v", got)
	}
}
