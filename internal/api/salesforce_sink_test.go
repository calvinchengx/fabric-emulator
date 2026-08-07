package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/warehouse"
)

// The ingest lifecycle is four calls, and three of them are easy to skip without
// the rows visibly failing to arrive: a job left Open uploads data that is never
// processed, and a job never polled reports success before Salesforce has said
// anything. So these assert the SEQUENCE, not just the payload.

type sfIngestOrg struct {
	mu              sync.Mutex
	calls           []string // "METHOD /path" in order
	created         []map[string]any
	uploads         []string
	patched         []map[string]any
	failed          int // numberRecordsFailed the job reports
	state           string
	contentRelative bool
}

func newIngestOrg(t *testing.T) (*sfIngestOrg, string) {
	t.Helper()
	org := &sfIngestOrg{state: "JobComplete"}
	srv := httptest.NewServer(org)
	t.Cleanup(srv.Close)
	return org, srv.URL
}

func (o *sfIngestOrg) seen() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.calls...)
}

func (o *sfIngestOrg) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	o.calls = append(o.calls, r.Method+" "+r.URL.Path)
	o.mu.Unlock()

	if r.Header.Get("Authorization") != "Bearer sf-token" {
		http.Error(w, `[{"errorCode":"INVALID_SESSION_ID"}]`, http.StatusUnauthorized)
		return
	}
	raw, _ := io.ReadAll(r.Body)

	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/jobs/ingest"):
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		o.mu.Lock()
		id := fmt.Sprintf("750J%06d", len(o.created))
		o.created = append(o.created, body)
		o.mu.Unlock()
		content := "/services/data/v59.0/jobs/ingest/" + id + "/batches"
		if o.contentRelative {
			content = strings.TrimPrefix(content, "/")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(`{"id":%q,"contentUrl":%q,"state":"Open"}`, id, content)))

	case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/batches"):
		if ct := r.Header.Get("Content-Type"); ct != "text/csv" {
			t := fmt.Sprintf("Content-Type %q, want text/csv", ct)
			http.Error(w, t, http.StatusUnsupportedMediaType)
			return
		}
		o.mu.Lock()
		o.uploads = append(o.uploads, string(raw))
		o.mu.Unlock()
		w.WriteHeader(http.StatusCreated)

	case r.Method == http.MethodPatch:
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		o.mu.Lock()
		o.patched = append(o.patched, body)
		o.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"state":"UploadComplete"}`))

	case r.Method == http.MethodGet:
		o.mu.Lock()
		state, failed := o.state, o.failed
		o.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(
			`{"state":%q,"errorMessage":"boom","numberRecordsProcessed":%d,"numberRecordsFailed":%d}`,
			state, 2, failed)))

	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}
}

// sfExport runs a Copy from a seeded CSV to a Salesforce sink.
func sfExport(t *testing.T, org *sfIngestOrg, url, sinkProps, csv string) (map[string]any, string) {
	t.Helper()
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lh := seedLakehouse(t, st, ws.ID, "lake")
	seedFile(t, st, ws.ID, lh.ID, "Files/out/rows.csv", []byte(csv))
	content := `{"properties":{"activities":[
      {"name":"Export","type":"Copy","typeProperties":{
        "source":{"type":"DelimitedTextSource","location":{"itemId":"` + lh.ID + `","path":"Files/out/rows.csv"}},
        "sink":{"type":"SalesforceV2Sink","instanceUrl":"` + url + `","accessToken":"sf-token",
                "objectApiName":"Account"` + sinkProps + `}
      }}]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	status := awaitJob(t, a, ws.ID, pl.ID, jid)
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	out, _ := runs[0]["output"].(map[string]any)
	errMsg, _ := runs[0]["error"].(string)
	if status != "Completed" {
		return out, errMsg
	}
	return out, ""
}

func TestSalesforceSinkRunsTheWholeIngestLifecycle(t *testing.T) {
	org, url := newIngestOrg(t)
	out, errMsg := sfExport(t, org, url, "", "Name,Industry\nAcme,Tech\nGlobex,Retail\n")
	if errMsg != "" {
		t.Fatalf("export failed: %s", errMsg)
	}

	// create -> PUT the CSV -> PATCH UploadComplete -> poll. Skipping the PATCH
	// leaves the job Open and the rows are never processed; skipping the poll
	// reports success before Salesforce has said anything.
	got := org.seen()
	want := []string{"POST", "PUT", "PATCH", "GET"}
	if len(got) < 4 {
		t.Fatalf("only %d calls: %v", len(got), got)
	}
	for i, verb := range want {
		if !strings.HasPrefix(got[i], verb+" ") {
			t.Fatalf("call %d = %q, want a %s (sequence: %v)", i, got[i], verb, got)
		}
	}

	if len(org.uploads) != 1 {
		t.Fatalf("uploads = %d", len(org.uploads))
	}
	// The payload is CSV with a header row, which is Bulk 2.0's contract.
	if !strings.HasPrefix(org.uploads[0], "Name,Industry\n") ||
		!strings.Contains(org.uploads[0], "Acme,Tech") {
		t.Fatalf("upload payload = %q", org.uploads[0])
	}
	if org.patched[0]["state"] != "UploadComplete" {
		t.Fatalf("patch body = %+v", org.patched[0])
	}
	if org.created[0]["operation"] != "insert" || org.created[0]["object"] != "Account" {
		t.Fatalf("create body = %+v", org.created[0])
	}
	if fmt.Sprint(out["rowsCopied"]) != "2" || fmt.Sprint(out["jobsWritten"]) != "1" {
		t.Fatalf("output = %+v", out)
	}
}

func TestSalesforceSinkOneJobPerWriteBatchSize(t *testing.T) {
	org, url := newIngestOrg(t)
	csv := "Name\nA\nB\nC\nD\nE\n"
	out, errMsg := sfExport(t, org, url, `,"writeBatchSize":2`, csv)
	if errMsg != "" {
		t.Fatal(errMsg)
	}
	if fmt.Sprint(out["jobsWritten"]) != "3" {
		t.Fatalf("jobsWritten = %v, want 3 (5 rows at 2 per batch)", out["jobsWritten"])
	}
	// Every row exactly once, in order — a repeated final batch would still total 5.
	var names []string
	for _, up := range org.uploads {
		for _, line := range strings.Split(strings.TrimSpace(up), "\n")[1:] {
			names = append(names, strings.TrimSpace(line))
		}
	}
	if strings.Join(names, ",") != "A,B,C,D,E" {
		t.Fatalf("rows across batches = %v", names)
	}
	// Each batch is its own job: create/upload/close/poll, three times over.
	posts := 0
	for _, c := range org.seen() {
		if strings.HasPrefix(c, "POST ") {
			posts++
		}
	}
	if posts != 3 {
		t.Fatalf("%d job creates for 3 batches", posts)
	}
}

func TestSalesforceSinkKeepsEmptyStringsDistinctFromNull(t *testing.T) {
	// A CSV source's empty field parses as the STRING "", not nil — and the two
	// must stay distinct. Conflating them would send `#N/A` for an empty string
	// and so make it impossible to write one; the null sentinel is reserved for a
	// value that is genuinely absent. (The nil path is exercised directly in
	// TestSalesforceCSVNullHandlingInverts, where a real nil can be constructed.)
	org, url := newIngestOrg(t)
	if _, errMsg := sfExport(t, org, url, "", "Name,Industry\nAcme,\n"); errMsg != "" {
		t.Fatal(errMsg)
	}
	rows := strings.Split(strings.TrimSpace(org.uploads[0]), "\n")
	if len(rows) < 2 || strings.TrimSpace(rows[1]) != "Acme," {
		t.Fatalf("an empty string must stay empty, not become the null sentinel: %q", org.uploads[0])
	}
}

func TestSalesforceSinkUpsertNeedsAnExternalId(t *testing.T) {
	org, url := newIngestOrg(t)

	// Missing: refused here, naming the property, rather than relaying an API
	// error about a request we composed.
	_, errMsg := sfExport(t, org, url, `,"writeBehavior":"Upsert"`, "Name\nA\n")
	if !strings.Contains(errMsg, "externalIdFieldName") {
		t.Fatalf("err = %q", errMsg)
	}

	// Present: it reaches the job create, which is what Salesforce matches on.
	org2, url2 := newIngestOrg(t)
	if _, errMsg := sfExport(t, org2, url2,
		`,"writeBehavior":"Upsert","externalIdFieldName":"External_Id__c"`, "Name\nA\n"); errMsg != "" {
		t.Fatal(errMsg)
	}
	if org2.created[0]["operation"] != "upsert" ||
		org2.created[0]["externalIdFieldName"] != "External_Id__c" {
		t.Fatalf("create body = %+v", org2.created[0])
	}
}

func TestSalesforceSinkRefusesAPartialWrite(t *testing.T) {
	// A job can reach JobComplete with records rejected. Reporting that as
	// Succeeded is a partial write presented as a whole one.
	org, url := newIngestOrg(t)
	org.failed = 1

	_, errMsg := sfExport(t, org, url, "", "Name\nA\nB\n")
	if errMsg == "" {
		t.Fatal("records rejected must fail the copy")
	}
	if !strings.Contains(errMsg, "failedResults") || !strings.Contains(errMsg, "partial") {
		t.Fatalf("the error should point at the per-row reasons: %q", errMsg)
	}
}

func TestSalesforceSinkFailsWhenTheJobFails(t *testing.T) {
	org, url := newIngestOrg(t)
	org.state = "Failed"
	_, errMsg := sfExport(t, org, url, "", "Name\nA\n")
	if !strings.Contains(errMsg, "Failed") || !strings.Contains(errMsg, "750J") {
		t.Fatalf("error should name the job and its state: %q", errMsg)
	}
}

func TestSalesforceSinkResolvesARelativeContentUrl(t *testing.T) {
	// Salesforce returns contentUrl relative to the instance more often than not.
	org, url := newIngestOrg(t)
	org.contentRelative = true
	if _, errMsg := sfExport(t, org, url, "", "Name\nA\n"); errMsg != "" {
		t.Fatalf("a relative contentUrl must resolve against the instance: %s", errMsg)
	}
	if len(org.uploads) != 1 {
		t.Fatalf("the upload did not land: %d", len(org.uploads))
	}
}

func TestSalesforceSinkRefusesConfigurationFabricDoesNotExpose(t *testing.T) {
	org, url := newIngestOrg(t)
	for _, tc := range []struct{ name, props, want string }{
		{"delete", `,"writeBehavior":"delete"`, "Insert and Upsert"},
		{"externalId without upsert", `,"externalIdFieldName":"X__c"`, "only applies to an upsert"},
		{"zero batch", `,"writeBatchSize":"0"`, "positive number"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, errMsg := sfExport(t, org, url, tc.props, "Name\nA\n")
			if !strings.Contains(errMsg, tc.want) {
				t.Fatalf("err = %q, want %q", errMsg, tc.want)
			}
		})
	}
}

func TestSalesforceCSVNullHandlingInverts(t *testing.T) {
	// The subtle one, and the reason ignoreNullValues cannot just always write "".
	// In Bulk CSV an EMPTY field means "leave this unchanged"; the literal `#N/A`
	// means "set it to NULL". So ignoreNullValues=true writes empty and =false
	// writes the sentinel — Fabric's documented wording, and backwards either way
	// silently wipes fields or silently fails to.
	tbl := &warehouse.Table{Columns: []string{"a", "b"}, Rows: [][]any{{"x", nil}}}

	if got := string(salesforceCSV(tbl, tbl.Rows, false)); !strings.Contains(got, "x,#N/A") {
		t.Fatalf("ignoreNullValues=false must SET NULL via the sentinel: %q", got)
	}
	if got := string(salesforceCSV(tbl, tbl.Rows, true)); !strings.Contains(got, "x,\n") &&
		!strings.HasSuffix(strings.TrimRight(got, "\n"), "x,") {
		t.Fatalf("ignoreNullValues=true must LEAVE UNCHANGED via an empty field: %q", got)
	}
}

func TestSalesforceCSVQuotesDelimiters(t *testing.T) {
	// A comma inside a value must be quoted, or every following field shifts one
	// column left and the record is silently wrong rather than rejected.
	tbl := &warehouse.Table{Columns: []string{"a", "b"}, Rows: [][]any{{1.0, "y,z"}}}
	if got := string(salesforceCSV(tbl, tbl.Rows, false)); !strings.Contains(got, `"y,z"`) {
		t.Fatalf("a comma in a value must be quoted: %q", got)
	}
}
