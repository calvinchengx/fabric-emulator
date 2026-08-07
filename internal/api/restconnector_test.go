package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/pipeline"
	"github.com/calvinchengx/fabric-emulator/internal/warehouse"
)

// Every case here drives a REAL server, for the same reason webactivity_test.go
// does: a test that asserted the shape of a fabricated response would reproduce
// the bug this replaced. `literal` and `webAct`'s style are shared with that
// file — same package, same contract.

// restSrc builds a RestSource copy and runs just the source half.
func restSrc(t *testing.T, api *API, source map[string]any, extra map[string]any) (*warehouse.Table, string, error) {
	t.Helper()
	tp := map[string]json.RawMessage{}
	raw, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	tp["source"] = raw
	for k, v := range extra {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		tp[k] = b
	}
	e := &pipelineExecutor{a: api}
	return e.restSourceTable(pipeline.Activity{Name: "Ingest", Type: "Copy"}, tp, literal)
}

func jsonServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRestSourceReallyCallsAndShapesRows(t *testing.T) {
	var got struct{ method, path, auth, accept string }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path = r.Method, r.URL.Path
		got.auth, got.accept = r.Header.Get("Authorization"), r.Header.Get("Accept")
		_, _ = w.Write([]byte(`{"entries":[
			{"id":"INC001","priority":1,"open":true},
			{"id":"INC002","priority":3,"open":false}]}`))
	}))
	defer srv.Close()

	tbl, url, err := restSrc(t, &API{}, map[string]any{
		"type":              "RestSource",
		"url":               srv.URL + "/api/arsys/v1/entry/HPD:Help%20Desk",
		"additionalHeaders": map[string]any{"Authorization": "AR-JWT t0ken"},
	}, nil)
	if err != nil {
		t.Fatalf("rest source failed: %v", err)
	}

	if got.method != http.MethodGet || !strings.HasPrefix(got.path, "/api/arsys/v1/entry/") {
		t.Fatalf("server saw %s %s", got.method, got.path)
	}
	// BMC Helix's scheme, passed straight through — this is R1's whole auth story.
	if got.auth != "AR-JWT t0ken" {
		t.Fatalf("Authorization not sent: %q", got.auth)
	}
	if url != srv.URL+"/api/arsys/v1/entry/HPD:Help%20Desk" {
		t.Fatalf("resolved url = %q", url)
	}

	if len(tbl.Rows) != 2 {
		t.Fatalf("want 2 rows, got %d: %+v", len(tbl.Rows), tbl.Rows)
	}
	// Columns are first-seen order, deterministic rather than map order.
	if strings.Join(tbl.Columns, ",") != "id,open,priority" {
		t.Fatalf("columns = %v", tbl.Columns)
	}
	// JSON DESCRIBES its types; a number must not arrive as a string.
	if _, ok := tbl.Rows[0][2].(float64); !ok {
		t.Fatalf("priority should stay numeric, got %T (%v)", tbl.Rows[0][2], tbl.Rows[0][2])
	}
	if tbl.Rows[0][1] != true {
		t.Fatalf("open should stay boolean, got %T", tbl.Rows[0][1])
	}
}

func TestRestSourceForcesAcceptJSON(t *testing.T) {
	// Fabric documents that the connector IGNORES the author's Accept, because
	// it only handles JSON. Honouring `text/csv` would fetch a body this cannot
	// parse and then blame the parser.
	var accept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accept = r.Header.Get("Accept")
		_, _ = w.Write([]byte(`{"rows":[{"a":1}]}`))
	}))
	defer srv.Close()

	if _, _, err := restSrc(t, &API{}, map[string]any{
		"type": "RestSource", "url": srv.URL,
		"additionalHeaders": map[string]any{"Accept": "text/csv"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if accept != "application/json" {
		t.Fatalf("Accept = %q, want it overridden to application/json", accept)
	}
}

func TestRestSourceCollectionReferenceSelectsTheArray(t *testing.T) {
	srv := jsonServer(t, `{"result":{"items":[{"a":1},{"a":2}]},"meta":[{"ignored":true}]}`)
	for _, ref := range []string{"$.result.items", "$['result']['items']", "$.result['items']"} {
		t.Run(ref, func(t *testing.T) {
			tbl, _, err := restSrc(t, &API{},
				map[string]any{"type": "RestSource", "url": srv.URL},
				map[string]any{"translator": map[string]any{"collectionReference": ref}})
			if err != nil {
				t.Fatal(err)
			}
			if len(tbl.Rows) != 2 || strings.Join(tbl.Columns, ",") != "a" {
				t.Fatalf("picked the wrong array: cols=%v rows=%v", tbl.Columns, tbl.Rows)
			}
		})
	}
}

func TestRestSourceRefusesToGuessBetweenArrays(t *testing.T) {
	// Two arrays and nothing saying which holds the records. Guessing is how a
	// copy silently ingests the wrong one, so it fails AND names them.
	srv := jsonServer(t, `{"entries":[{"a":1}],"errors":[{"b":2}]}`)
	_, _, err := restSrc(t, &API{}, map[string]any{"type": "RestSource", "url": srv.URL}, nil)
	if err == nil {
		t.Fatal("two candidate arrays must not be guessed between")
	}
	if !strings.Contains(err.Error(), "entries") || !strings.Contains(err.Error(), "errors") ||
		!strings.Contains(err.Error(), "collectionReference") {
		t.Fatalf("error should name both arrays and the fix: %v", err)
	}
}

func TestRestSourceAutoDetectsTheOnlyArray(t *testing.T) {
	// One array is unambiguous — requiring a collectionReference for the common
	// case would be ceremony.
	srv := jsonServer(t, `{"count":2,"entries":[{"a":1},{"a":2}]}`)
	tbl, _, err := restSrc(t, &API{}, map[string]any{"type": "RestSource", "url": srv.URL}, nil)
	if err != nil || len(tbl.Rows) != 2 {
		t.Fatalf("err=%v tbl=%+v", err, tbl)
	}
}

func TestRestSourceAcceptsATopLevelArray(t *testing.T) {
	srv := jsonServer(t, `[{"a":1},{"a":2},{"a":3}]`)
	tbl, _, err := restSrc(t, &API{}, map[string]any{"type": "RestSource", "url": srv.URL}, nil)
	if err != nil || len(tbl.Rows) != 3 {
		t.Fatalf("err=%v tbl=%+v", err, tbl)
	}
}

func TestRestSourceFailsOnNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"AR-JWT expired"}`))
	}))
	defer srv.Close()

	_, _, err := restSrc(t, &API{}, map[string]any{"type": "RestSource", "url": srv.URL}, nil)
	if err == nil {
		t.Fatal("a 401 must fail the copy")
	}
	// Status AND body: "copy failed" with neither sends someone to the server's logs.
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "AR-JWT expired") {
		t.Fatalf("error does not say what happened: %v", err)
	}
}

func TestRestSourceRefusesAnAuthenticationTypeItCannotHonour(t *testing.T) {
	// Falling through to an anonymous request would 401 at the endpoint and be
	// reported as a connector bug.
	srv := jsonServer(t, `{"entries":[]}`)
	_, _, err := restSrc(t, &API{}, map[string]any{
		"type": "RestSource",
		"datasetSettings": map[string]any{
			"linkedService": map[string]any{"properties": map[string]any{
				"typeProperties": map[string]any{
					"url": srv.URL, "authenticationType": "OAuth2ClientCredential"}}},
		},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "OAuth2ClientCredential") {
		t.Fatalf("err = %v, want it to name the unsupported authenticationType", err)
	}
}

func TestRestSourceBuildsTheURLFromLinkedServiceAndDataset(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = w.Write([]byte(`{"entries":[{"a":1}]}`))
	}))
	defer srv.Close()

	// Fabric's real split: base on the linked service, relative on the dataset.
	if _, _, err := restSrc(t, &API{}, map[string]any{
		"type": "RestSource",
		"datasetSettings": map[string]any{
			"typeProperties": map[string]any{"relativeUrl": "/api/now/table/incident"},
			"linkedService": map[string]any{"properties": map[string]any{
				"typeProperties": map[string]any{"url": srv.URL, "authenticationType": "Anonymous"}}},
		},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if path != "/api/now/table/incident" {
		t.Fatalf("path = %q, want base+relative joined", path)
	}
}

func TestRestSourceRefusesAMissingOrNonHTTPURL(t *testing.T) {
	for _, tc := range []struct{ name, url, want string }{
		{"absent", "", "needs a url"},
		{"scheme", "file:///etc/passwd", "not http(s)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := map[string]any{"type": "RestSource"}
			if tc.url != "" {
				src["url"] = tc.url
			}
			_, _, err := restSrc(t, &API{}, src, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestRestSourceRefusesADisallowedMethod(t *testing.T) {
	srv := jsonServer(t, `{"entries":[]}`)
	_, _, err := restSrc(t, &API{}, map[string]any{
		"type": "RestSource", "url": srv.URL, "requestMethod": "DELETE",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "DELETE") {
		t.Fatalf("err = %v, want the verb named", err)
	}
}

func TestRestSourcePostsARequestBody(t *testing.T) {
	var saw, ctype string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		saw, ctype = string(b), r.Header.Get("Content-Type")
		_, _ = w.Write([]byte(`{"entries":[{"a":1}]}`))
	}))
	defer srv.Close()

	if _, _, err := restSrc(t, &API{}, map[string]any{
		"type": "RestSource", "url": srv.URL,
		"requestMethod": "POST", "requestBody": `{"q":"state=open"}`,
	}, nil); err != nil {
		t.Fatal(err)
	}
	if saw != `{"q":"state=open"}` || ctype != "application/json" {
		t.Fatalf("body=%q content-type=%q", saw, ctype)
	}
}

func TestRestSourceRefusesAnOversizedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, restMaxBody+16))
	}))
	defer srv.Close()

	_, _, err := restSrc(t, &API{}, map[string]any{"type": "RestSource", "url": srv.URL}, nil)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("err = %v, want an exceeds-limit refusal", err)
	}
}

func TestRestSourceHonoursHTTPRequestTimeout(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-block }))
	defer func() { close(block); srv.Close() }()

	_, _, err := restSrc(t, &API{}, map[string]any{
		"type": "RestSource", "url": srv.URL, "httpRequestTimeout": "00:00:01",
	}, nil)
	if err == nil {
		t.Fatal("a hung endpoint must fail the copy, not hang the pipeline")
	}
}

func TestRestSourceNamesSkippedNestedColumns(t *testing.T) {
	// A nested object has no column shape. Dropping it silently is how someone
	// spends an afternoon asking where a field went.
	srv := jsonServer(t, `{"entries":[{"id":"A","assignee":{"name":"ada"},"tags":["x"]}]}`)
	tbl, _, err := restSrc(t, &API{}, map[string]any{"type": "RestSource", "url": srv.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(tbl.Columns, ",") != "id" {
		t.Fatalf("columns = %v, want only the scalar", tbl.Columns)
	}
	if strings.Join(tbl.Skipped, ",") != "assignee,tags" {
		t.Fatalf("skipped = %v, want both nested fields named", tbl.Skipped)
	}
}

func TestRestSourceRefusesRecordsThatAreNotObjects(t *testing.T) {
	srv := jsonServer(t, `{"entries":[{"a":1},"just a string"]}`)
	_, _, err := restSrc(t, &API{}, map[string]any{"type": "RestSource", "url": srv.URL}, nil)
	if err == nil || !strings.Contains(err.Error(), "record 1") {
		t.Fatalf("err = %v, want the offending index named", err)
	}
}

func TestRestTableRefusesAboveTheRowCeiling(t *testing.T) {
	// Refused, not truncated — the same rule httpx.ReadBounded encodes for bodies.
	_, err := restTable(pipeline.Activity{Name: "Ingest"}, make([]any, restMaxRows+1))
	if err == nil || !strings.Contains(err.Error(), "refused rather than truncated") {
		t.Fatalf("err = %v", err)
	}
}

func TestJSONPathLookupSubset(t *testing.T) {
	doc := map[string]any{"a": map[string]any{"b": []any{1.0}}}
	for _, tc := range []struct {
		path string
		ok   bool
	}{
		{"$.a.b", true}, {"$['a']['b']", true}, {`$["a"]["b"]`, true},
		{"$.a", true}, {"$.missing", false}, {"$.a.b.c", false},
		{"a.b", false}, // no leading $ then a bare segment: unsupported, must not match
		{"$..a", false}, {"$.a[*]", false},
	} {
		_, ok := jsonPathLookup(doc, tc.path)
		if ok != tc.ok {
			t.Fatalf("jsonPathLookup(%q) ok = %v, want %v", tc.path, ok, tc.ok)
		}
	}
}

// --- end to end, through a real pipeline run -------------------------------

func TestRestSourceCopyLandsRowsInADeltaTable(t *testing.T) {
	srv := jsonServer(t, `{"entries":[
		{"incident":"INC001","priority":1},
		{"incident":"INC002","priority":2}]}`)

	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lh := seedLakehouse(t, st, ws.ID, "lake")
	loc := `"datasetSettings":{"linkedService":{"properties":{"typeProperties":{"artifactId":"` + lh.ID + `"}}}}`
	content := `{"properties":{"activities":[
      {"name":"Ingest","type":"Copy","typeProperties":{
        "source":{"type":"RestSource","url":"` + srv.URL + `/entry/HPD"},
        "sink":{"type":"LakehouseTableSink","table":"bronze_incidents",` + loc + `}
      }}]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		t.Fatalf("status = %s", s)
	}

	tbl, err := warehouse.ReadDeltaTable(st, lh.ID, "bronze_incidents")
	if err != nil {
		t.Fatalf("the rows must really be committed as Delta: %v", err)
	}
	if len(tbl.Rows) != 2 {
		t.Fatalf("want 2 rows, got %d: %v", len(tbl.Rows), tbl.Rows)
	}

	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	out := runs[0]["output"].(map[string]any)
	if fmt.Sprint(out["rowsCopied"]) != "2" {
		t.Fatalf("rowsCopied = %v", out["rowsCopied"])
	}
	// The lineage edge marks the source as OUTSIDE Fabric and carries the URL —
	// without that the portal would hunt for an item that does not exist.
	edge, ok := out["lineage"].(map[string]any)
	if !ok {
		t.Fatalf("no lineage in output: %+v", out)
	}
	if edge["sourceKind"] != "connection" {
		t.Fatalf("sourceKind = %v, want connection", edge["sourceKind"])
	}
	if !strings.Contains(fmt.Sprint(edge["sourcePath"]), "/entry/HPD") {
		t.Fatalf("sourcePath should carry the url: %v", edge["sourcePath"])
	}
}

func TestRestSourceCopyRefusesANonTableSink(t *testing.T) {
	// R1 lands rows in a Delta table. A Files/ sink would mean choosing a file
	// format the source never described.
	srv := jsonServer(t, `{"entries":[{"a":1}]}`)
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lh := seedLakehouse(t, st, ws.ID, "lake")
	content := `{"properties":{"activities":[
      {"name":"Ingest","type":"Copy","typeProperties":{
        "source":{"type":"RestSource","url":"` + srv.URL + `"},
        "sink":{"location":{"itemId":"` + lh.ID + `","path":"Files/out.json"}}
      }}]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := jobStatus(t, a, ws.ID, pl.ID, jid); s == "Completed" {
		t.Fatal("a non-table sink must fail rather than quietly do something else")
	}
}
