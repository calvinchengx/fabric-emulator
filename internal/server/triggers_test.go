package server_test

// Event triggers end to end: a Reflex created over the real control plane, a
// file uploaded through the real OneLake ADLS surface with a real Storage
// token, and a pipeline that runs because of it — reading the file's name out
// of `@pipeline()?.TriggerEvent?.FileName`.

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"testing"

	entra "github.com/calvinchengx/entra-emulator/emulator"
)

// uploadToOneLake writes a file through the real DFS surface (create → append
// → flush), the three calls an ADLS SDK makes — so the event under test is
// produced by an ordinary storage client, not by reaching into the store.
func (f *fixture) uploadToOneLake(t *testing.T, token, wsID, itemID, rel, content string) {
	t.Helper()
	base := "/" + wsID + "/" + itemID + "/" + rel
	step := func(method, path string, body []byte, want int, ctx string) {
		if resp := f.ol(t, method, path, token, body); resp.StatusCode != want {
			raw, _ := io.ReadAll(resp.Body)
			t.Fatalf("%s: %d, want %d — %s", ctx, resp.StatusCode, want, raw)
		}
	}
	step("PUT", base+"?resource=file", nil, http.StatusCreated, "create")
	step("PATCH", base+"?action=append&position=0", []byte(content), http.StatusAccepted, "append")
	step("PATCH", base+fmt.Sprintf("?action=flush&position=%d", len(content)), nil, http.StatusOK, "flush")
}

func TestEventTriggerFiresFromARealOneLakeUpload(t *testing.T) {
	f := newFixture(t)

	var ws struct{ ID string }
	f.mustStatus(f.call("POST", "/v1/workspaces", f.token,
		map[string]string{"displayName": "Events"}, &ws), http.StatusCreated, "create workspace")

	// The event source, the Reflex that watches it, and the pipeline to run.
	var lake, reflex, pipe struct{ ID string }
	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/items", f.token,
		map[string]any{"displayName": "landing", "type": "Lakehouse"}, &lake), http.StatusCreated, "lakehouse")
	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/reflexes", f.token,
		map[string]any{"displayName": "on-arrival"}, &reflex), http.StatusCreated, "reflex")

	// The pipeline records the arriving file's name, so the assertion proves
	// the event data reached the definition, not just that something ran.
	def := `{"properties":{"activities":[
		{"name":"Capture","type":"SetVariable","typeProperties":{
			"variableName":"arrived","value":"@pipeline()?.TriggerEvent?.FileName"}}],
		"variables":{"arrived":{"type":"String"}}}}`
	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/items", f.token,
		map[string]any{"displayName": "ingest", "type": "DataPipeline"}, &pipe),
		http.StatusCreated, "pipeline")
	update := map[string]any{"definition": map[string]any{"parts": []map[string]string{{
		"path":        "pipeline-content.json",
		"payload":     base64.StdEncoding.EncodeToString([]byte(def)),
		"payloadType": "InlineBase64",
	}}}}
	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/items/"+pipe.ID+"/updateDefinition",
		f.token, update, nil), http.StatusAccepted, "updateDefinition")

	// Bind the trigger.
	triggers := "/v1/workspaces/" + ws.ID + "/reflexes/" + reflex.ID + "/triggers"
	var trig struct{ ID string }
	f.mustStatus(f.call("POST", triggers, f.token, map[string]any{
		"displayName": "on-landing",
		"eventType":   "Microsoft.Fabric.OneLake.FileCreated",
		"source":      map[string]any{"itemId": lake.ID, "pathPrefix": "Files/landing"},
		"action":      map[string]any{"itemId": pipe.ID, "jobType": "Pipeline"},
	}, &trig), http.StatusCreated, "create trigger")
	if trig.ID == "" {
		t.Fatal("no trigger id")
	}

	instances := "/v1/workspaces/" + ws.ID + "/items/" + pipe.ID + "/jobs/instances"
	var runs struct{ Value []map[string]any }
	f.mustStatus(f.call("GET", instances, f.token, nil, &runs), http.StatusOK, "no runs yet")
	if len(runs.Value) != 0 {
		t.Fatalf("runs before any upload: %+v", runs.Value)
	}

	// Storage-audience token for the same daemon SP: OneLake refuses the
	// control-plane audience, exactly as real Fabric does.
	storage := f.forgeToken(t, map[string]any{
		"clientId": entra.DaemonClientID, "audience": "https://storage.azure.com",
	})

	// A file outside the watched folder must not start anything.
	f.uploadToOneLake(t, storage, ws.ID, lake.ID, "Files/other/skip.csv", "id\n9\n")
	f.mustStatus(f.call("GET", instances, f.token, nil, &runs), http.StatusOK, "list after unwatched upload")
	if len(runs.Value) != 0 {
		t.Fatalf("an unwatched upload started %d runs", len(runs.Value))
	}

	// The real thing: an upload under the watched prefix.
	f.uploadToOneLake(t, storage, ws.ID, lake.ID, "Files/landing/orders.csv", "id,total\n1,10\n")
	f.mustStatus(f.call("GET", instances, f.token, nil, &runs), http.StatusOK, "list after watched upload")
	if len(runs.Value) != 1 {
		t.Fatalf("watched upload started %d runs, want 1", len(runs.Value))
	}
	if runs.Value[0]["invokeType"] != "EventTriggered" {
		t.Fatalf("invokeType = %v", runs.Value[0]["invokeType"])
	}

	// And the pipeline saw which file arrived.
	jid, _ := runs.Value[0]["id"].(string)
	resp := f.call("POST", instances+"/"+jid+"/queryactivityruns", f.token, map[string]any{}, nil)
	f.mustStatus(resp, http.StatusOK, "queryactivityruns")
	raw, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(raw, []byte("orders.csv")) {
		t.Fatalf("TriggerEvent.FileName never reached the pipeline: %s", raw)
	}

	// Deleting the trigger stops it.
	f.mustStatus(f.call("DELETE", triggers+"/"+trig.ID, f.token, nil, nil), http.StatusOK, "delete trigger")
	f.uploadToOneLake(t, storage, ws.ID, lake.ID, "Files/landing/more.csv", "id\n2\n")
	f.mustStatus(f.call("GET", instances, f.token, nil, &runs), http.StatusOK, "list after delete")
	if len(runs.Value) != 1 {
		t.Fatalf("a deleted trigger still fired: %d runs", len(runs.Value))
	}
}
