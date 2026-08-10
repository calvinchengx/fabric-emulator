package server_test

// e2e witness for Variable Library resolution in data pipelines, over real
// HTTP: publish a Variable Library and a pipeline that declares a reference to
// it, run the pipeline, and observe that the value the run saw is the one the
// ACTIVE VALUE SET selects. Then flip the active value set through the item
// PATCH surface and observe the same definition resolve differently.
//
// That last step is the point of the whole feature and the reason this is an
// e2e rather than a unit test: it is the "one flag switches environments"
// property, demonstrated end to end rather than asserted about a parser.
//
// The wire shapes are captured, not invented — see docs/48-variable-libraries.md.

import (
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// varLibParts is a Variable Library definition in the format a real tenant
// round-trips: declarations with defaults, plus one alternative value set that
// overrides a SUBSET of them.
func varLibParts() []store.DefinitionPart {
	return []store.DefinitionPart{
		{Path: "variables.json", PayloadType: "InlineBase64", Payload: b64(`{
			"$schema": "https://developer.microsoft.com/json-schemas/fabric/item/variableLibrary/definition/variables/1.0.0/schema.json",
			"variables": [
				{"name": "bronzePath", "type": "String", "value": "Files/bronze", "note": "env-invariant relative path"},
				{"name": "batchSize", "type": "Integer", "value": 100}
			]
		}`)},
		{Path: "settings.json", PayloadType: "InlineBase64", Payload: b64(`{
			"$schema": "https://developer.microsoft.com/json-schemas/fabric/item/variableLibrary/definition/settings/1.0.0/schema.json",
			"valueSetsOrder": ["prod"]
		}`)},
		{Path: "valueSets/prod.json", PayloadType: "InlineBase64", Payload: b64(`{
			"$schema": "https://developer.microsoft.com/json-schemas/fabric/item/variableLibrary/definition/valueSet/1.0.0/schema.json",
			"name": "prod",
			"variableOverrides": [{"name": "bronzePath", "value": "Files/bronze-prod"}]
		}`)},
	}
}

// The pipeline compares the resolved library variable against a run parameter
// and FAILS when they differ. So a green run is evidence the value was what
// the test expected, and the witness can fail — an assertion that always
// passes would witness nothing.
const varLibPipeline = `{"properties":{
	"parameters":{"expected":{"type":"String","defaultValue":"Files/bronze"}},
	"libraryVariables":{
		"envLib_bronzePath":{"type":"String","variableName":"bronzePath","libraryName":"envLib"}
	},
	"activities":[
		{"name":"Check","type":"IfCondition","typeProperties":{
			"expression":{"value":"@equals(pipeline().libraryVariables.envLib_bronzePath, pipeline().parameters.expected)","type":"Expression"},
			"ifTrueActivities":[{"name":"Matched","type":"Wait","typeProperties":{"waitTimeInSeconds":0}}],
			"ifFalseActivities":[{"name":"Mismatch","type":"Fail","typeProperties":{
				"message":{"value":"@concat('resolved to ', pipeline().libraryVariables.envLib_bronzePath)","type":"Expression"}}}]
		}}
	]}}`

// runPipelineExpecting starts the pipeline with `expected` and returns the
// terminal job status plus the activity names that succeeded.
func runPipelineExpecting(t *testing.T, f *fixture, ws, pl, expected string) (string, map[string]bool) {
	t.Helper()
	base := "/v1/workspaces/" + ws + "/items/" + pl + "/jobs/instances"
	run := f.call("POST", base+"?jobType=Pipeline", f.token,
		map[string]any{"executionData": map[string]any{
			"parameters": map[string]any{"expected": expected},
		}}, nil)
	f.mustStatus(run, http.StatusAccepted, "run pipeline")
	loc := run.Header.Get("Location")
	jid := loc[strings.LastIndex(loc, "/")+1:]

	var job struct{ Status string }
	deadline := time.Now().Add(30 * time.Second)
	for {
		f.mustStatus(f.call("GET", base+"/"+jid, f.token, nil, &job), http.StatusOK, "get job")
		if job.Status != "InProgress" && job.Status != "NotStarted" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pipeline job never reached a terminal state")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var ar struct {
		Value []map[string]any
	}
	f.mustStatus(f.call("POST", base+"/"+jid+"/queryactivityruns", f.token, nil, &ar),
		http.StatusOK, "queryactivityruns")
	ok := map[string]bool{}
	for _, a := range ar.Value {
		if a["status"] == "Succeeded" {
			ok[a["activityName"].(string)] = true
		}
	}
	return job.Status, ok
}

func TestVariableLibraryResolutionE2E(t *testing.T) {
	f := newFixture(t)

	var ws struct{ ID string }
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "varlib-ws"}, &ws)

	// The library, published through the typed collection the REST reference
	// documents for this item type.
	var lib struct{ ID string }
	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/variableLibraries", f.token,
		map[string]any{"displayName": "envLib"}, &lib), http.StatusCreated, "create variable library")
	if err := f.srv.API.Store.SetDefinition(lib.ID, varLibParts()); err != nil {
		t.Fatal(err)
	}

	var pl struct{ ID string }
	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/items", f.token,
		map[string]string{"displayName": "medallion", "type": "DataPipeline"}, &pl),
		http.StatusCreated, "create pipeline")
	if err := f.srv.API.Store.SetDefinition(pl.ID, []store.DefinitionPart{
		{Path: "pipeline-content.json", PayloadType: "InlineBase64", Payload: b64(varLibPipeline)},
	}); err != nil {
		t.Fatal(err)
	}

	// 1. No active value set: the declared default resolves.
	status, ok := runPipelineExpecting(t, f, ws.ID, pl.ID, "Files/bronze")
	if status != "Completed" || !ok["Matched"] {
		t.Fatalf("default value set: job %s, activities %v; the library variable did not resolve to Files/bronze",
			status, ok)
	}

	// 2. The witness can fail. Without this the green above proves nothing:
	//    an expression resolving to blank would still "match" a blank
	//    expectation, which is exactly the silent-empty-path failure the
	//    feature exists to prevent.
	status, _ = runPipelineExpecting(t, f, ws.ID, pl.ID, "Files/somewhere-else")
	if status == "Completed" {
		t.Fatal("the pipeline succeeded against a value the library never declared; the check is vacuous")
	}

	// 3. THE ENVIRONMENT SWITCH. One PATCH of activeValueSetName, no change to
	//    either definition, and the same pipeline resolves the prod value.
	var patched struct {
		Properties struct{ ActiveValueSetName string }
	}
	f.mustStatus(f.call("PATCH", "/v1/workspaces/"+ws.ID+"/variableLibraries/"+lib.ID, f.token,
		map[string]any{"properties": map[string]any{"activeValueSetName": "prod"}}, &patched),
		http.StatusOK, "set active value set")
	if patched.Properties.ActiveValueSetName != "prod" {
		t.Fatalf("PATCH did not echo the active value set: %+v", patched)
	}

	status, ok = runPipelineExpecting(t, f, ws.ID, pl.ID, "Files/bronze-prod")
	if status != "Completed" || !ok["Matched"] {
		t.Fatalf("prod value set: job %s, activities %v; the override did not take effect", status, ok)
	}

	// 4. And the un-overridden variable still resolves to its default, so a
	//    value set really is a partial override rather than a replacement.
	status, _ = runPipelineExpecting(t, f, ws.ID, pl.ID, "Files/bronze")
	if status == "Completed" {
		t.Fatal("the default value still resolved after the prod set was activated")
	}

	// The active value set is readable back on the item, which is how a
	// deployment tool confirms which stage a workspace is configured for.
	var got struct {
		Properties struct{ ActiveValueSetName string }
	}
	f.mustStatus(f.call("GET", "/v1/workspaces/"+ws.ID+"/variableLibraries/"+lib.ID, f.token, nil, &got),
		http.StatusOK, "get variable library")
	if got.Properties.ActiveValueSetName != "prod" {
		t.Errorf("activeValueSetName not reported on the item: %+v", got)
	}
}

// Fabric documents a variable library name as NOT case sensitive, so a
// pipeline saying `envLib` must find a library called `ENVLIB`. Getting this
// wrong would fail only in whichever environment happened to capitalise
// differently, which is the class of bug this whole feature exists to remove.
func TestVariableLibraryNameIsCaseInsensitive(t *testing.T) {
	f := newFixture(t)

	var ws struct{ ID string }
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "varlib-case-ws"}, &ws)

	var lib struct{ ID string }
	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/variableLibraries", f.token,
		map[string]any{"displayName": "ENVLIB"}, &lib), http.StatusCreated, "create variable library")
	if err := f.srv.API.Store.SetDefinition(lib.ID, varLibParts()); err != nil {
		t.Fatal(err)
	}

	var pl struct{ ID string }
	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/items", f.token,
		map[string]string{"displayName": "cased", "type": "DataPipeline"}, &pl),
		http.StatusCreated, "create pipeline")
	if err := f.srv.API.Store.SetDefinition(pl.ID, []store.DefinitionPart{
		{Path: "pipeline-content.json", PayloadType: "InlineBase64", Payload: b64(varLibPipeline)},
	}); err != nil {
		t.Fatal(err)
	}

	status, ok := runPipelineExpecting(t, f, ws.ID, pl.ID, "Files/bronze")
	if status != "Completed" || !ok["Matched"] {
		t.Fatalf("job %s, activities %v; `envLib` did not match the library named ENVLIB", status, ok)
	}
}

// A library that exists but no longer declares the referenced variable, and a
// library whose definition cannot be read at all. Both must fail the run for
// the same reason a missing library does: the alternative is a blank value.
func TestVariableLibraryBrokenDefinitionsFailRun(t *testing.T) {
	for _, tc := range []struct {
		name  string
		parts []store.DefinitionPart
	}{
		{"variable was renamed away", []store.DefinitionPart{
			{Path: "variables.json", PayloadType: "InlineBase64", Payload: b64(
				`{"variables":[{"name":"silverPath","type":"String","value":"Files/silver"}]}`)},
		}},
		{"no variables.json", []store.DefinitionPart{
			{Path: "settings.json", PayloadType: "InlineBase64", Payload: b64(`{"valueSetsOrder":[]}`)},
		}},
		{"payload is not base64", []store.DefinitionPart{
			{Path: "variables.json", PayloadType: "InlineBase64", Payload: "!!! not base64 !!!"},
		}},
		{"variables.json is not JSON", []store.DefinitionPart{
			{Path: "variables.json", PayloadType: "InlineBase64", Payload: b64(`{"variables":[`)},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			var ws struct{ ID string }
			f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "varlib-broken-ws"}, &ws)

			var lib struct{ ID string }
			f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/variableLibraries", f.token,
				map[string]any{"displayName": "envLib"}, &lib), http.StatusCreated, "create variable library")
			if err := f.srv.API.Store.SetDefinition(lib.ID, tc.parts); err != nil {
				t.Fatal(err)
			}

			var pl struct{ ID string }
			f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/items", f.token,
				map[string]string{"displayName": "broken", "type": "DataPipeline"}, &pl),
				http.StatusCreated, "create pipeline")
			if err := f.srv.API.Store.SetDefinition(pl.ID, []store.DefinitionPart{
				{Path: "pipeline-content.json", PayloadType: "InlineBase64", Payload: b64(varLibPipeline)},
			}); err != nil {
				t.Fatal(err)
			}

			status, _ := runPipelineExpecting(t, f, ws.ID, pl.ID, "Files/bronze")
			if status != "Failed" {
				t.Fatalf("job status = %s; an unreadable library must fail the run, not resolve to blank", status)
			}
		})
	}
}

// activeValueSetName is refused on an item type that has no such property,
// rather than being stored where nothing would ever read it.
func TestActiveValueSetNameRejectedOnNonLibrary(t *testing.T) {
	f := newFixture(t)

	var ws struct{ ID string }
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "varlib-wrongtype-ws"}, &ws)

	var pl struct{ ID string }
	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/items", f.token,
		map[string]string{"displayName": "not-a-library", "type": "DataPipeline"}, &pl),
		http.StatusCreated, "create pipeline")

	f.mustStatus(f.call("PATCH", "/v1/workspaces/"+ws.ID+"/items/"+pl.ID, f.token,
		map[string]any{"properties": map[string]any{"activeValueSetName": "prod"}}, nil),
		http.StatusBadRequest, "activeValueSetName on a DataPipeline")
}

// A reference to a library that is not in the workspace fails the RUN with its
// own code, rather than resolving to blank and letting an activity write to
// the wrong place. This is the deploy-time mistake the feature must catch.
func TestVariableLibraryMissingReferenceFailsRun(t *testing.T) {
	f := newFixture(t)

	var ws struct{ ID string }
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "varlib-missing-ws"}, &ws)

	var pl struct{ ID string }
	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/items", f.token,
		map[string]string{"displayName": "orphan", "type": "DataPipeline"}, &pl),
		http.StatusCreated, "create pipeline")
	if err := f.srv.API.Store.SetDefinition(pl.ID, []store.DefinitionPart{
		{Path: "pipeline-content.json", PayloadType: "InlineBase64", Payload: b64(varLibPipeline)},
	}); err != nil {
		t.Fatal(err)
	}

	base := "/v1/workspaces/" + ws.ID + "/items/" + pl.ID + "/jobs/instances"
	run := f.call("POST", base+"?jobType=Pipeline", f.token, map[string]any{}, nil)
	f.mustStatus(run, http.StatusAccepted, "run pipeline")
	loc := run.Header.Get("Location")
	jid := loc[strings.LastIndex(loc, "/")+1:]

	var job struct {
		Status        string
		FailureReason struct {
			ErrorCode string
			Message   string
		}
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		f.mustStatus(f.call("GET", base+"/"+jid, f.token, nil, &job), http.StatusOK, "get job")
		if job.Status != "InProgress" && job.Status != "NotStarted" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pipeline job never reached a terminal state")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if job.Status != "Failed" {
		t.Fatalf("job status = %s; a pipeline referencing a missing library must fail", job.Status)
	}
	if job.FailureReason.ErrorCode != "PipelineLibraryVariableUnresolved" {
		t.Errorf("errorCode = %q, want PipelineLibraryVariableUnresolved (a generic code sends the reader to the wrong file)",
			job.FailureReason.ErrorCode)
	}
	if !strings.Contains(job.FailureReason.Message, "Variable Library") {
		t.Errorf("failure message does not name the cause: %q", job.FailureReason.Message)
	}
}
