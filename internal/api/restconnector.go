package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/httpx"
	"github.com/calvinchengx/fabric-emulator/internal/pipeline"
	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/calvinchengx/fabric-emulator/internal/warehouse"
)

// The REST connector (source half — R1 of docs/40-rest-connector-plan.md).
//
// `RestSource` is a REAL Fabric copy-activity type: the generic connector
// Microsoft points you at for any RESTful store with no dedicated connector.
// It is how a real Fabric user reaches BMC Helix — which has no Fabric
// connector at all — and how they reach ServiceNow when they need OAuth, since
// Fabric's ServiceNow connector is Basic-auth only.
//
// Implementing THIS rather than a `BMCHelixSource` is the whole point: a
// fictional item type would run here and fail to parse in Fabric, which is the
// one direction this emulator must never diverge. See docs/40 for the argument.
//
// R1 is one request and the row-shaping. Pagination (R2) and RestSink (R3) are
// separate: a `RestSink` still fails loudly by name in copySideTypes, and
// `paginationRules` are rejected here rather than silently ignored — a copy
// that reads page one of fifty and reports success is precisely the fabricated
// result this connector exists to avoid.

const (
	// Fabric's documented default httpRequestTimeout is 00:01:40.
	restDefaultTimeout = 100 * time.Second
	// One response body ceiling. Same reasoning as the Web activity: the rows
	// are held in memory and committed to a Delta table, so this is not a
	// download mechanism.
	restMaxBody = 8 << 20 // 8 MiB
	// Row ceiling, refused rather than truncated.
	restMaxRows = 1_000_000
)

// copySideType reads a Copy side's own `type` discriminator, expression-resolved.
// Deliberately the side's own `type` and never an inner one — the same rule
// resolveLoc applies, since `datasetSettings.type` and `location.type` are
// different vocabularies.
func copySideType(raw json.RawMessage, resolve func(json.RawMessage) (any, error)) (string, error) {
	var obj map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &obj) != nil {
		return "", nil
	}
	t, ok := obj["type"]
	if !ok || len(t) == 0 {
		return "", nil
	}
	v, err := resolve(t)
	if err != nil {
		return "", err
	}
	if v == nil {
		return "", nil
	}
	return strings.TrimSpace(fmt.Sprint(v)), nil
}

// restToLakehouse reads the REST source and commits the rows to the sink's
// Delta table. R1's sink is a lakehouse `Tables/<name>` — the shape that makes
// the rows queryable, and the one an ITSM ingest actually targets. A Files/
// sink would mean choosing a file format the source never described.
func (e *pipelineExecutor) restToLakehouse(
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
		return nil, fmt.Errorf("copy %q: a RestSource copy must land in a lakehouse table "+
			"(sink path `Tables/<name>`), got %q", act.Name, dst.path)
	}

	tbl, url, err := e.restSourceTable(act, tp, resolve)
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

	// SourceKindConnection is what marks a source as OUTSIDE Fabric, so the
	// portal draws it as an external node rather than hunting for an item. The
	// source item id stays empty because the emulator models no connections yet
	// (docs/40); the URL in SourcePath is the identity that matters here.
	edge := &store.LineageEdge{WorkspaceID: e.wid, JobID: e.jobID, ActivityName: act.Name,
		Producer:          store.ProducerCopy,
		SourceKind:        store.SourceKindConnection,
		SourcePath:        url,
		TargetWorkspaceID: dst.wsID, TargetItemID: dst.itemID, TargetPath: dst.path}
	if err := e.a.Store.CreateLineageEdge(edge); err != nil {
		return nil, fmt.Errorf("copy %q lineage: %v", act.Name, err)
	}

	out := map[string]any{
		"rowsRead": len(tbl.Rows), "rowsCopied": len(tbl.Rows),
		"dataRead": restBytes(tbl), "copyDuration": 0,
		"writeBehavior": mode, "source": url, "lineage": edge,
	}
	// Named, never silent: a nested field has no column shape, and "some columns
	// might not be available" is a miserable thing to debug without a name.
	if len(tbl.Skipped) > 0 {
		out["skippedColumns"] = tbl.Skipped
	}
	return out, nil
}

// restSourceTable performs the request and shapes the JSON response into rows.
// It returns the table and the resolved URL, which becomes the lineage edge's
// source path — a REST read has no OneLake source to point at.
func (e *pipelineExecutor) restSourceTable(
	act pipeline.Activity,
	tp map[string]json.RawMessage,
	resolve func(json.RawMessage) (any, error),
) (*warehouse.Table, string, error) {
	var src map[string]json.RawMessage
	if err := json.Unmarshal(tp["source"], &src); err != nil {
		return nil, "", fmt.Errorf("copy %q: source is not an object", act.Name)
	}

	rules, err := restPaginationRulesOf(act, src, resolve)
	if err != nil {
		return nil, "", err
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

	url, err := e.restURL(act, "RestSource", src, resolve)
	if err != nil {
		return nil, "", err
	}

	method := http.MethodGet
	if m, err := str("requestMethod"); err != nil {
		return nil, "", err
	} else if m != "" {
		method = strings.ToUpper(m)
	}
	// Fabric allows GET and POST here and nothing else. Naming the value beats
	// letting an unexpected verb reach the server.
	if method != http.MethodGet && method != http.MethodPost {
		return nil, "", fmt.Errorf("copy %q: RestSource requestMethod %q is not allowed "+
			"(Fabric permits GET and POST)", act.Name, method)
	}

	reqBody, err := str("requestBody")
	if err != nil {
		return nil, "", err
	}

	timeout := restDefaultTimeout
	if s, err := str("httpRequestTimeout"); err != nil {
		return nil, "", err
	} else if s != "" {
		d, ok := pipeline.ParseTimeout(s)
		if !ok || d <= 0 {
			return nil, "", fmt.Errorf("copy %q: RestSource httpRequestTimeout %q is not a "+
				"TimeSpan (expected hh:mm:ss)", act.Name, s)
		}
		timeout = d
	}

	// The header set is built once and carried across pages: `Headers.{x}` rules
	// step a placeholder inside it, and `Headers.name` rules overwrite one from
	// the previous response.
	tmpl, _ := http.NewRequest(method, url, nil)
	if err := restHeaders(act, "RestSource", src, resolve, tmpl); err != nil {
		return nil, "", err
	}
	hdr := tmpl.Header.Clone()
	if reqBody != "" {
		hdr.Set("Content-Type", "application/json")
	}
	// Set AFTER additionalHeaders so it wins: Fabric documents that the REST
	// connector ignores any Accept the author supplied, because it only handles
	// JSON. Honouring a user's `Accept: text/csv` here would get a body this
	// cannot parse and blame the wrong thing.
	hdr.Set("Accept", "application/json")

	var records []any
	pageURL, pages := url, 0
	for {
		pages++
		if pages > restMaxPages {
			return nil, "", fmt.Errorf("copy %q: stopped after %d pages — a `next` that never "+
				"terminates is a documented real case, so this is a ceiling rather than a loop; "+
				"set MaxRequestNumber or an EndCondition if the source really is this long",
				act.Name, restMaxPages)
		}
		if rules.maxRequests > 0 && pages > rules.maxRequests {
			break
		}

		pageHdr := hdr.Clone()
		reqURL := rules.apply(pageURL, pageHdr)

		doc, resp, err := e.restFetch(act, method, reqURL, reqBody, pageHdr, timeout)
		if err != nil {
			return nil, "", err
		}
		// 204 No Content ends the loop, as Fabric documents. It is also the one
		// status with no body to parse.
		if resp.StatusCode == http.StatusNoContent {
			break
		}

		page, err := restRecordsOf(act, tp, doc, rules.declared)
		if err != nil {
			return nil, "", err
		}
		records = append(records, page...)
		if len(records) > restMaxRows {
			return nil, "", fmt.Errorf("copy %q: passed the %d-row ceiling after %d pages — "+
				"refused rather than truncated", act.Name, restMaxRows, pages)
		}

		if rules.done(doc, resp.Header) {
			break
		}
		if !rules.advance() {
			break
		}
		next, ok := rules.nextRequest(pageURL, hdr, doc, resp)
		if !ok {
			break
		}
		pageURL = next
	}

	tbl, err := restTable(act, records, translatorMappings(tp))
	if err != nil {
		return nil, "", err
	}
	return tbl, url, nil
}

// restFetch performs one page request and returns the decoded body.
func (e *pipelineExecutor) restFetch(
	act pipeline.Activity, method, reqURL, reqBody string, hdr http.Header, timeout time.Duration,
) (any, *http.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var body io.Reader
	if reqBody != "" {
		// Rebuilt per page: a Reader is consumed by the first request, and a POST
		// whose second page silently sent an empty body would be a very quiet bug.
		body = strings.NewReader(reqBody)
	}
	req, err := http.NewRequestWithContext(ctx, method, reqURL, body)
	if err != nil {
		return nil, nil, fmt.Errorf("copy %q: %w", act.Name, err)
	}
	req.Header = hdr

	resp, err := e.a.webClient().Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("copy %q: %s %s: %w", act.Name, method, reqURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, ok := httpx.ReadBounded(resp.Body, restMaxBody)
	if !ok {
		return nil, nil, fmt.Errorf("copy %q: response body is unreadable or exceeds %d bytes — "+
			"the rows are held in memory before they are committed, so this is not a "+
			"download mechanism", act.Name, restMaxBody)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, nil, fmt.Errorf("copy %q: %s %s returned %d: %s",
			act.Name, method, reqURL, resp.StatusCode, snippet(raw))
	}
	if resp.StatusCode == http.StatusNoContent {
		return nil, resp, nil
	}

	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, nil, fmt.Errorf("copy %q: response is not JSON: %v", act.Name, err)
	}
	return doc, resp, nil
}

// restURL assembles the request URL.
//
// Fabric splits this across a linked service (`RestService.url`, the base) and a
// dataset (`RestResource.relativeUrl`). The emulator models no connections, so
// R1 reads the nested shape when a pipeline authored in Fabric carries it, and
// also accepts a plain `url` on the source — the same three-shapes tolerance
// resolveLoc already applies to OneLake locations.
func (e *pipelineExecutor) restURL(
	act pipeline.Activity,
	kind string,
	src map[string]json.RawMessage,
	resolve func(json.RawMessage) (any, error),
) (string, error) {
	descend := func(m map[string]json.RawMessage, keys ...string) map[string]json.RawMessage {
		cur := m
		for _, k := range keys {
			v, ok := cur[k]
			if !ok {
				return nil
			}
			next := map[string]json.RawMessage{}
			if json.Unmarshal(v, &next) != nil {
				return nil
			}
			cur = next
		}
		return cur
	}
	read := func(m map[string]json.RawMessage, key string) (string, error) {
		if m == nil {
			return "", nil
		}
		raw, ok := m[key]
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

	base, err := read(src, "url")
	if err != nil {
		return "", err
	}
	ds := descend(src, "datasetSettings", "typeProperties")
	ls := descend(src, "datasetSettings", "linkedService", "properties", "typeProperties")
	if base == "" {
		if base, err = read(ls, "url"); err != nil {
			return "", err
		}
	}
	rel, err := read(ds, "relativeUrl")
	if err != nil {
		return "", err
	}

	url := base
	if rel != "" {
		url = strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(rel, "/")
	}
	if url == "" {
		return "", fmt.Errorf("copy %q: %s needs a url — set `url` on the side, or "+
			"the linked service's `url` with the dataset's `relativeUrl`", act.Name, kind)
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return "", fmt.Errorf("copy %q: %s url %q is not http(s)", act.Name, kind, url)
	}
	return url, nil
}

// restHeaders applies additionalHeaders, each value expression-resolved.
//
// This is R1's whole authentication story, and deliberately so: Fabric puts auth
// on the linked service, which the emulator does not model. It is also the case
// that MATTERS — BMC Helix authenticates with `Authorization: AR-JWT <token>`, a
// proprietary scheme that is not one of Fabric's built-in types anyway, so a
// real Fabric user fetches the token with a Web activity and passes it here as
// an expression. That pipeline works today.
func restHeaders(
	act pipeline.Activity,
	kind string,
	src map[string]json.RawMessage,
	resolve func(json.RawMessage) (any, error),
	req *http.Request,
) error {
	// An authenticationType the emulator cannot honour must fail by name. Falling
	// through to an anonymous request would 401 at the endpoint and get reported
	// as a connector bug.
	if ls, ok := src["datasetSettings"]; ok && len(ls) > 0 {
		var probe struct {
			LinkedService struct {
				Properties struct {
					TypeProperties struct {
						AuthenticationType string `json:"authenticationType"`
					} `json:"typeProperties"`
				} `json:"properties"`
			} `json:"linkedService"`
		}
		if json.Unmarshal(ls, &probe) == nil {
			at := probe.LinkedService.Properties.TypeProperties.AuthenticationType
			if at != "" && !strings.EqualFold(at, "Anonymous") {
				return fmt.Errorf("copy %q: %s authenticationType %q is not implemented — "+
					"the emulator models no connections, so R1 supports Anonymous plus "+
					"`additionalHeaders` (a Web activity can fetch a token and pass it as an "+
					"expression)", act.Name, kind, at)
			}
		}
	}

	raw, ok := src["additionalHeaders"]
	if !ok || len(raw) == 0 {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return fmt.Errorf("copy %q: %s additionalHeaders is not an object", act.Name, kind)
	}
	for name, vraw := range fields {
		v, err := resolve(vraw)
		if err != nil {
			return fmt.Errorf("copy %q: header %q: %w", act.Name, name, err)
		}
		req.Header.Set(name, fmt.Sprint(v))
	}
	return nil
}

// restPaginationRulesOf reads and parses the source's `paginationRules`. Values
// are expression-resolved, so a page size can come from a pipeline parameter.
func restPaginationRulesOf(
	act pipeline.Activity,
	src map[string]json.RawMessage,
	resolve func(json.RawMessage) (any, error),
) (*restPagination, error) {
	raw, ok := src["paginationRules"]
	if !ok || len(raw) == 0 {
		return parsePaginationRules(act.Name, nil)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("copy %q: RestSource paginationRules is not an object", act.Name)
	}
	rules := make(map[string]string, len(fields))
	for k, vraw := range fields {
		v, err := resolve(vraw)
		if err != nil {
			return nil, fmt.Errorf("copy %q: pagination rule %q: %w", act.Name, k, err)
		}
		rules[k] = strings.TrimSpace(fmt.Sprint(v))
	}
	return parsePaginationRules(act.Name, rules)
}

// restRecordsOf finds the array of records in one decoded page.
//
// Fabric selects it with the copy activity's `translator.collectionReference`
// (a JSONPath). When one is given it decides; when none is, a response whose
// object holds exactly one array is unambiguous and is used. Anything else
// fails and SAYS what it found — guessing between two arrays is how a copy
// silently ingests the wrong one.
func restRecordsOf(act pipeline.Activity, tp map[string]json.RawMessage, doc any, paginated bool) ([]any, error) {
	// Fabric states this outright: pagination is not supported when the response's
	// top-level structure is a JSON array. There is nowhere for a cursor to live,
	// so the rules could never fire and the copy would read exactly one page while
	// looking like it paged.
	if _, isArr := doc.([]any); isArr && paginated {
		return nil, fmt.Errorf("copy %q: paginationRules are set, but the response's top-level "+
			"structure is a JSON array — Fabric does not paginate that shape, and the rules "+
			"could never fire", act.Name)
	}

	ref := collectionReference(tp)
	if ref != "" {
		node, ok := jsonPathLookup(doc, ref)
		if !ok {
			return nil, fmt.Errorf("copy %q: collectionReference %q matches nothing in the response",
				act.Name, ref)
		}
		arr, ok := node.([]any)
		if !ok {
			return nil, fmt.Errorf("copy %q: collectionReference %q is a %T, not an array",
				act.Name, ref, node)
		}
		return arr, nil
	}

	// A bare top-level array is unambiguous.
	if arr, ok := doc.([]any); ok {
		return arr, nil
	}
	obj, ok := doc.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("copy %q: response is a %T; RestSource expects a JSON object or array",
			act.Name, doc)
	}
	var found []string
	for k, v := range obj {
		if _, isArr := v.([]any); isArr {
			found = append(found, k)
		}
	}
	switch len(found) {
	case 1:
		return obj[found[0]].([]any), nil
	case 0:
		return nil, fmt.Errorf("copy %q: the response object holds no array of records; "+
			"set the copy activity's translator.collectionReference", act.Name)
	default:
		sortStrings(found)
		return nil, fmt.Errorf("copy %q: the response object holds %d arrays (%s) and nothing says "+
			"which holds the records; set the copy activity's translator.collectionReference",
			act.Name, len(found), strings.Join(found, ", "))
	}
}

// collectionReference reads translator.collectionReference off the copy activity.
// restMapping is one entry of the copy activity's `translator.mappings`: a
// JSONPath into each RECORD, and the column it becomes.
type restMapping struct {
	path, name string
}

// translatorMappings reads `translator.mappings`.
//
// Auto-flatten covers a response whose records are flat objects. Plenty are not:
// BMC Helix's AR System returns `{"entries":[{"values":{…},"_links":{…}}]}`, so
// every field is one level down and auto-flatten finds NO scalar columns at all.
// Mappings are Fabric's own answer to that, and without them a Helix example
// could only be written by faking the response shape.
func translatorMappings(tp map[string]json.RawMessage) []restMapping {
	raw, ok := tp["translator"]
	if !ok || len(raw) == 0 {
		return nil
	}
	var t struct {
		Mappings []struct {
			Source struct {
				Path string `json:"path"`
				Name string `json:"name"`
			} `json:"source"`
			Sink struct {
				Name string `json:"name"`
			} `json:"sink"`
		} `json:"mappings"`
	}
	if json.Unmarshal(raw, &t) != nil {
		return nil
	}
	var out []restMapping
	for _, m := range t.Mappings {
		path := strings.TrimSpace(m.Source.Path)
		if path == "" && m.Source.Name != "" {
			// A bare `source.name` is the flat-record form; treat it as a
			// one-segment path so both shapes go through the same lookup.
			path = "$['" + m.Source.Name + "']"
		}
		if path == "" {
			continue
		}
		name := strings.TrimSpace(m.Sink.Name)
		if name == "" {
			name = lastPathSegment(path)
		}
		out = append(out, restMapping{path: path, name: name})
	}
	return out
}

// lastPathSegment names a column after the leaf of its source path, for a
// mapping that gives no sink name.
func lastPathSegment(path string) string {
	p := strings.TrimSuffix(strings.TrimSpace(path), "]")
	for _, cut := range []string{"['", "[\"", "."} {
		if i := strings.LastIndex(p, cut); i >= 0 {
			return strings.Trim(p[i+len(cut):], "'\"")
		}
	}
	return strings.Trim(p, "$'\"")
}

func collectionReference(tp map[string]json.RawMessage) string {
	raw, ok := tp["translator"]
	if !ok || len(raw) == 0 {
		return ""
	}
	var t struct {
		CollectionReference string `json:"collectionReference"`
	}
	if json.Unmarshal(raw, &t) != nil {
		return ""
	}
	return strings.TrimSpace(t.CollectionReference)
}

// jsonPathLookup walks the small JSONPath subset Fabric's collectionReference
// uses: `$.a.b`, `$['a']['b']`, and the two mixed. Not a general JSONPath — no
// filters, wildcards or slices — and an unsupported expression simply fails to
// match, which restRecords reports by name.
func jsonPathLookup(doc any, path string) (any, bool) {
	p := strings.TrimSpace(path)
	p = strings.TrimPrefix(p, "$")
	cur := doc
	for len(p) > 0 {
		var key string
		switch {
		case strings.HasPrefix(p, "['"), strings.HasPrefix(p, `["`):
			q := p[1:2] // ' or "
			end := strings.Index(p[2:], q+"]")
			if end < 0 {
				return nil, false
			}
			key, p = p[2:2+end], p[2+end+2:]
		case strings.HasPrefix(p, "."):
			rest := p[1:]
			cut := strings.IndexAny(rest, ".[")
			if cut < 0 {
				key, p = rest, ""
			} else {
				key, p = rest[:cut], rest[cut:]
			}
		default:
			return nil, false
		}
		if key == "" {
			return nil, false
		}
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = obj[key]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// restTable flattens the records into columns.
//
// Column order is first-seen across the records, so it is deterministic for a
// given response rather than map-iteration order. Values keep the type JSON
// gave them — unlike a CSV, a JSON document DESCRIBES its types, and discarding
// that would be the guess. A nested object or array has no column shape, so it
// is recorded in Skipped by name rather than silently dropped or stringified.
func restTable(act pipeline.Activity, records []any, mappings []restMapping) (*warehouse.Table, error) {
	if len(records) > restMaxRows {
		return nil, fmt.Errorf("copy %q: the response holds %d records, above the %d-row ceiling — "+
			"refused rather than truncated", act.Name, len(records), restMaxRows)
	}

	if len(mappings) > 0 {
		return restMappedTable(act, records, mappings)
	}

	tbl := &warehouse.Table{}
	index := map[string]int{}
	skipped := map[string]bool{}
	rows := make([]map[string]any, 0, len(records))

	for i, rec := range records {
		obj, ok := rec.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("copy %q: record %d is a %T, not an object — RestSource "+
				"expects an array of objects", act.Name, i, rec)
		}
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sortStrings(keys) // stable within a record; first-seen order across records
		for _, k := range keys {
			switch obj[k].(type) {
			case map[string]any, []any:
				skipped[k] = true
				continue
			}
			if _, seen := index[k]; !seen {
				index[k] = len(tbl.Columns)
				tbl.Columns = append(tbl.Columns, k)
			}
		}
		rows = append(rows, obj)
	}

	if len(tbl.Columns) == 0 && len(records) > 0 {
		return nil, fmt.Errorf("copy %q: the records carry no scalar fields to make columns from",
			act.Name)
	}
	for _, obj := range rows {
		row := make([]any, len(tbl.Columns))
		for k, i := range index {
			if v, ok := obj[k]; ok {
				switch v.(type) {
				case map[string]any, []any:
				default:
					row[i] = v
				}
			}
		}
		tbl.Rows = append(tbl.Rows, row)
	}
	for k := range skipped {
		if _, isColumn := index[k]; !isColumn {
			tbl.Skipped = append(tbl.Skipped, k)
		}
	}
	sortStrings(tbl.Skipped)
	return tbl, nil
}

// sortStrings is an insertion sort — these slices are column counts, and this
// avoids importing sort for one call in a file that is otherwise stdlib-light.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// restBytes reports the size a table stands for, for the copy's output counters.
func restBytes(tbl *warehouse.Table) int {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	for _, row := range tbl.Rows {
		_ = enc.Encode(row)
	}
	return b.Len()
}

// restMappedTable shapes records through explicit `translator.mappings`.
//
// Column ORDER is the mapping order, not first-seen: the author wrote the list,
// so the list is the schema. A path that matches nothing leaves the cell empty
// rather than failing the copy — an optional field absent on some records is
// ordinary, and failing the whole ingest over one would be worse than a gap.
// A path that matches nothing on EVERY record is a different thing, and is
// reported, because that is a mapping that does not describe this response.
func restMappedTable(act pipeline.Activity, records []any, mappings []restMapping) (*warehouse.Table, error) {
	tbl := &warehouse.Table{Columns: make([]string, 0, len(mappings))}
	hits := make([]int, len(mappings))
	for _, m := range mappings {
		tbl.Columns = append(tbl.Columns, m.name)
	}

	for i, rec := range records {
		row := make([]any, len(mappings))
		for j, m := range mappings {
			v, ok := jsonPathLookup(rec, m.path)
			if !ok || v == nil {
				continue
			}
			switch v.(type) {
			case map[string]any, []any:
				// A mapping that lands on a container has selected a subtree, not a
				// value. Naming the record index makes it findable.
				return nil, fmt.Errorf("copy %q: mapping %q selects a %T in record %d, not a value",
					act.Name, m.path, v, i)
			}
			row[j] = v
			hits[j]++
		}
		tbl.Rows = append(tbl.Rows, row)
	}

	if len(records) > 0 {
		var dead []string
		for j, m := range mappings {
			if hits[j] == 0 {
				dead = append(dead, m.path)
			}
		}
		if len(dead) > 0 {
			return nil, fmt.Errorf("copy %q: %d mapping(s) matched nothing in any of the %d records "+
				"(%s) — the mapping does not describe this response",
				act.Name, len(dead), len(records), strings.Join(dead, ", "))
		}
	}
	return tbl, nil
}
