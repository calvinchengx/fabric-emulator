package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/pipeline"
)

// Malformed and hostile INPUTS to the compute-external activities.
//
// The behavioural suites next door drive each activity through a pipeline and
// assert what reached the engine. What they cannot reach cheaply is the input
// layer: an expression that fails to resolve, a typeProperty of the wrong JSON
// shape, an agent that will not answer. Those paths are most of each
// activity's error surface, and every one of them is a message a user reads
// while something is already going wrong — the worst moment for the emulator
// to answer with a Go type name or a bare "invalid character".
//
// `@nope(1)` is the resolve failure: an unknown function, which the expression
// evaluator rejects for every field uniformly.

// brokenAgent answers every statement with a transport error, so the
// agent-post failure branch is reachable without stopping a server mid-test.
func brokenAgent(t *testing.T, a *API) {
	t.Helper()
	if err := a.SetLivyAgent("http://127.0.0.1:1"); err != nil {
		t.Fatal(err)
	}
}

func TestActivityInputsFailLoudlyPerField(t *testing.T) {
	for _, tc := range []struct{ name, actType, tp, wantErr string }{
		// --- expression resolution, one per field that takes one ---------
		{"databricks notebookPath expr", "DatabricksNotebook",
			`"notebookPath":"@nope(1)"`, "notebookPath"},
		{"databricks baseParameter expr", "DatabricksNotebook",
			`"notebookPath":"%s/Files/j/e.py","baseParameters":{"p":"@nope(1)"}`, "baseParameter \"p\""},
		{"databricks parameter expr", "DatabricksSparkPython",
			`"pythonFile":"%s/Files/j/e.py","parameters":["@nope(1)"]`, "parameter 0"},
		{"hdinsight rootPath expr", "HDInsightSpark",
			`"rootPath":"@nope(1)","entryFilePath":"e.py"`, "rootPath"},
		{"hdinsight argument expr", "HDInsightSpark",
			`"rootPath":"%s/Files/j","entryFilePath":"e.py","arguments":["@nope(1)"]`, "argument 0"},
		{"hdinsight sparkConfig expr", "HDInsightSpark",
			`"rootPath":"%s/Files/j","entryFilePath":"e.py","sparkConfig":{"k":"@nope(1)"}`, "sparkConfig \"k\""},
		{"functions functionName expr", "AzureFunctionActivity",
			`"functionAppUrl":"http://x","functionName":"@nope(1)","method":"GET"`, "functionName"},
		{"functions header expr", "AzureFunctionActivity",
			`"functionAppUrl":"http://x","functionName":"f","method":"GET","headers":{"H":"@nope(1)"}`,
			"header \"H\""},
		{"webhook url expr", "WebHook", `"url":"@nope(1)","method":"POST"`, "url"},
		{"webhook header expr", "WebHook",
			`"url":"http://x","method":"POST","headers":{"H":"@nope(1)"}`, "header \"H\""},
		{"webhook timeout expr", "WebHook",
			`"url":"http://x","method":"POST","timeout":"@nope(1)"`, "timeout"},

		// --- JSON shape: the field exists but is the wrong kind ----------
		{"databricks baseParameters not an object", "DatabricksNotebook",
			`"notebookPath":"%s/Files/j/e.py","baseParameters":["a"]`, "baseParameters must be an object"},
		{"databricks parameters not an array", "DatabricksSparkPython",
			`"pythonFile":"%s/Files/j/e.py","parameters":{"a":1}`, "parameters must be an array"},
		{"hdinsight arguments not an array", "HDInsightSpark",
			`"rootPath":"%s/Files/j","entryFilePath":"e.py","arguments":{"a":1}`, "arguments must be an array"},
		{"hdinsight sparkConfig not an object", "HDInsightSpark",
			`"rootPath":"%s/Files/j","entryFilePath":"e.py","sparkConfig":["a"]`, "sparkConfig must be an object"},
		{"functions headers not an object", "AzureFunctionActivity",
			`"functionAppUrl":"http://x","functionName":"f","method":"GET","headers":["a"]`,
			"headers are not an object"},
		{"webhook headers not an object", "WebHook",
			`"url":"http://x","method":"POST","headers":["a"]`, "headers are not an object"},

		// --- addressing: a path with no path part -------------------------
		{"databricks bare item id", "DatabricksNotebook",
			`"notebookPath":"just-an-id"`, "<lakehouseItemId>/<path>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, st := newAPI(t)
			newFakeAgent(t, a)
			ws := seedWorkspace(t, st)
			// A real entry file, because several of these fields are resolved
			// only AFTER the file is fetched — without it those cases would
			// assert a not-found error and never reach the field under test.
			lh := seedLakehouse(t, st, ws.ID, "lake")
			seedFile(t, st, ws.ID, lh.ID, "Files/j/e.py", []byte("x = 1\n"))
			tp := strings.ReplaceAll(tc.tp, "%s", lh.ID)
			pl := createPipeline(t, st, ws.ID,
				`{"properties":{"activities":[{"name":"A","type":"`+tc.actType+
					`","typeProperties":{`+tp+`}}]}}`)
			_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
			if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
				t.Fatalf("%s = %s, want Failed", tc.name, s)
			}
			_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
			e, _ := runs[0]["error"].(string)
			if !strings.Contains(e, tc.wantErr) {
				t.Errorf("error %q does not name %q", e, tc.wantErr)
			}
			// Every message must name the activity, or a pipeline with a dozen
			// activities gives the reader nothing to look at.
			if !strings.Contains(e, `"A"`) {
				t.Errorf("error %q does not name the activity", e)
			}
		})
	}
}

// TestActivityAgentTransportFailure: the agent being unreachable is the
// activity's error, carrying the cause — not a nil-deref and not a success.
func TestActivityAgentTransportFailure(t *testing.T) {
	for _, tc := range []struct{ name, actType, tp string }{
		{"hdinsight", "HDInsightSpark", `"rootPath":"%s/Files/j","entryFilePath":"e.py"`},
		{"databricks", "DatabricksNotebook", `"notebookPath":"%s/Files/j/e.py"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, st := newAPI(t)
			brokenAgent(t, a)
			ws := seedWorkspace(t, st)
			lh := seedLakehouse(t, st, ws.ID, "lake")
			seedFile(t, st, ws.ID, lh.ID, "Files/j/e.py", []byte("x = 1\n"))
			tp := strings.ReplaceAll(tc.tp, "%s", lh.ID)

			pl := createPipeline(t, st, ws.ID,
				`{"properties":{"activities":[{"name":"A","type":"`+tc.actType+
					`","typeProperties":{`+tp+`}}]}}`)
			_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
			if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
				t.Fatalf("%s = %s, want Failed when the agent is unreachable", tc.name, s)
			}
			_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
			e, _ := runs[0]["error"].(string)
			if !strings.Contains(e, "A") || e == "" {
				t.Fatalf("error %q does not identify the failing activity", e)
			}
		})
	}
}

// TestDatabricksUnknownTypeIsRefused reaches the switch's default arm, which
// the dispatch cannot produce (it routes exactly three types here). Kept as a
// guard rather than deleted: a future dispatch entry that forgets to extend
// the switch would otherwise fall through to a nil spec and panic on the first
// field read. Driven directly, because going through a pipeline cannot reach it.
func TestDatabricksUnknownTypeIsRefused(t *testing.T) {
	a, _ := newAPI(t)
	e := &pipelineExecutor{a: a, wid: "w", jobID: "j", chain: []string{"p"}}
	_, err := e.databricksActivity(
		pipeline.Activity{Name: "A", Type: "DatabricksSomethingNew"},
		map[string]json.RawMessage{},
		func(json.RawMessage) (any, error) { return nil, nil },
	)
	if err == nil || !strings.Contains(err.Error(), "unknown type") {
		t.Fatalf("err = %v, want an unknown-type refusal", err)
	}
}

// TestAsIntReadsBothNumberShapes: a callback body decoded from JSON carries
// float64; a body built in Go carries int. reportStatusOnCallBack must fail
// the activity for either, so both shapes are read.
func TestAsIntReadsBothNumberShapes(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   any
		want int
		ok   bool
	}{
		{"json number", float64(500), 500, true},
		{"go int", 500, 500, true},
		{"string is not a status code", "500", 0, false},
		{"absent", nil, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := asInt(tc.in)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("asInt(%#v) = (%d,%v), want (%d,%v)", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestSplitRootPathShapes: the addressing helper both activities share.
func TestSplitRootPathShapes(t *testing.T) {
	for _, tc := range []struct {
		in, item, base string
		ok             bool
	}{
		{"item/Files/jobs", "item", "Files/jobs", true},
		{"/item/Files/", "item", "Files", true},
		{"item", "item", "", true}, // entry file at the item root
		{"", "", "", false},
		{"/", "", "", false},
	} {
		item, base, ok := splitRootPath(tc.in)
		if item != tc.item || base != tc.base || ok != tc.ok {
			t.Errorf("splitRootPath(%q) = (%q,%q,%v), want (%q,%q,%v)",
				tc.in, item, base, ok, tc.item, tc.base, tc.ok)
		}
	}
}

// --- the carried-through happy paths, and the timing/edge branches ---------
//
// The remaining uncovered blocks after the table above are of two kinds. Most
// are the SAME error shape on a different field — `str()` returning an error
// for `entryFilePath` rather than `rootPath` — where covering one establishes
// the helper and the rest are duplicates. The ones below are not duplicates:
// they are behaviours a user depends on (a config actually reaching the
// engine, a header actually being sent) or timing paths nothing else reaches.

// TestHDInsightCarriesSparkConfig: sparkConfig must reach the engine, not be
// parsed and dropped. Asserting only that the activity succeeds would pass on
// an implementation that discards it.
func TestHDInsightCarriesSparkConfig(t *testing.T) {
	a, st := newAPI(t)
	agent := newFakeAgent(t, a)
	ws := seedWorkspace(t, st)
	lh := seedLakehouse(t, st, ws.ID, "lake")
	seedFile(t, st, ws.ID, lh.ID, "Files/j/e.py", []byte("x = 1\n"))

	pl := createPipeline(t, st, ws.ID,
		`{"properties":{"activities":[{"name":"A","type":"HDInsightSpark","typeProperties":{
          "rootPath":"`+lh.ID+`/Files/j","entryFilePath":"e.py",
          "sparkConfig":{"spark.sql.shuffle.partitions":"8"}}}]}}`)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		t.Fatalf("job = %s", s)
	}
	code := strings.Join(agent.statements(), "\n")
	if !strings.Contains(code, "spark.sql.shuffle.partitions") || !strings.Contains(code, `8`) {
		t.Fatalf("sparkConfig never reached the engine: %s", code)
	}
}

// TestFunctionsAndWebhookSendTheirHeaders: a declared header must be on the
// wire. The receiver asserts it, because the activity's own output cannot.
func TestFunctionsAndWebhookSendTheirHeaders(t *testing.T) {
	for _, tc := range []struct{ name, actType, tp string }{
		{"functions", "AzureFunctionActivity",
			`"functionAppUrl":"%s","functionName":"f","method":"GET","headers":{"X-Trace":"abc123"}`},
		{"webhook", "WebHook",
			`"url":"%s","method":"POST","headers":{"X-Trace":"abc123"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, st := newAPI(t)
			ws := seedWorkspace(t, st)
			seen := make(chan string, 4)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen <- r.Header.Get("X-Trace")
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				if uri, _ := body["callBackUri"].(string); uri != "" {
					// A webhook parks; call it back at once so the test ends.
					go func() {
						parts := strings.Split(strings.TrimPrefix(uri, "/v1/workspaces/"), "/")
						req := httptest.NewRequest("POST", "/x", strings.NewReader(`{}`))
						for k, v := range map[string]string{
							"wid": parts[0], "iid": parts[2], "jid": parts[5], "token": parts[7],
						} {
							req.SetPathValue(k, v)
						}
						a.webhookCallbackHandler(httptest.NewRecorder(), req)
					}()
				}
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))
			defer srv.Close()

			pl := createPipeline(t, st, ws.ID,
				`{"properties":{"activities":[{"name":"A","type":"`+tc.actType+
					`","typeProperties":{`+strings.ReplaceAll(tc.tp, "%s", srv.URL)+`}}]}}`)
			_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
			if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Completed" {
				_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
				t.Fatalf("job = %s; runs=%+v", s, runs)
			}
			select {
			case got := <-seen:
				if got != "abc123" {
					t.Fatalf("X-Trace = %q, want the declared header value", got)
				}
			default:
				t.Fatal("the receiver was never called")
			}
		})
	}
}

// TestWebHookInitialCallFailureIsTheActivitysError: if the endpoint cannot be
// reached at all, the activity fails on THAT rather than parking for ten
// minutes waiting for a callback nobody was asked for.
func TestWebHookInitialCallFailureIsTheActivitysError(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	pl := createPipeline(t, st, ws.ID,
		`{"properties":{"activities":[{"name":"A","type":"WebHook","typeProperties":{
          "url":"http://127.0.0.1:1/hook","method":"POST"}}]}}`)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job = %s, want Failed without parking", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	if e, _ := runs[0]["error"].(string); !strings.Contains(e, "webhook activity") {
		t.Fatalf("error %q is not the webhook's own", e)
	}
}

// TestWebHookReportedStatusWithoutAnError: reportStatusOnCallBack with a
// non-2xx and NO error field still fails, with a message naming the code —
// an empty reason would be the silent half of a loud rule.
func TestWebHookReportedStatusWithoutAnError(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	rcv := newWebhookReceiver(t)
	pl := createPipeline(t, st, ws.ID, webhookPipeline(
		`"url":"`+rcv.srv.URL+`","method":"POST","reportStatusOnCallBack":true`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if code := rcv.callBack(t, a, `{"statusCode":503}`); code != http.StatusOK {
		t.Fatalf("callback = %d", code)
	}
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job = %s, want Failed", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	if e, _ := runs[0]["error"].(string); !strings.Contains(e, "503") {
		t.Fatalf("error %q does not name the reported status code", e)
	}
}

// TestWebHookExpiresOnRealTimeToo: the clock-change wake is not the only exit.
// With time running normally and a one-second timeout, the park must end by
// the real-time arm — the branch that keeps an unfrozen deployment from
// hanging forever when nobody advances anything.
func TestWebHookExpiresOnRealTimeToo(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	rcv := newWebhookReceiver(t)
	pl := createPipeline(t, st, ws.ID, webhookPipeline(
		`"url":"`+rcv.srv.URL+`","method":"POST","timeout":"00:00:01"`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job = %s, want Failed by real-time expiry", s)
	}
}

// TestFunctionsEmptyMethodIsRefused: an empty string is not a method, and
// defaulting one would send a request the definition never described.
func TestFunctionsEmptyMethodIsRefused(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	pl := createPipeline(t, st, ws.ID,
		`{"properties":{"activities":[{"name":"A","type":"AzureFunctionActivity","typeProperties":{
          "functionAppUrl":"http://x","functionName":"f","method":""}}]}}`)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job = %s, want Failed", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	if e, _ := runs[0]["error"].(string); !strings.Contains(e, "method is required") {
		t.Fatalf("error %q does not name the missing method", e)
	}
}
