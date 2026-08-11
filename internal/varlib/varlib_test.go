package varlib

import "testing"

// The shapes below are the ones a real tenant round-trips, captured 2026-08-10
// against workspace fd6cc69d rather than invented: `variables` entries are
// {name,type,value,note?}, a value set is {name,variableOverrides:[{name,value}]},
// and settings.json carries valueSetsOrder and NOTHING about which set is
// active. See docs/48-variable-libraries.md.
func fixture() map[string][]byte {
	return map[string][]byte{
		"variables.json": []byte(`{
			"$schema": "https://developer.microsoft.com/json-schemas/fabric/item/variableLibrary/definition/variables/1.0.0/schema.json",
			"variables": [
				{"name": "bronzePath", "type": "String", "value": "Files/bronze", "note": "env-invariant relative path"},
				{"name": "batchSize", "type": "Integer", "value": 100},
				{"name": "strict", "type": "Boolean", "value": false}
			]
		}`),
		"settings.json": []byte(`{
			"$schema": "https://developer.microsoft.com/json-schemas/fabric/item/variableLibrary/definition/settings/1.0.0/schema.json",
			"valueSetsOrder": ["prod", "test"]
		}`),
		"valueSets/prod.json": []byte(`{
			"$schema": "https://developer.microsoft.com/json-schemas/fabric/item/variableLibrary/definition/valueSet/1.0.0/schema.json",
			"name": "prod",
			"variableOverrides": [
				{"name": "bronzePath", "value": "Files/bronze-prod"},
				{"name": "strict", "value": true}
			]
		}`),
	}
}

func TestParseAndResolveDefaults(t *testing.T) {
	lib, err := Parse(fixture())
	if err != nil {
		t.Fatal(err)
	}
	if len(lib.Variables) != 3 {
		t.Fatalf("parsed %d variables, want 3", len(lib.Variables))
	}
	if got := lib.Order; len(got) != 2 || got[0] != "prod" {
		t.Errorf("valueSetsOrder = %v", got)
	}
	// No active set named: every variable keeps its declared default.
	got := lib.Resolve("")
	if got["bronzePath"].Value != "Files/bronze" {
		t.Errorf("bronzePath = %v, want the default", got["bronzePath"].Value)
	}
	if got["batchSize"].Value != float64(100) {
		t.Errorf("batchSize = %v (%T), want 100", got["batchSize"].Value, got["batchSize"].Value)
	}
	if got["strict"].Value != false {
		t.Errorf("strict = %v, want false", got["strict"].Value)
	}
	if got["bronzePath"].Note != "env-invariant relative path" {
		t.Errorf("note lost: %q", got["bronzePath"].Note)
	}
}

// The environment switch itself: the SAME definition yields different values
// under a different active value set, and a variable the set does not mention
// keeps its default. That partial-override behaviour is the reason a value set
// is not simply a second copy of the variable list.
func TestResolveAppliesActiveValueSetAsPartialOverride(t *testing.T) {
	lib, err := Parse(fixture())
	if err != nil {
		t.Fatal(err)
	}
	got := lib.Resolve("prod")
	if got["bronzePath"].Value != "Files/bronze-prod" {
		t.Errorf("bronzePath = %v, want the prod override", got["bronzePath"].Value)
	}
	if got["strict"].Value != true {
		t.Errorf("strict = %v, want the prod override true", got["strict"].Value)
	}
	// Not mentioned by the prod set.
	if got["batchSize"].Value != float64(100) {
		t.Errorf("batchSize = %v, want the default to survive", got["batchSize"].Value)
	}
	// The declared type is not rewritten by an override.
	if got["bronzePath"].Type != "String" {
		t.Errorf("type = %q, want String", got["bronzePath"].Type)
	}
}

// An unknown active-set name resolves to the defaults rather than failing,
// because the library's own default values ARE a value set and have no file
// under valueSets/ — see Resolve's doc comment. A library with no alternative
// sets at all must still resolve.
func TestResolveUnknownValueSetFallsBackToDefaults(t *testing.T) {
	lib, err := Parse(fixture())
	if err != nil {
		t.Fatal(err)
	}
	if got := lib.Resolve("no-such-set")["bronzePath"].Value; got != "Files/bronze" {
		t.Errorf("bronzePath = %v, want the default", got)
	}
}

// A value set that overrides a variable the library no longer declares must
// not resurrect it. The declaration list is authoritative.
func TestResolveIgnoresOverrideForUndeclaredVariable(t *testing.T) {
	parts := fixture()
	parts["valueSets/stale.json"] = []byte(`{
		"name": "stale",
		"variableOverrides": [{"name": "deletedVariable", "value": "ghost"}]
	}`)
	lib, err := Parse(parts)
	if err != nil {
		t.Fatal(err)
	}
	got := lib.Resolve("stale")
	if _, ok := got["deletedVariable"]; ok {
		t.Error("an override resurrected a variable the library does not declare")
	}
	if len(got) != 3 {
		t.Errorf("resolved %d variables, want the 3 declared", len(got))
	}
}

// The set's identity is the `name` INSIDE the file, not the filename —
// activeValueSetName names the value set, and only the file can say what a set
// is called. A file whose name disagrees must resolve under the declared name.
func TestValueSetIdentityComesFromFileContentNotFilename(t *testing.T) {
	parts := fixture()
	parts["valueSets/whatever.json"] = []byte(`{
		"name": "staging",
		"variableOverrides": [{"name": "bronzePath", "value": "Files/bronze-staging"}]
	}`)
	lib, err := Parse(parts)
	if err != nil {
		t.Fatal(err)
	}
	if got := lib.Resolve("staging")["bronzePath"].Value; got != "Files/bronze-staging" {
		t.Errorf("resolving by declared name gave %v", got)
	}
	if got := lib.Resolve("whatever")["bronzePath"].Value; got != "Files/bronze" {
		t.Errorf("the filename should not name the set; got %v", got)
	}
}

// A Git-integration export roots the parts under the item folder. The same
// definition must parse either way.
func TestParseAcceptsFolderRootedParts(t *testing.T) {
	rooted := map[string][]byte{}
	for p, c := range fixture() {
		rooted["envLib.VariableLibrary/"+p] = c
	}
	lib, err := Parse(rooted)
	if err != nil {
		t.Fatal(err)
	}
	if got := lib.Resolve("prod")["bronzePath"].Value; got != "Files/bronze-prod" {
		t.Errorf("folder-rooted parts resolved to %v", got)
	}
}

func TestParseRejectsDefinitionWithoutVariables(t *testing.T) {
	if _, err := Parse(map[string][]byte{"settings.json": []byte(`{"valueSetsOrder":[]}`)}); err == nil {
		t.Fatal("a definition with no variables.json parsed without error")
	}
}

func TestParseReportsMalformedJSON(t *testing.T) {
	for _, bad := range []struct{ name, path, body string }{
		{"variables", "variables.json", `{"variables": [`},
		{"settings", "settings.json", `not json`},
		{"value set", "valueSets/prod.json", `{"name":`},
	} {
		parts := fixture()
		parts[bad.path] = []byte(bad.body)
		if _, err := Parse(parts); err == nil {
			t.Errorf("malformed %s parsed without error", bad.name)
		}
	}
}

// capturedTenantDefinition is the definition of `emuProbeVarLib` as a real
// tenant returned it from getDefinition on 2026-08-11, transcribed verbatim
// apart from shortened GUIDs. Nothing here is idealised: `note` really is
// emitted as "" rather than omitted, the library's type for a reference really
// is "ItemReference" (the PIPELINE declaration for the same variable says
// "Object"), and valueSetsOrder really does omit the active set. See
// docs/48-variable-libraries.md.
func capturedTenantDefinition() map[string][]byte {
	return map[string][]byte{
		"variables.json": []byte(`{
			"$schema": "https://developer.microsoft.com/json-schemas/fabric/item/variableLibrary/definition/variables/1.0.0/schema.json",
			"variables": [
				{"name": "bronzePath", "note": "env-invariant relative path", "type": "String", "value": "Files/bronze"},
				{"name": "runId", "note": "", "type": "Guid", "value": "11111111-2222-3333-4444-555555555555"},
				{"name": "silverNotebook", "note": "", "type": "ItemReference",
				 "value": {"itemId": "3f33c8a7-46bb-421b-8d24-dd1ddcba3953",
				           "workspaceId": "fd6cc69d-8250-4829-8e2a-7b3165fdf6af"}}
			]
		}`),
		"settings.json": []byte(`{
			"$schema": "https://developer.microsoft.com/json-schemas/fabric/item/variableLibrary/definition/settings/1.0.0/schema.json",
			"valueSetsOrder": ["qat"]
		}`),
		"valueSets/qat.json": []byte(`{
			"$schema": "https://developer.microsoft.com/json-schemas/fabric/item/variableLibrary/definition/valueSet/1.0.0/schema.json",
			"name": "qat",
			"variableOverrides": [{"name": "bronzePath", "value": "Files/bronze-qat"}]
		}`),
		".platform": []byte(`{"metadata":{"type":"VariableLibrary","displayName":"emuProbeVarLib"}}`),
	}
}

// THE decisive case for the fallback rule. The tenant reports
// activeValueSetName = "Default value set" — a name with no file under
// valueSets/ AND absent from valueSetsOrder. That is the out-of-the-box state
// of every Variable Library, so treating "active set matches no file" as an
// error would fail every library in its default configuration.
func TestActiveDefaultValueSetNameResolvesToDefaults(t *testing.T) {
	lib, err := Parse(capturedTenantDefinition())
	if err != nil {
		t.Fatal(err)
	}
	got := lib.Resolve("Default value set")
	if got["bronzePath"].Value != "Files/bronze" {
		t.Errorf("bronzePath = %v, want the default; the out-of-the-box active set must not fail",
			got["bronzePath"].Value)
	}
	if len(got) != 3 {
		t.Errorf("resolved %d variables, want 3", len(got))
	}
	// And .platform must not be mistaken for a value set.
	if _, ok := lib.ValueSets[".platform"]; ok {
		t.Error(".platform parsed as a value set")
	}
	if len(lib.ValueSets) != 1 {
		t.Errorf("value sets = %v, want just qat", lib.ValueSets)
	}
}

// An ItemReference value is an object, and it survives resolution intact so
// that @pipeline().libraryVariables.<alias>.itemId reaches it.
func TestItemReferenceValueRoundTrips(t *testing.T) {
	lib, err := Parse(capturedTenantDefinition())
	if err != nil {
		t.Fatal(err)
	}
	v, ok := lib.Lookup("Default value set", "silverNotebook")
	if !ok {
		t.Fatal("silverNotebook missing")
	}
	if v.Type != "ItemReference" {
		t.Errorf("library type = %q, want ItemReference (the PIPELINE side says Object)", v.Type)
	}
	obj, ok := v.Value.(map[string]any)
	if !ok {
		t.Fatalf("value = %#v, want an object", v.Value)
	}
	if obj["itemId"] != "3f33c8a7-46bb-421b-8d24-dd1ddcba3953" {
		t.Errorf("itemId = %v", obj["itemId"])
	}
	if obj["workspaceId"] != "fd6cc69d-8250-4829-8e2a-7b3165fdf6af" {
		t.Errorf("workspaceId = %v", obj["workspaceId"])
	}
}

// The qat set overrides bronzePath and NOTHING else, even though the library
// editor displays a value for every variable in the qat column — those cells
// show the defaults rather than overriding them. So resolution is a merge, and
// the non-overridden variables keep their defaults including the object one.
func TestCapturedValueSetIsAPartialOverride(t *testing.T) {
	lib, err := Parse(capturedTenantDefinition())
	if err != nil {
		t.Fatal(err)
	}
	got := lib.Resolve("qat")
	if got["bronzePath"].Value != "Files/bronze-qat" {
		t.Errorf("bronzePath = %v, want the qat override", got["bronzePath"].Value)
	}
	if got["runId"].Value != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("runId = %v, want the default to survive", got["runId"].Value)
	}
	obj, ok := got["silverNotebook"].Value.(map[string]any)
	if !ok || obj["itemId"] != "3f33c8a7-46bb-421b-8d24-dd1ddcba3953" {
		t.Errorf("silverNotebook = %#v, want the default object to survive", got["silverNotebook"].Value)
	}
}

func TestLookup(t *testing.T) {
	lib, err := Parse(fixture())
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := lib.Lookup("prod", "bronzePath"); !ok || v.Value != "Files/bronze-prod" {
		t.Errorf("Lookup = %v, %v", v, ok)
	}
	if _, ok := lib.Lookup("prod", "nope"); ok {
		t.Error("Lookup found a variable that does not exist")
	}
}
