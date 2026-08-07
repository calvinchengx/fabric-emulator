package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/httpx"
	"github.com/calvinchengx/fabric-emulator/internal/pipeline"
	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/calvinchengx/fabric-emulator/internal/warehouse"
)

// Salesforce (S1 of docs/41-salesforce-connector-plan.md).
//
// Unlike BMC Helix, Salesforce HAS a first-party Fabric connector, so there is a
// real surface to match. And unlike the REST connector, it is not a request: it
// is Bulk API 2.0, a JOB LIFECYCLE — create a query job, poll it to a terminal
// state, then download CSV result sets paged by an opaque locator.
//
// That is why this is a separate file rather than a RestSource variant. The only
// thing the two share is bounded HTTP.

const (
	// v50.0 is where Salesforce began guaranteeing that result CSV columns come
	// back in the query's column order. Below that the header row is the only
	// thing tying values to fields, and it is not ordered — so this default is a
	// correctness floor, not a preference.
	salesforceAPIVersion = "v59.0"
	// Result pages. Same ceilings and the same reasoning as the REST connector:
	// the rows are held in memory and committed to Delta.
	salesforceMaxPageBytes = 8 << 20
	salesforceMaxRows      = 1_000_000
	// A job that never terminates must not hang a pipeline. Bounded polls with a
	// short wait, so a stand-in that answers JobComplete immediately costs one
	// round trip and a real job still gets a couple of minutes.
	salesforceMaxPolls  = 600
	salesforcePollWait  = 200 * time.Millisecond
	salesforceMaxPages  = 1000
	salesforceJobStates = "Open, UploadComplete, InProgress, JobComplete, Failed, Aborted"
)

// salesforceSourceTypes are the Copy source discriminators this handles. Fabric
// ships a V2 connector; the unsuffixed name is the older one and reaches the
// same place.
var salesforceSourceTypes = map[string]bool{
	"SalesforceV2Source":             true,
	"SalesforceServiceCloudV2Source": true,
}

// salesforceCfg is the resolved connection and query.
type salesforceCfg struct {
	instance   string
	token      string
	apiVersion string
	operation  string // query | queryAll
	soql       string
	timeout    time.Duration
}

// salesforceToLakehouse runs the Bulk API 2.0 query lifecycle and commits the
// rows to the sink's Delta table.
func (e *pipelineExecutor) salesforceToLakehouse(
	act pipeline.Activity,
	tp map[string]json.RawMessage,
	resolve func(json.RawMessage) (any, error),
) (map[string]any, error) {
	dst, err := e.resolveLoc("sink", tp["sink"], resolve)
	if err != nil {
		return nil, fmt.Errorf("copy %q sink: %w", act.Name, err)
	}
	table, ok := deltaTableName(dst.path)
	if !ok {
		return nil, fmt.Errorf("copy %q: a Salesforce copy must land in a lakehouse table "+
			"(sink path `Tables/<name>`), got %q", act.Name, dst.path)
	}

	cfg, err := e.salesforceConfig(act, tp, resolve)
	if err != nil {
		return nil, err
	}

	jobID, err := e.salesforceCreateJob(act, cfg)
	if err != nil {
		return nil, err
	}
	if err := e.salesforceAwaitJob(act, cfg, jobID); err != nil {
		return nil, err
	}
	tbl, pages, err := e.salesforceResults(act, cfg, jobID)
	if err != nil {
		return nil, err
	}

	mode := warehouse.WriteOverwrite
	if action, err := e.copySinkAction(tp, resolve); err != nil {
		return nil, err
	} else if strings.EqualFold(action, "Append") {
		mode = warehouse.WriteAppend
	}
	if err := warehouse.WriteDeltaTableAs(store.ActivityBy(e.jobID, act.Name),
		e.a.Store, dst.wsID, dst.itemID, table, mode, tbl); err != nil {
		return nil, fmt.Errorf("copy %q: writing table %s: %v", act.Name, table, err)
	}

	// The source is outside Fabric — SourceKindConnection is what makes the portal
	// draw it as an external node rather than hunt for an item.
	edge := &store.LineageEdge{WorkspaceID: e.wid, JobID: e.jobID, ActivityName: act.Name,
		Producer:          store.ProducerCopy,
		SourceKind:        store.SourceKindConnection,
		SourcePath:        cfg.instance + "/" + cfg.apiVersion + " " + cfg.soql,
		TargetWorkspaceID: dst.wsID, TargetItemID: dst.itemID, TargetPath: dst.path}
	if err := e.a.Store.CreateLineageEdge(edge); err != nil {
		return nil, fmt.Errorf("copy %q lineage: %v", act.Name, err)
	}

	return map[string]any{
		"rowsRead": len(tbl.Rows), "rowsCopied": len(tbl.Rows),
		"resultPages": pages, "jobId": jobID, "operation": cfg.operation,
		"soql": cfg.soql, "copyDuration": 0, "writeBehavior": mode, "lineage": edge,
	}, nil
}

// salesforceConfig resolves the connection and composes the SOQL.
func (e *pipelineExecutor) salesforceConfig(
	act pipeline.Activity,
	tp map[string]json.RawMessage,
	resolve func(json.RawMessage) (any, error),
) (*salesforceCfg, error) {
	var src map[string]json.RawMessage
	if err := json.Unmarshal(tp["source"], &src); err != nil {
		return nil, fmt.Errorf("copy %q: source is not an object", act.Name)
	}
	str := func(key string) (string, error) {
		raw, ok := src[key]
		if !ok || len(raw) == 0 {
			return "", nil
		}
		v, err := resolve(raw)
		if err != nil {
			return "", fmt.Errorf("copy %q: source %s: %w", act.Name, key, err)
		}
		if v == nil {
			return "", nil
		}
		return strings.TrimSpace(fmt.Sprint(v)), nil
	}

	// Reports are the Analytics REST API, not a Bulk query. Running a Bulk query
	// instead would return the OBJECT's rows rather than the report's — plausible
	// data, wrong data, and no error to notice.
	if r, err := str("reportId"); err != nil {
		return nil, err
	} else if r != "" {
		return nil, fmt.Errorf("copy %q: Salesforce reportId %q is not implemented — a report is "+
			"the Analytics REST API, not a Bulk API 2.0 query, and running a query instead would "+
			"return the object's rows rather than the report's (docs/41)", act.Name, r)
	}

	base, err := e.salesforceConnection(act, "source", src, resolve)
	if err != nil {
		return nil, err
	}
	cfg := base
	cfg.operation = "query"

	// includeDeletedObjects is not a filter applied to results — it selects a
	// DIFFERENT Bulk operation. Treating it as a post-filter would silently drop
	// the deleted rows the author asked for.
	if s, err := str("includeDeletedObjects"); err != nil {
		return nil, err
	} else if s != "" && !strings.EqualFold(s, "false") {
		cfg.operation = "queryAll"
	}

	object, err := str("objectApiName")
	if err != nil {
		return nil, err
	}
	soql, err := str("query")
	if err != nil {
		return nil, err
	}
	switch {
	case soql != "":
		cfg.soql = soql
	case object != "":
		// Fabric: with no SOQL, all the object's data is retrieved. FIELDS(ALL)
		// is how Bulk 2.0 says that, and it requires an explicit LIMIT.
		cfg.soql = fmt.Sprintf("SELECT FIELDS(ALL) FROM %s LIMIT %d", object, salesforceMaxRows)
	default:
		return nil, fmt.Errorf("copy %q: Salesforce needs an `objectApiName` or a SOQL `query`", act.Name)
	}
	return cfg, nil
}

func (e *pipelineExecutor) salesforceDo(
	act pipeline.Activity, cfg *salesforceCfg, method, url string, body string, accept string,
) (*http.Response, []byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()
	var rdr *strings.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	var req *http.Request
	var err error
	if rdr != nil {
		req, err = http.NewRequestWithContext(ctx, method, url, rdr)
	} else {
		req, err = http.NewRequestWithContext(ctx, method, url, nil)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("copy %q: %w", act.Name, err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", accept)

	resp, err := e.a.webClient().Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("copy %q: %s %s: %w", act.Name, method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, ok := httpx.ReadBounded(resp.Body, salesforceMaxPageBytes)
	if !ok {
		return nil, nil, fmt.Errorf("copy %q: a Salesforce response is unreadable or exceeds %d bytes "+
			"— the rows are held in memory before they are committed", act.Name, salesforceMaxPageBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, nil, fmt.Errorf("copy %q: %s %s returned %d: %s",
			act.Name, method, url, resp.StatusCode, snippet(raw))
	}
	return resp, raw, nil
}

func (e *pipelineExecutor) salesforceCreateJob(act pipeline.Activity, cfg *salesforceCfg) (string, error) {
	body, err := json.Marshal(map[string]any{
		"operation": cfg.operation, "query": cfg.soql,
		"contentType": "CSV", "lineEnding": "LF",
	})
	if err != nil {
		return "", err
	}
	_, raw, err := e.salesforceDo(act, cfg, http.MethodPost,
		cfg.instance+"/services/data/"+cfg.apiVersion+"/jobs/query", string(body), "application/json")
	if err != nil {
		return "", err
	}
	var job struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(raw, &job); err != nil {
		return "", fmt.Errorf("copy %q: the job-create response is not JSON: %v", act.Name, err)
	}
	if job.ID == "" {
		return "", fmt.Errorf("copy %q: Salesforce accepted the job but returned no id: %s",
			act.Name, snippet(raw))
	}
	return job.ID, nil
}

// salesforceAwaitJob polls to a terminal state.
func (e *pipelineExecutor) salesforceAwaitJob(act pipeline.Activity, cfg *salesforceCfg, jobID string) error {
	url := cfg.instance + "/services/data/" + cfg.apiVersion + "/jobs/query/" + jobID
	state := ""
	for i := 0; i < salesforceMaxPolls; i++ {
		_, raw, err := e.salesforceDo(act, cfg, http.MethodGet, url, "", "application/json")
		if err != nil {
			return err
		}
		var job struct {
			State   string `json:"state"`
			Message string `json:"errorMessage"`
		}
		if err := json.Unmarshal(raw, &job); err != nil {
			return fmt.Errorf("copy %q: the job-status response is not JSON: %v", act.Name, err)
		}
		state = job.State
		switch state {
		case "JobComplete":
			return nil
		case "Failed", "Aborted":
			// Naming the state AND the job id is what makes this findable in the
			// org's own job monitor, which is where the real answer lives.
			return fmt.Errorf("copy %q: Salesforce job %s ended %s: %s",
				act.Name, jobID, state, job.Message)
		}
		time.Sleep(salesforcePollWait)
	}
	return fmt.Errorf("copy %q: Salesforce job %s never reached a terminal state — last seen %q "+
		"after %d polls (states are %s)", act.Name, jobID, state, salesforceMaxPolls, salesforceJobStates)
}

// salesforceResults downloads every result page and parses the CSV into rows.
//
// Paging is by the `Sforce-Locator` response header, and it ends when that
// header is the LITERAL STRING "null" — not an absent header and not an empty
// one. Treating absent-or-empty as the only stop would loop forever against a
// real org; treating "null" as an opaque locator would fetch a page named null.
func (e *pipelineExecutor) salesforceResults(
	act pipeline.Activity, cfg *salesforceCfg, jobID string,
) (*warehouse.Table, int, error) {
	base := cfg.instance + "/services/data/" + cfg.apiVersion + "/jobs/query/" + jobID + "/results"
	var out *warehouse.Table
	locator, pages := "", 0

	for {
		pages++
		if pages > salesforceMaxPages {
			return nil, 0, fmt.Errorf("copy %q: Salesforce job %s served more than %d result pages — "+
				"refused rather than looped", act.Name, jobID, salesforceMaxPages)
		}
		u := base
		if locator != "" {
			u += "?locator=" + url.QueryEscape(locator)
		}
		resp, raw, err := e.salesforceDo(act, cfg, http.MethodGet, u, "", "text/csv")
		if err != nil {
			return nil, 0, err
		}

		page, err := parseTabular(raw, "csv")
		if err != nil {
			return nil, 0, fmt.Errorf("copy %q: parsing result page %d: %v", act.Name, pages, err)
		}
		if out == nil {
			out = page
		} else {
			// Every page repeats the header row; only the rows accumulate.
			out.Rows = append(out.Rows, page.Rows...)
		}
		if len(out.Rows) > salesforceMaxRows {
			return nil, 0, fmt.Errorf("copy %q: the query passed the %d-row ceiling after %d pages — "+
				"refused rather than truncated", act.Name, salesforceMaxRows, pages)
		}

		next := resp.Header.Get("Sforce-Locator")
		if next == "" || next == "null" {
			break
		}
		locator = next
	}
	if out == nil {
		out = &warehouse.Table{}
	}
	return out, pages, nil
}

// salesforceConnection reads the org coordinates a Copy side names directly.
//
// Fabric puts these on a CONNECTION, which this emulator does not model — the
// same wall RestSource hit, and the same shape the Script activity already uses
// for its target. Shared between the source and the sink so one side cannot
// drift into accepting something the other rejects.
func (e *pipelineExecutor) salesforceConnection(
	act pipeline.Activity,
	side string,
	m map[string]json.RawMessage,
	resolve func(json.RawMessage) (any, error),
) (*salesforceCfg, error) {
	str := func(key string) (string, error) {
		raw, ok := m[key]
		if !ok || len(raw) == 0 {
			return "", nil
		}
		v, err := resolve(raw)
		if err != nil {
			return "", fmt.Errorf("copy %q: %s %s: %w", act.Name, side, key, err)
		}
		if v == nil {
			return "", nil
		}
		return strings.TrimSpace(fmt.Sprint(v)), nil
	}

	cfg := &salesforceCfg{apiVersion: salesforceAPIVersion, timeout: restDefaultTimeout}
	var err error
	if cfg.instance, err = str("instanceUrl"); err != nil {
		return nil, err
	}
	if cfg.instance == "" {
		return nil, fmt.Errorf("copy %q: Salesforce needs an `instanceUrl` — the emulator models no "+
			"connections, so the %s names the org directly (docs/41)", act.Name, side)
	}
	if !strings.HasPrefix(cfg.instance, "http://") && !strings.HasPrefix(cfg.instance, "https://") {
		return nil, fmt.Errorf("copy %q: Salesforce instanceUrl %q is not http(s)", act.Name, cfg.instance)
	}
	cfg.instance = strings.TrimSuffix(cfg.instance, "/")

	if cfg.token, err = str("accessToken"); err != nil {
		return nil, err
	}
	if cfg.token == "" {
		return nil, fmt.Errorf("copy %q: Salesforce needs an `accessToken` — a Web activity can run "+
			"the OAuth call and pass it as an expression (docs/41)", act.Name)
	}

	if v, err := str("apiVersion"); err != nil {
		return nil, err
	} else if v != "" {
		if !strings.HasPrefix(v, "v") {
			v = "v" + v
		}
		cfg.apiVersion = v
	}
	return cfg, nil
}
