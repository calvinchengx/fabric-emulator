package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The stand-in function app REJECTS a missing or wrong key before anything
// else: without that negative control, every happy-path assertion below could
// be satisfied by a server that lets anyone in, which is the SeaweedFS rule.
func functionStandIn(t *testing.T, wantKey string, calls *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-functions-key") != wantKey {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid key"}`))
			return
		}
		*calls = append(*calls, r.Method+" "+r.URL.Path)
		_, _ = w.Write([]byte(`{"rows":3}`))
	}))
}

func functionPipeline(tp string) string {
	return `{"properties":{"activities":[
      {"name":"Fn","type":"AzureFunctionActivity","typeProperties":{` + tp + `}}]}}`
}

// TestFunctionActivityCallsForReal: the URL is functionAppUrl + /api/ + name,
// the key rides x-functions-key, and the JSON response merges into the output
// exactly as Web's does — @activity('Fn').output.rows must resolve downstream.
func TestFunctionActivityCallsForReal(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	var calls []string
	srv := functionStandIn(t, "s3cret", &calls)
	defer srv.Close()

	content := `{"properties":{
      "variables":{"n":{"type":"String"}},
      "activities":[
        {"name":"Fn","type":"AzureFunctionActivity","typeProperties":{
          "functionAppUrl":"` + srv.URL + `","functionKey":"s3cret",
          "functionName":"ingest","method":"POST","body":{"batch":1}}},
        {"name":"Use","type":"SetVariable","dependsOn":[{"activity":"Fn","dependencyConditions":["Succeeded"]}],
          "typeProperties":{"variableName":"n","value":"@string(activity('Fn').output.rows)"}}
      ]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
		t.Fatalf("job = %s; runs=%+v", s, runs)
	}
	if len(calls) != 1 || calls[0] != "POST /api/ingest" {
		t.Fatalf("calls = %v, want exactly [POST /api/ingest]", calls)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	if v := outputOf(runs, "Use")["value"]; v != "3" {
		t.Fatalf("downstream saw %v, want the function's rows=3", v)
	}
}

// TestFunctionActivityWrongKeyFails: the 401 fails the activity (Fabric's
// non-2xx rule), and the stand-in proves the pass above cannot be vacuous.
func TestFunctionActivityWrongKeyFails(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	var calls []string
	srv := functionStandIn(t, "s3cret", &calls)
	defer srv.Close()

	pl := createPipeline(t, st, ws.ID, functionPipeline(
		`"functionAppUrl":"`+srv.URL+`","functionKey":"wrong",
         "functionName":"ingest","method":"POST","body":{}`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("wrong key = %s, want Failed", s)
	}
	if len(calls) != 0 {
		t.Fatalf("the stand-in accepted a wrong key — the control is broken")
	}
}

// TestFunctionActivitySchemaRules: the ADF schema's own constraints, refused by
// name rather than silently tolerated — an accepted definition Fabric rejects
// is the permissive failure an emulator must not have.
func TestFunctionActivitySchemaRules(t *testing.T) {
	for _, tc := range []struct{ name, tp, wantErr string }{
		{"body on GET", `"functionAppUrl":"http://x","functionName":"f","method":"GET","body":{}`,
			"not allowed for GET"},
		{"no body on POST", `"functionAppUrl":"http://x","functionName":"f","method":"POST"`,
			"body is required for POST"},
		{"method outside enum", `"functionAppUrl":"http://x","functionName":"f","method":"PATCH"`,
			"not in the activity's enum"},
		{"missing functionName", `"functionAppUrl":"http://x","method":"GET"`,
			"functionName is required"},
		{"missing functionAppUrl", `"functionName":"f","method":"GET"`,
			"models no connections"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, st := newAPI(t)
			ws := seedWorkspace(t, st)
			pl := createPipeline(t, st, ws.ID, functionPipeline(tc.tp))
			_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
			if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
				t.Fatalf("%s = %s, want Failed", tc.name, s)
			}
			_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
			e, _ := runs[0]["error"].(string)
			if !strings.Contains(e, tc.wantErr) {
				t.Errorf("error %q does not carry %q", e, tc.wantErr)
			}
		})
	}
}
