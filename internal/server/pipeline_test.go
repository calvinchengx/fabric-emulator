package server_test

// R5 e2e: a Data Pipeline run over real HTTP. A real entra token is validated,
// RBAC is checked, then the interpreter executes the pipeline's control flow
// (ForEach → IfCondition → notebook activity), chaining a real notebook job —
// and the activity runs are queryable. The full auth → RBAC → orchestration
// path, not a handler unit test.

import (
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// TestHCLivyE2E confirms the high-concurrency Livy layer over real HTTP: the
// packing contract works, and — critically — the `highConcurrencySessions`
// routes win over the classic `{livypath...}` catch-all on the real ServeMux.
func TestHCLivyE2E(t *testing.T) {
	f := newFixture(t)

	var ws struct{ ID string }
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "spark-ws"}, &ws)
	var lake struct{ ID string }
	f.call("POST", "/v1/workspaces/"+ws.ID+"/lakehouses", f.token, map[string]any{"displayName": "lh"}, &lake)

	hc := "/v1/workspaces/" + ws.ID + "/lakehouses/" + lake.ID + "/livyapi/versions/2023-12-01/highConcurrencySessions"

	// Two acquires sharing a tag pack into one underlying Livy session with
	// distinct HC ids — proving the HC route resolved (not the classic proxy).
	var a, b struct {
		ID, SessionId, ReplId string
	}
	f.mustStatus(f.call("POST", hc, f.token, map[string]string{"sessionTag": "etl"}, &a), http.StatusOK, "acquire a")
	f.mustStatus(f.call("POST", hc, f.token, map[string]string{"sessionTag": "etl"}, &b), http.StatusOK, "acquire b")
	if a.ID == "" || a.ID == b.ID {
		t.Fatalf("expected distinct HC ids, got %q and %q", a.ID, b.ID)
	}
	if a.SessionId != b.SessionId {
		t.Fatalf("same-tag acquires should share a session, got %s vs %s", a.SessionId, b.SessionId)
	}
	f.mustStatus(f.call("GET", hc+"/"+a.ID, f.token, nil, nil), http.StatusOK, "get hc")
	f.mustStatus(f.call("DELETE", hc+"/"+a.ID, f.token, nil, nil), http.StatusOK, "delete hc")

	// The classic Livy path still routes to the proxy (501, no backend) — both
	// route families coexist on the same mux.
	classic := "/v1/workspaces/" + ws.ID + "/lakehouses/" + lake.ID + "/livyapi/versions/2023-12-01/sessions"
	f.mustStatus(f.call("GET", classic, f.token, nil, nil), http.StatusNotImplemented, "classic sessions")
}

func TestPipelineRunE2E(t *testing.T) {
	f := newFixture(t)

	var ws struct{ ID string }
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "pipeline-ws"}, &ws)

	// A notebook the pipeline will invoke.
	var nb struct{ ID string }
	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/items", f.token,
		map[string]string{"displayName": "etl-nb", "type": "Notebook"}, &nb), http.StatusCreated, "create notebook")

	// The pipeline item (definition published out-of-band via the store; the
	// fabric-cicd e2e already covers the HTTP publish path).
	var pl struct{ ID string }
	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/items", f.token,
		map[string]string{"displayName": "nightly-etl", "type": "DataPipeline"}, &pl), http.StatusCreated, "create pipeline")

	content := `{"properties":{
      "parameters":{"tables":{"type":"Array","defaultValue":["sales","regions","dim"]}},
      "variables":{"seen":{"type":"Array"}},
      "activities":[
        {"name":"Each","type":"ForEach","typeProperties":{
          "items":"@pipeline().parameters.tables",
          "activities":[{"name":"Track","type":"AppendVariable","typeProperties":{"variableName":"seen","value":"@item()"}}]
        }},
        {"name":"Gate","type":"IfCondition","dependsOn":[{"activity":"Each","dependencyConditions":["Succeeded"]}],
          "typeProperties":{"expression":{"value":"@greaterOrEquals(length(variables('seen')),3)","type":"Expression"},
            "ifTrueActivities":[{"name":"RunNb","type":"TridentNotebook","typeProperties":{"notebookId":"` + nb.ID + `"}}]}}
      ]}}`
	payload := base64.StdEncoding.EncodeToString([]byte(content))
	if err := f.srv.API.Store.SetDefinition(pl.ID, []store.DefinitionPart{
		{Path: "pipeline-content.json", Payload: payload, PayloadType: "InlineBase64"},
	}); err != nil {
		t.Fatal(err)
	}

	// Run it through the real jobs API.
	base := "/v1/workspaces/" + ws.ID + "/items/" + pl.ID + "/jobs/instances"
	run := f.call("POST", base+"?jobType=Pipeline", f.token, map[string]any{}, nil)
	f.mustStatus(run, http.StatusAccepted, "run pipeline")
	loc := run.Header.Get("Location")
	jid := loc[strings.LastIndex(loc, "/")+1:]

	var job struct{ Status string }
	f.mustStatus(f.call("GET", base+"/"+jid, f.token, nil, &job), http.StatusOK, "get job")
	if job.Status != "Completed" {
		t.Fatalf("pipeline job status = %s", job.Status)
	}

	// Activity runs are queryable, and the notebook activity ran.
	var ar struct {
		Status string
		Value  []map[string]any
	}
	f.mustStatus(f.call("POST", base+"/"+jid+"/queryactivityruns", f.token, nil, &ar),
		http.StatusOK, "queryactivityruns")
	if ar.Status != "Succeeded" {
		t.Fatalf("activity-run status = %s", ar.Status)
	}
	names := map[string]bool{}
	for _, a := range ar.Value {
		if a["status"] == "Succeeded" {
			names[a["activityName"].(string)] = true
		}
	}
	for _, n := range []string{"Each", "Gate", "RunNb", "Track"} {
		if !names[n] {
			t.Errorf("activity %s not Succeeded in %+v", n, ar.Value)
		}
	}
}
