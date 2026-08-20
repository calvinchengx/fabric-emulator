package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/pipeline"
	"github.com/calvinchengx/fabric-emulator/internal/warehouse"
)

// Bulk API 2.0 is a LIFECYCLE, so the stand-in below is a state machine rather
// than a canned response: create, poll, page. A test that returned rows from one
// GET would prove nothing about the part that actually breaks.

type sfOrg struct {
	mu sync.Mutex
	// requests records "METHOD path" in order — the only thing that distinguishes
	// "ran the lifecycle" from "guessed the answer".
	requests []string
	created  map[string]any
	// pollsBeforeComplete makes the job spend time in a non-terminal state, the
	// way a real one does.
	pollsBeforeComplete int
	polls               int
	state               string
	// pages maps a locator ("" for the first) to its CSV body and next locator.
	pages map[string][2]string
}

func newOrg(t *testing.T, pages map[string][2]string) (*sfOrg, string) {
	t.Helper()
	org := &sfOrg{created: map[string]any{}, state: "JobComplete", pages: pages}
	srv := httptest.NewServer(org)
	t.Cleanup(srv.Close)
	return org, srv.URL
}

func (o *sfOrg) log(r *http.Request) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.requests = append(o.requests, r.Method+" "+r.URL.Path)
}

func (o *sfOrg) seen() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.requests...)
}

func (o *sfOrg) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.log(r)
	if r.Header.Get("Authorization") != "Bearer sf-token" {
		// A real org rejects an unauthenticated Bulk call; so does this, so the
		// token path is proven rather than assumed.
		http.Error(w, `[{"errorCode":"INVALID_SESSION_ID"}]`, http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/jobs/query"):
		body, _ := json.Marshal(map[string]any{"id": "750xx000000005LAAQ", "state": "UploadComplete"})
		var got map[string]any
		raw := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(raw)
		_ = json.Unmarshal(raw, &got)
		o.mu.Lock()
		o.created = got
		o.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)

	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/results"):
		loc := r.URL.Query().Get("locator")
		page, ok := o.pages[loc]
		if !ok {
			http.Error(w, "unknown locator "+loc, http.StatusBadRequest)
			return
		}
		// "null" as a STRING is how Salesforce says "no more pages".
		w.Header().Set("Sforce-Locator", page[1])
		w.Header().Set("Content-Type", "text/csv")
		_, _ = w.Write([]byte(page[0]))

	case r.Method == http.MethodGet:
		o.mu.Lock()
		o.polls++
		state := "InProgress"
		if o.polls > o.pollsBeforeComplete {
			state = o.state
		}
		o.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"750xx000000005LAAQ","state":%q,"errorMessage":"boom"}`, state)

	default:
		http.Error(w, "unexpected", http.StatusNotFound)
	}
}

func sfSource(t *testing.T, extra map[string]any, instance string) (*warehouse.Table, int, error) {
	t.Helper()
	src := map[string]any{"type": "SalesforceV2Source", "instanceUrl": instance, "accessToken": "sf-token"}
	for k, v := range extra {
		src[k] = v
	}
	raw, err := json.Marshal(src)
	if err != nil {
		t.Fatal(err)
	}
	e := &pipelineExecutor{a: &API{}}
	act := pipeline.Activity{Name: "Ingest", Type: "Copy"}
	cfg, err := e.salesforceConfig(act, map[string]json.RawMessage{"source": raw}, literal)
	if err != nil {
		return nil, 0, err
	}
	jobID, err := e.salesforceCreateJob(act, cfg)
	if err != nil {
		return nil, 0, err
	}
	if err := e.salesforceAwaitJob(act, cfg, jobID); err != nil {
		return nil, 0, err
	}
	return e.salesforceResults(act, cfg, jobID)
}

func TestSalesforceRunsTheWholeJobLifecycle(t *testing.T) {
	org, url := newOrg(t, map[string][2]string{
		"": {"Id,Name\n001,Acme\n002,Globex\n", "null"},
	})
	org.pollsBeforeComplete = 2 // spends time InProgress, as a real job does

	tbl, pages, err := sfSource(t, map[string]any{"objectApiName": "Account"}, url)
	if err != nil {
		t.Fatalf("lifecycle failed: %v", err)
	}
	if len(tbl.Rows) != 2 || strings.Join(tbl.Columns, ",") != "Id,Name" {
		t.Fatalf("cols=%v rows=%v", tbl.Columns, tbl.Rows)
	}
	if pages != 1 {
		t.Fatalf("pages = %d", pages)
	}

	// The ORDER of calls is the claim: create, then poll until terminal, then
	// results. A connector that fetched results without polling would still
	// return these rows.
	got := org.seen()
	if !strings.HasPrefix(got[0], "POST ") || !strings.HasSuffix(got[0], "/jobs/query") {
		t.Fatalf("first call = %q, want the job create", got[0])
	}
	if !strings.HasSuffix(got[len(got)-1], "/results") {
		t.Fatalf("last call = %q, want the results fetch", got[len(got)-1])
	}
	polls := 0
	for _, c := range got[1 : len(got)-1] {
		if strings.HasPrefix(c, "GET ") && strings.Contains(c, "/jobs/query/") {
			polls++
		}
	}
	if polls < 3 {
		t.Fatalf("polled %d times, want it to wait out InProgress (%v)", polls, got)
	}
}

func TestSalesforcePagesByLocatorUntilTheStringNull(t *testing.T) {
	// The detail most likely to be got wrong: the END of paging is the LITERAL
	// STRING "null" in Sforce-Locator, not an absent or empty header.
	org, url := newOrg(t, map[string][2]string{
		"":      {"Id,Name\n001,Acme\n", "AQ0x1"},
		"AQ0x1": {"Id,Name\n002,Globex\n", "AQ0x2"},
		"AQ0x2": {"Id,Name\n003,Initech\n", "null"},
	})

	tbl, pages, err := sfSource(t, map[string]any{"objectApiName": "Account"}, url)
	if err != nil {
		t.Fatal(err)
	}
	if pages != 3 || len(tbl.Rows) != 3 {
		t.Fatalf("pages=%d rows=%d, want every page followed", pages, len(tbl.Rows))
	}
	// Each page repeats the header row; only the data accumulates.
	if strings.Join(tbl.Columns, ",") != "Id,Name" {
		t.Fatalf("columns = %v — a repeated header leaked into the rows?", tbl.Columns)
	}
	if fmt.Sprint(tbl.Rows[2][1]) != "Initech" {
		t.Fatalf("rows = %v", tbl.Rows)
	}
	_ = org
}

func TestSalesforceIncludeDeletedObjectsSelectsQueryAll(t *testing.T) {
	// Not a filter applied to results — a DIFFERENT Bulk operation. Treating it
	// as a post-filter would silently drop the rows the author asked for.
	org, url := newOrg(t, map[string][2]string{"": {"Id\n001\n", "null"}})
	if _, _, err := sfSource(t, map[string]any{
		"objectApiName": "Account", "includeDeletedObjects": true}, url); err != nil {
		t.Fatal(err)
	}
	org.mu.Lock()
	op := org.created["operation"]
	org.mu.Unlock()
	if op != "queryAll" {
		t.Fatalf("operation = %v, want queryAll", op)
	}
}

func TestSalesforceSendsTheAuthoredSOQL(t *testing.T) {
	org, url := newOrg(t, map[string][2]string{"": {"Id\n001\n", "null"}})
	soql := "SELECT Id, Name FROM Account WHERE IsDeleted = false"
	if _, _, err := sfSource(t, map[string]any{"query": soql}, url); err != nil {
		t.Fatal(err)
	}
	org.mu.Lock()
	got := org.created["query"]
	org.mu.Unlock()
	if got != soql {
		t.Fatalf("query = %v, want the authored SOQL verbatim", got)
	}
}

func TestSalesforceFailsWhenTheJobFails(t *testing.T) {
	org, url := newOrg(t, map[string][2]string{"": {"Id\n001\n", "null"}})
	org.state = "Failed"

	_, _, err := sfSource(t, map[string]any{"objectApiName": "Account"}, url)
	if err == nil {
		t.Fatal("a Failed job must fail the copy")
	}
	// The job id is what makes this findable in the org's own job monitor, which
	// is where the real answer lives.
	if !strings.Contains(err.Error(), "750xx000000005LAAQ") || !strings.Contains(err.Error(), "Failed") {
		t.Fatalf("error should name the job and its state: %v", err)
	}
}

func TestSalesforceRefusesConfigurationItCannotHonour(t *testing.T) {
	_, url := newOrg(t, map[string][2]string{"": {"Id\n001\n", "null"}})
	for _, tc := range []struct {
		name string
		src  map[string]any
		want string
	}{
		{"reportId", map[string]any{"reportId": "00O123"}, "Analytics REST API"},
		{"no object or query", map[string]any{}, "objectApiName` or a SOQL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := sfSource(t, tc.src, url)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}

	t.Run("missing instanceUrl", func(t *testing.T) {
		raw, _ := json.Marshal(map[string]any{"type": "SalesforceV2Source", "accessToken": "x",
			"objectApiName": "Account"})
		e := &pipelineExecutor{a: &API{}}
		_, err := e.salesforceConfig(pipeline.Activity{Name: "Ingest"},
			map[string]json.RawMessage{"source": raw}, literal)
		if err == nil || !strings.Contains(err.Error(), "instanceUrl") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("missing accessToken", func(t *testing.T) {
		raw, _ := json.Marshal(map[string]any{"type": "SalesforceV2Source", "instanceUrl": url,
			"objectApiName": "Account"})
		e := &pipelineExecutor{a: &API{}}
		_, err := e.salesforceConfig(pipeline.Activity{Name: "Ingest"},
			map[string]json.RawMessage{"source": raw}, literal)
		if err == nil || !strings.Contains(err.Error(), "accessToken") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestSalesforceRejectsABadToken(t *testing.T) {
	// The stand-in enforces the bearer, so this proves the token really travels
	// rather than being assembled and dropped.
	_, url := newOrg(t, map[string][2]string{"": {"Id\n001\n", "null"}})
	raw, _ := json.Marshal(map[string]any{"type": "SalesforceV2Source", "instanceUrl": url,
		"accessToken": "wrong", "objectApiName": "Account"})
	e := &pipelineExecutor{a: &API{}}
	act := pipeline.Activity{Name: "Ingest"}
	cfg, err := e.salesforceConfig(act, map[string]json.RawMessage{"source": raw}, literal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.salesforceCreateJob(act, cfg); err == nil ||
		!strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v, want the 401 surfaced", err)
	}
}

// --- end to end, through a real pipeline run -------------------------------

func TestSalesforceCopyLandsRowsInADeltaTable(t *testing.T) {
	org, url := newOrg(t, map[string][2]string{
		"":    {"Id,Name\n001,Acme\n", "AQ1"},
		"AQ1": {"Id,Name\n002,Globex\n", "null"},
	})
	// The job spends time InProgress so this test — which drives the REAL
	// dispatch path, not the helper — can see whether it was polled to a terminal
	// state before the results were fetched. Mutation testing caught that gap:
	// removing the poll from salesforceToLakehouse left every test green, because
	// the unit helper calls create/await/results itself and cannot observe it.
	org.pollsBeforeComplete = 2

	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lh := seedLakehouse(t, st, ws.ID, "lake")
	loc := `"datasetSettings":{"linkedService":{"properties":{"typeProperties":{"artifactId":"` + lh.ID + `"}}}}`
	content := `{"properties":{"activities":[
      {"name":"Ingest","type":"Copy","typeProperties":{
        "source":{"type":"SalesforceV2Source","instanceUrl":"` + url + `","accessToken":"sf-token","objectApiName":"Account"},
        "sink":{"type":"LakehouseTableSink","table":"bronze_accounts",` + loc + `}
      }}]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
		t.Fatalf("status = %s: %v", s, runs[0]["error"])
	}

	tbl, err := warehouse.ReadDeltaTable(st, lh.ID, "bronze_accounts")
	if err != nil {
		t.Fatalf("the rows must really be committed as Delta: %v", err)
	}
	if len(tbl.Rows) != 2 {
		t.Fatalf("want both pages committed, got %d rows", len(tbl.Rows))
	}

	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	out := runs[0]["output"].(map[string]any)
	if fmt.Sprint(out["resultPages"]) != "2" {
		t.Fatalf("resultPages = %v, want both pages reported", out["resultPages"])
	}
	if fmt.Sprint(out["jobId"]) != "750xx000000005LAAQ" {
		t.Fatalf("the Bulk job id should be reported: %v", out["jobId"])
	}
	edge, ok := out["lineage"].(map[string]any)
	if !ok || edge["sourceKind"] != "connection" {
		t.Fatalf("lineage should mark Salesforce as outside Fabric: %+v", out["lineage"])
	}

	// The lifecycle really ran in order through the dispatch path: create, then
	// polls while InProgress, and only then results.
	seen := org.seen()
	firstResult := -1
	polls := 0
	for i, c := range seen {
		if strings.HasSuffix(c, "/results") {
			if firstResult < 0 {
				firstResult = i
			}
			continue
		}
		if i > 0 && strings.HasPrefix(c, "GET ") {
			polls++
		}
	}
	if polls < 3 {
		t.Fatalf("the copy fetched results after %d polls; it must wait out InProgress (%v)", polls, seen)
	}
	if firstResult < polls {
		t.Fatalf("results were fetched before polling completed: %v", seen)
	}
}
