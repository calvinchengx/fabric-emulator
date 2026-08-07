package api

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/pipeline"
	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/calvinchengx/fabric-emulator/internal/warehouse"
)

// Salesforce ingest (S2 of docs/41-salesforce-connector-plan.md).
//
// The write half of Bulk API 2.0, and a longer lifecycle than the read half:
//
//	POST  /jobs/ingest            -> { id, contentUrl }
//	PUT   {contentUrl}            text/csv, the rows
//	PATCH /jobs/ingest/{id}       { "state": "UploadComplete" }
//	GET   /jobs/ingest/{id}       poll to JobComplete / Failed / Aborted
//
// The upload verb is **PUT**. A doc summary said POST while scoping this, and
// the reference says PUT — recorded in docs/41 because a wrong verb here would
// have made the whole path fiction.

const (
	// Fabric's documented default for the Salesforce sink.
	salesforceWriteBatch = 100000
	// Salesforce's sentinel for "set this field to NULL". An EMPTY field means
	// something different — "leave it unchanged" — which is the whole reason
	// ignoreNullValues has to pick between them rather than always writing "".
	salesforceNullSentinel = "#N/A"
)

var salesforceSinkTypes = map[string]bool{
	"SalesforceV2Sink":             true,
	"SalesforceServiceCloudV2Sink": true,
}

type salesforceSinkCfg struct {
	salesforceCfg
	object      string
	operation   string // insert | upsert
	externalID  string
	ignoreNulls bool
	batchSize   int
}

// salesforceFromLakehouse reads the source rows and ingests them.
func (e *pipelineExecutor) salesforceFromLakehouse(
	act pipeline.Activity,
	tp map[string]json.RawMessage,
	resolve func(json.RawMessage) (any, error),
) (map[string]any, error) {
	src, err := e.resolveLoc("source", tp["source"], resolve)
	if err != nil {
		return nil, fmt.Errorf("copy %q source: %w", act.Name, err)
	}
	// Same reasoning as the REST sink: a BinarySource is the author saying the
	// source is opaque bytes, and lookupFormat's fallback is "csv", so without an
	// explicit refusal a binary file would be parsed as text and ingested as
	// garbage records into someone's org.
	if t, err := copySideType(tp["source"], resolve); err != nil {
		return nil, fmt.Errorf("copy %q source: %w", act.Name, err)
	} else if t == "BinarySource" {
		return nil, fmt.Errorf("copy %q: a Salesforce sink cannot take a BinarySource — Bulk API 2.0 "+
			"ingests CSV records, and opaque bytes have no records", act.Name)
	}
	tbl, readable, err := e.readTabularSource(act, tp, src)
	if err != nil {
		return nil, err
	}
	if !readable {
		return nil, fmt.Errorf("copy %q: a Salesforce sink source must be rows the emulator can read "+
			"— a Delta table (`Tables/<name>`), or a standalone Parquet or CSV file; %q is neither",
			act.Name, src.path)
	}

	cfg, err := e.salesforceSinkConfig(act, tp, resolve)
	if err != nil {
		return nil, err
	}

	jobs := make([]string, 0, 1)
	sent := 0
	for start := 0; start < len(tbl.Rows); start += cfg.batchSize {
		end := min(start+cfg.batchSize, len(tbl.Rows))
		jobID, err := e.salesforceIngestBatch(act, cfg, tbl, tbl.Rows[start:end])
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, jobID)
		sent += end - start
	}

	edge := &store.LineageEdge{WorkspaceID: e.wid, JobID: e.jobID, ActivityName: act.Name,
		Producer:          store.ProducerCopy,
		SourceWorkspaceID: src.wsID, SourceItemID: src.itemID, SourcePath: src.path,
		TargetPath: cfg.instance + "/" + cfg.object}
	if err := e.a.Store.CreateLineageEdge(edge); err != nil {
		return nil, fmt.Errorf("copy %q lineage: %v", act.Name, err)
	}

	return map[string]any{
		"rowsRead": len(tbl.Rows), "rowsCopied": sent,
		"jobsWritten": len(jobs), "jobIds": jobs,
		"objectApiName": cfg.object, "writeBehavior": cfg.operation,
		"writeBatchSize": cfg.batchSize, "copyDuration": 0, "lineage": edge,
	}, nil
}

func (e *pipelineExecutor) salesforceSinkConfig(
	act pipeline.Activity,
	tp map[string]json.RawMessage,
	resolve func(json.RawMessage) (any, error),
) (*salesforceSinkCfg, error) {
	var sink map[string]json.RawMessage
	if err := json.Unmarshal(tp["sink"], &sink); err != nil {
		return nil, fmt.Errorf("copy %q: sink is not an object", act.Name)
	}
	str := func(key string) (string, error) {
		raw, ok := sink[key]
		if !ok || len(raw) == 0 {
			return "", nil
		}
		v, err := resolve(raw)
		if err != nil {
			return "", fmt.Errorf("copy %q: sink %s: %w", act.Name, key, err)
		}
		if v == nil {
			return "", nil
		}
		return strings.TrimSpace(fmt.Sprint(v)), nil
	}

	base, err := e.salesforceConnection(act, "sink", sink, resolve)
	if err != nil {
		return nil, err
	}
	cfg := &salesforceSinkCfg{salesforceCfg: *base, operation: "insert",
		batchSize: salesforceWriteBatch}

	if cfg.object, err = str("objectApiName"); err != nil {
		return nil, err
	}
	if cfg.object == "" {
		return nil, fmt.Errorf("copy %q: a Salesforce sink needs an `objectApiName`", act.Name)
	}

	if w, err := str("writeBehavior"); err != nil {
		return nil, err
	} else if w != "" {
		switch strings.ToLower(w) {
		case "insert":
			cfg.operation = "insert"
		case "upsert":
			cfg.operation = "upsert"
		default:
			// Bulk 2.0 also has update, delete and hardDelete, but Fabric's sink
			// exposes only these two — and a `delete` reaching a real org because
			// the emulator was permissive is not a risk worth taking.
			return nil, fmt.Errorf("copy %q: Salesforce writeBehavior %q is not allowed "+
				"(Fabric exposes Insert and Upsert)", act.Name, w)
		}
	}

	if cfg.externalID, err = str("externalIdFieldName"); err != nil {
		return nil, err
	}
	if cfg.operation == "upsert" && cfg.externalID == "" {
		// Salesforce rejects the job without it; failing here names the property
		// instead of relaying an API error about a request we composed.
		return nil, fmt.Errorf("copy %q: Salesforce upsert needs an `externalIdFieldName` — it is "+
			"the field the upsert matches on", act.Name)
	}
	if cfg.operation == "insert" && cfg.externalID != "" {
		return nil, fmt.Errorf("copy %q: `externalIdFieldName` only applies to an upsert; "+
			"writeBehavior is Insert", act.Name)
	}

	if s, err := str("ignoreNullValues"); err != nil {
		return nil, err
	} else if s != "" && !strings.EqualFold(s, "false") {
		cfg.ignoreNulls = true
	}

	if s, err := str("writeBatchSize"); err != nil {
		return nil, err
	} else if s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("copy %q: Salesforce writeBatchSize %q is not a positive number",
				act.Name, s)
		}
		cfg.batchSize = n
	}
	return cfg, nil
}

// salesforceIngestBatch runs one create → upload → close → poll cycle.
func (e *pipelineExecutor) salesforceIngestBatch(
	act pipeline.Activity, cfg *salesforceSinkCfg, tbl *warehouse.Table, rows [][]any,
) (string, error) {
	create := map[string]any{
		"object": cfg.object, "operation": cfg.operation,
		"contentType": "CSV", "lineEnding": "LF",
	}
	if cfg.externalID != "" {
		create["externalIdFieldName"] = cfg.externalID
	}
	body, err := json.Marshal(create)
	if err != nil {
		return "", err
	}
	_, raw, err := e.salesforceDo(act, &cfg.salesforceCfg, http.MethodPost,
		cfg.instance+"/services/data/"+cfg.apiVersion+"/jobs/ingest", string(body), "application/json")
	if err != nil {
		return "", err
	}
	var job struct {
		ID         string `json:"id"`
		ContentURL string `json:"contentUrl"`
	}
	if err := json.Unmarshal(raw, &job); err != nil {
		return "", fmt.Errorf("copy %q: the ingest-create response is not JSON: %v", act.Name, err)
	}
	if job.ID == "" {
		return "", fmt.Errorf("copy %q: Salesforce accepted the ingest job but returned no id: %s",
			act.Name, snippet(raw))
	}

	// PUT, not POST. Salesforce returns contentUrl relative to the instance more
	// often than not, so it is resolved rather than trusted to be absolute.
	upload := job.ContentURL
	switch {
	case upload == "":
		upload = cfg.instance + "/services/data/" + cfg.apiVersion + "/jobs/ingest/" + job.ID + "/batches"
	case strings.HasPrefix(upload, "http://"), strings.HasPrefix(upload, "https://"):
	default:
		upload = cfg.instance + "/" + strings.TrimPrefix(upload, "/")
	}
	if err := e.salesforceUpload(act, &cfg.salesforceCfg, upload, salesforceCSV(tbl, rows, cfg.ignoreNulls)); err != nil {
		return "", err
	}

	// Closing the job is what starts processing. A job left Open is a silent
	// no-op: the rows are uploaded and nothing ever happens to them.
	closeBody, _ := json.Marshal(map[string]any{"state": "UploadComplete"})
	if _, _, err := e.salesforceDo(act, &cfg.salesforceCfg, http.MethodPatch,
		cfg.instance+"/services/data/"+cfg.apiVersion+"/jobs/ingest/"+job.ID,
		string(closeBody), "application/json"); err != nil {
		return "", err
	}

	return job.ID, e.salesforceAwaitIngest(act, &cfg.salesforceCfg, job.ID)
}

// salesforceUpload PUTs the CSV batch.
func (e *pipelineExecutor) salesforceUpload(
	act pipeline.Activity, cfg *salesforceCfg, url string, body []byte,
) error {
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("copy %q: %w", act.Name, err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.token)
	req.Header.Set("Content-Type", "text/csv")
	resp, err := e.a.webClient().Do(req)
	if err != nil {
		return fmt.Errorf("copy %q: PUT %s: %w", act.Name, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("copy %q: uploading the batch to %s returned %d", act.Name, url, resp.StatusCode)
	}
	return nil
}

// salesforceAwaitIngest polls the ingest job and refuses a partial write.
func (e *pipelineExecutor) salesforceAwaitIngest(act pipeline.Activity, cfg *salesforceCfg, jobID string) error {
	url := cfg.instance + "/services/data/" + cfg.apiVersion + "/jobs/ingest/" + jobID
	state := ""
	for i := 0; i < salesforceMaxPolls; i++ {
		_, raw, err := e.salesforceDo(act, cfg, http.MethodGet, url, "", "application/json")
		if err != nil {
			return err
		}
		var job struct {
			State     string `json:"state"`
			Message   string `json:"errorMessage"`
			Processed int    `json:"numberRecordsProcessed"`
			Failed    int    `json:"numberRecordsFailed"`
		}
		if err := json.Unmarshal(raw, &job); err != nil {
			return fmt.Errorf("copy %q: the ingest-status response is not JSON: %v", act.Name, err)
		}
		state = job.State
		switch state {
		case "JobComplete":
			// A job can reach JobComplete with records rejected. Reporting that as
			// Succeeded is a PARTIAL WRITE presented as a whole one — the failure
			// mode worth refusing, and the failedResults endpoint is where the
			// per-row reasons live.
			if job.Failed > 0 {
				return fmt.Errorf("copy %q: Salesforce job %s completed with %d of %d records "+
					"rejected — the write is partial; GET /jobs/ingest/%s/failedResults has the "+
					"per-row reasons", act.Name, jobID, job.Failed, job.Processed+job.Failed, jobID)
			}
			return nil
		case "Failed", "Aborted":
			return fmt.Errorf("copy %q: Salesforce ingest job %s ended %s: %s",
				act.Name, jobID, state, job.Message)
		}
	}
	return fmt.Errorf("copy %q: Salesforce ingest job %s never reached a terminal state — last seen "+
		"%q after %d polls (states are %s)", act.Name, jobID, state, salesforceMaxPolls, salesforceJobStates)
}

// salesforceCSV renders rows as the Bulk API's CSV payload.
//
// The null handling is the subtle part and it INVERTS: an empty CSV field means
// "leave this unchanged", while the literal `#N/A` means "set it to NULL". So
// ignoreNullValues=true writes empty and =false writes the sentinel — which is
// exactly Fabric's documented wording, and backwards either way would silently
// wipe fields or silently fail to.
func salesforceCSV(tbl *warehouse.Table, rows [][]any, ignoreNulls bool) []byte {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	_ = w.Write(tbl.Columns)
	for _, row := range rows {
		rec := make([]string, len(tbl.Columns))
		for i := range tbl.Columns {
			if i >= len(row) || row[i] == nil {
				if !ignoreNulls {
					rec[i] = salesforceNullSentinel
				}
				continue
			}
			rec[i] = fmt.Sprint(row[i])
		}
		_ = w.Write(rec)
	}
	w.Flush()
	return buf.Bytes()
}
