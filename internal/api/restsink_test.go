package api

import (
	"compress/gzip"
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

// The sink's payload spec is one line — batches of writeBatchSize records, each
// a JSON array of row objects — so the BATCHING is what these assert. A sink
// that sent every row in one request, or repeated the last batch, still reports
// a plausible total; only the per-request payloads distinguish them.

type sinkRec struct {
	mu      sync.Mutex
	batches [][]map[string]any
	methods []string
	headers []http.Header
}

func (s *sinkRec) handler(t *testing.T, status int) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		var body io.Reader = r.Body
		if r.Header.Get("Content-Encoding") == "gzip" {
			zr, err := gzip.NewReader(r.Body)
			if err != nil {
				t.Errorf("gzip declared but not readable: %v", err)
				w.WriteHeader(500)
				return
			}
			body = zr
		}
		raw, _ := io.ReadAll(body)
		var batch []map[string]any
		if err := json.Unmarshal(raw, &batch); err != nil {
			t.Errorf("payload is not a JSON array of objects: %v (%s)", err, raw)
		}
		s.mu.Lock()
		s.batches = append(s.batches, batch)
		s.methods = append(s.methods, r.Method)
		s.headers = append(s.headers, r.Header.Clone())
		s.mu.Unlock()
		w.WriteHeader(status)
	}
}

func (s *sinkRec) sizes() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]int, len(s.batches))
	for i, b := range s.batches {
		out[i] = len(b)
	}
	return out
}

// exportPipeline runs a Copy from a seeded CSV file to a RestSink.
func exportPipeline(t *testing.T, sinkProps string, csv string) (*sinkRec, map[string]any, string) {
	rec, out, status, _ := exportPipelineE(t, sinkProps, csv)
	return rec, out, status
}

// exportPipelineE additionally returns the activity's error string. A failed
// activity has an empty output, so the message lives in `error` — reading the
// wrong field is how a refusal test passes on any failure at all.
func exportPipelineE(t *testing.T, sinkProps string, csv string) (*sinkRec, map[string]any, string, string) {
	t.Helper()
	rec := &sinkRec{}
	srv := httptest.NewServer(rec.handler(t, http.StatusOK))
	t.Cleanup(srv.Close)

	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lh := seedLakehouse(t, st, ws.ID, "lake")
	seedFile(t, st, ws.ID, lh.ID, "Files/out/rows.csv", []byte(csv))

	content := `{"properties":{"activities":[
      {"name":"Export","type":"Copy","typeProperties":{
        "source":{"type":"DelimitedTextSource","location":{"itemId":"` + lh.ID + `","path":"Files/out/rows.csv"}},
        "sink":{"type":"RestSink","url":"` + srv.URL + `/ingest"` + sinkProps + `}
      }}]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	status := awaitJob(t, a, ws.ID, pl.ID, jid)
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	out, _ := runs[0]["output"].(map[string]any)
	errMsg, _ := runs[0]["error"].(string)
	return rec, out, status, errMsg
}

func TestRestSinkPostsRowsAsJSONArrays(t *testing.T) {
	rec, out, status := exportPipeline(t, "", "id,name\n1,ada\n2,grace\n")
	if status != "Completed" {
		t.Fatalf("status = %s", status)
	}
	if got := rec.sizes(); len(got) != 1 || got[0] != 2 {
		t.Fatalf("batch sizes = %v, want one batch of 2", got)
	}
	if rec.methods[0] != http.MethodPost {
		t.Fatalf("method = %s, want POST by default", rec.methods[0])
	}
	if ct := rec.headers[0].Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
	// The rows arrive as objects keyed by column, which is Fabric's payload shape.
	if rec.batches[0][0]["name"] != "ada" || rec.batches[0][1]["id"] != "2" {
		t.Fatalf("payload objects = %+v", rec.batches[0])
	}
	if fmt.Sprint(out["rowsCopied"]) != "2" || fmt.Sprint(out["batchesWritten"]) != "1" {
		t.Fatalf("output = %+v", out)
	}
}

func TestRestSinkSplitsIntoBatchesOfWriteBatchSize(t *testing.T) {
	// 5 rows at 2 per batch is 3 requests sized 2,2,1 — the assertion that
	// separates real batching from one big request with a plausible total.
	csv := "id\n1\n2\n3\n4\n5\n"
	rec, out, status := exportPipeline(t, `,"writeBatchSize":2`, csv)
	if status != "Completed" {
		t.Fatalf("status = %s", status)
	}
	if got := rec.sizes(); fmt.Sprint(got) != "[2 2 1]" {
		t.Fatalf("batch sizes = %v, want [2 2 1]", got)
	}
	if fmt.Sprint(out["batchesWritten"]) != "3" || fmt.Sprint(out["rowsCopied"]) != "5" {
		t.Fatalf("output = %+v", out)
	}
	// Every row exactly once, in order — a repeated final batch would still total 5.
	var ids []string
	for _, b := range rec.batches {
		for _, row := range b {
			ids = append(ids, fmt.Sprint(row["id"]))
		}
	}
	if strings.Join(ids, ",") != "1,2,3,4,5" {
		t.Fatalf("rows across batches = %v", ids)
	}
}

func TestRestSinkSendsNothingForZeroRows(t *testing.T) {
	// An empty array is a write the author never asked for, and plenty of APIs
	// read `[]` as "replace with nothing".
	rec, out, status := exportPipeline(t, "", "id,name\n")
	if status != "Completed" {
		t.Fatalf("status = %s", status)
	}
	if n := len(rec.sizes()); n != 0 {
		t.Fatalf("%d requests for zero rows, want none", n)
	}
	if fmt.Sprint(out["batchesWritten"]) != "0" {
		t.Fatalf("batchesWritten = %v", out["batchesWritten"])
	}
}

func TestRestSinkGzipsWhenAsked(t *testing.T) {
	// The handler decompresses, so this fails if the body is not really gzip.
	rec, _, status := exportPipeline(t, `,"httpCompressionType":"gzip"`, "id\n1\n2\n")
	if status != "Completed" {
		t.Fatalf("status = %s", status)
	}
	if enc := rec.headers[0].Get("Content-Encoding"); enc != "gzip" {
		t.Fatalf("Content-Encoding = %q", enc)
	}
	if got := rec.sizes(); len(got) != 1 || got[0] != 2 {
		t.Fatalf("gzip payload did not decode to the rows: %v", got)
	}
}

func TestRestSinkHonoursPutAndPatch(t *testing.T) {
	for _, m := range []string{"PUT", "PATCH"} {
		t.Run(m, func(t *testing.T) {
			rec, _, status := exportPipeline(t, `,"requestMethod":"`+m+`"`, "id\n1\n")
			if status != "Completed" {
				t.Fatalf("status = %s", status)
			}
			if rec.methods[0] != m {
				t.Fatalf("method = %s, want %s", rec.methods[0], m)
			}
		})
	}
}

func TestRestSinkRefusesConfigurationFabricDoesNotAllow(t *testing.T) {
	for _, tc := range []struct{ name, props, want string }{
		{"DELETE", `,"requestMethod":"DELETE"`, "POST, PUT and PATCH"},
		{"bad compression", `,"httpCompressionType":"brotli"`, "none and gzip"},
		{"interval too large", `,"requestInterval":"600000"`, "outside Fabric's"},
		{"interval too small", `,"requestInterval":"1"`, "outside Fabric's"},
		{"zero batch size", `,"writeBatchSize":"0"`, "positive number"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, status, errMsg := exportPipelineE(t, tc.props, "id\n1\n")
			if status == "Completed" {
				t.Fatalf("%s must fail the copy", tc.name)
			}
			// The refusal has to NAME the constraint, not just fail.
			if !strings.Contains(errMsg, tc.want) {
				t.Fatalf("error should mention %q; got %q", tc.want, errMsg)
			}
		})
	}
}

func TestRestSinkFailsOnNon2xx(t *testing.T) {
	rec := &sinkRec{}
	srv := httptest.NewServer(rec.handler(t, http.StatusForbidden))
	defer srv.Close()

	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	lh := seedLakehouse(t, st, ws.ID, "lake")
	seedFile(t, st, ws.ID, lh.ID, "Files/out/rows.csv", []byte("id\n1\n"))
	content := `{"properties":{"activities":[
      {"name":"Export","type":"Copy","typeProperties":{
        "source":{"type":"DelimitedTextSource","location":{"itemId":"` + lh.ID + `","path":"Files/out/rows.csv"}},
        "sink":{"type":"RestSink","url":"` + srv.URL + `/ingest"}
      }}]}}`
	pl := createPipeline(t, st, ws.ID, content)
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s == "Completed" {
		t.Fatal("a 403 must fail the copy — a partial export reported as success is the worst outcome here")
	}
}

func TestRestSinkRefusesASourceWithNoRows(t *testing.T) {
	// Two ways a source has no rows to send, and both must refuse rather than
	// post something arbitrary.
	//
	// The BinarySource case is the sharp one: lookupFormat FALLS BACK to "csv"
	// for an unrecognised path, so without an explicit check a binary file is
	// parsed as text and its garbage rows are posted with a Succeeded beside
	// them. That fallback is harmless for a OneLake byte copy — the bytes move
	// either way — and wrong here.
	for _, tc := range []struct{ name, srcType, path, want string }{
		{"declared binary", "BinarySource", "Files/blob/data.bin", "opaque bytes have no rows"},
		{"a directory", "DelimitedTextSource", "Files/blob", "must be rows the emulator can read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &sinkRec{}
			srv := httptest.NewServer(rec.handler(t, http.StatusOK))
			defer srv.Close()

			a, st := newAPI(t)
			ws := seedWorkspace(t, st)
			lh := seedLakehouse(t, st, ws.ID, "lake")
			seedFile(t, st, ws.ID, lh.ID, "Files/blob/data.bin", []byte("\x00\x01binary"))
			content := `{"properties":{"activities":[
      {"name":"Export","type":"Copy","typeProperties":{
        "source":{"type":"` + tc.srcType + `","location":{"itemId":"` + lh.ID + `","path":"` + tc.path + `"}},
        "sink":{"type":"RestSink","url":"` + srv.URL + `/ingest"}
      }}]}}`
			pl := createPipeline(t, st, ws.ID, content)
			_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
			if s := awaitJob(t, a, ws.ID, pl.ID, jid); s == "Completed" {
				t.Fatal("must fail rather than post something arbitrary")
			}
			_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
			if e, _ := runs[0]["error"].(string); !strings.Contains(e, tc.want) {
				t.Fatalf("error should mention %q; got %q", tc.want, e)
			}
			if n := len(rec.sizes()); n != 0 {
				t.Fatalf("nothing should have been sent, got %d requests", n)
			}
		})
	}
}
func TestRestSinkCarriesAdditionalHeaders(t *testing.T) {
	rec, _, status := exportPipeline(t,
		`,"additionalHeaders":{"Authorization":"AR-JWT t0ken","X-Batch":"export"}`, "id\n1\n")
	if status != "Completed" {
		t.Fatalf("status = %s", status)
	}
	if got := rec.headers[0].Get("Authorization"); got != "AR-JWT t0ken" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := rec.headers[0].Get("X-Batch"); got != "export" {
		t.Fatalf("X-Batch = %q", got)
	}
}

func TestRestRowObjectsOmitsAbsentValues(t *testing.T) {
	// null means "set this to nothing" to a good many APIs, which is a different
	// instruction from "I have nothing to say about this field".
	tbl := &warehouse.Table{Columns: []string{"a", "b"}, Rows: [][]any{{1.0, nil}, {2.0, "x"}}}
	rows := restRowObjects(tbl)
	if _, present := rows[0]["b"]; present {
		t.Fatalf("an absent value must be omitted, not sent as null: %+v", rows[0])
	}
	if rows[1]["b"] != "x" {
		t.Fatalf("a present value must survive: %+v", rows[1])
	}
}
