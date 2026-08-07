package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/httpx"
	"github.com/calvinchengx/fabric-emulator/internal/pipeline"
	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/calvinchengx/fabric-emulator/internal/warehouse"
)

// The REST connector's sink half (R3 of docs/40-rest-connector-plan.md).
//
// `RestSink` is the other direction: rows out of a lakehouse and into a REST
// API. Fabric's wire contract is short — batches of `writeBatchSize` records,
// each batch one request carrying a JSON ARRAY of row objects:
//
//	[ { <row> }, { <row> }, … ]
//
// That array shape is the whole payload spec, which makes the batching itself
// the thing worth testing: a sink that sent every row in one request, or sent
// the last batch twice, still writes a plausible-looking total.

const (
	// Fabric's documented default.
	restSinkBatch = 10000
	// Fabric documents requestInterval as milliseconds in [10, 60000].
	restMinInterval = 10 * time.Millisecond
	restMaxInterval = 60000 * time.Millisecond
)

// restFromLakehouse reads the Copy's source rows and POSTs them in batches.
func (e *pipelineExecutor) restFromLakehouse(
	act pipeline.Activity,
	tp map[string]json.RawMessage,
	resolve func(json.RawMessage) (any, error),
) (map[string]any, error) {
	src, err := e.resolveLoc("source", tp["source"], resolve)
	if err != nil {
		return nil, fmt.Errorf("copy %q source: %w", act.Name, err)
	}
	// A BinarySource is the author stating the source is opaque bytes. Nothing
	// below could turn that into JSON rows honestly — and it must be refused
	// EXPLICITLY, because lookupFormat's fallback is "csv", so a binary file would
	// otherwise be parsed as text and posted as garbage rows with a Succeeded
	// beside it. The guess is harmless for a OneLake byte copy, which moves the
	// bytes either way; here it would export nonsense to someone's API.
	if t, err := copySideType(tp["source"], resolve); err != nil {
		return nil, fmt.Errorf("copy %q source: %w", act.Name, err)
	} else if t == "BinarySource" {
		return nil, fmt.Errorf("copy %q: a RestSink cannot take a BinarySource — the payload is a "+
			"JSON array of row objects, and opaque bytes have no rows; read the source as "+
			"DelimitedText, Parquet or a Delta table instead", act.Name)
	}

	tbl, readable, err := e.readTabularSource(act, tp, src)
	if err != nil {
		return nil, err
	}
	if !readable {
		// No byte-copy fallback exists in this direction: there is no way to POST
		// an opaque directory as JSON rows, so this fails rather than pretending.
		return nil, fmt.Errorf("copy %q: a RestSink source must be rows the emulator can read — "+
			"a Delta table (`Tables/<name>`), or a standalone Parquet or CSV file; %q is neither",
			act.Name, src.path)
	}

	var sink map[string]json.RawMessage
	if err := json.Unmarshal(tp["sink"], &sink); err != nil {
		return nil, fmt.Errorf("copy %q: sink is not an object", act.Name)
	}
	cfg, err := e.restSinkConfig(act, sink, resolve)
	if err != nil {
		return nil, err
	}

	rows := restRowObjects(tbl)
	batches, sent := 0, 0
	for start := 0; start < len(rows); start += cfg.batchSize {
		end := start + cfg.batchSize
		if end > len(rows) {
			end = len(rows)
		}
		if err := e.restSinkSend(act, cfg, rows[start:end]); err != nil {
			return nil, err
		}
		batches++
		sent += end - start
	}
	// Zero rows means zero requests. Sending an empty array would be a write the
	// author never asked for, and some APIs treat `[]` as "replace with nothing".
	if len(rows) == 0 {
		batches = 0
	}

	// The target is outside Fabric. SourceKind has a `connection` value for this
	// on the source side; the edge model has no TargetKind counterpart yet, so the
	// URL goes in TargetPath and the target item id stays empty. The portal will
	// render that node without a label until the model gains one — noted rather
	// than worked around, because inventing an item id here would be worse.
	edge := &store.LineageEdge{WorkspaceID: e.wid, JobID: e.jobID, ActivityName: act.Name,
		Producer:          store.ProducerCopy,
		SourceWorkspaceID: src.wsID, SourceItemID: src.itemID, SourcePath: src.path,
		TargetPath: cfg.url}
	if err := e.a.Store.CreateLineageEdge(edge); err != nil {
		return nil, fmt.Errorf("copy %q lineage: %v", act.Name, err)
	}

	out := map[string]any{
		"rowsRead": len(rows), "rowsCopied": sent,
		"batchesWritten": batches, "writeBatchSize": cfg.batchSize,
		"copyDuration": 0, "sink": cfg.url, "lineage": edge,
	}
	if cfg.intervalTotal > 0 {
		// Virtual, like the interpreter's retry backoff: reported, never slept.
		// Real waiting between batches would cost the suite minutes and prove
		// nothing the count does not already.
		out["requestIntervalSeconds"] = cfg.intervalTotal.Seconds() * float64(max(batches-1, 0))
	}
	return out, nil
}

// restSinkCfg is the resolved sink configuration.
type restSinkCfg struct {
	url           string
	method        string
	header        http.Header
	batchSize     int
	timeout       time.Duration
	gzip          bool
	intervalTotal time.Duration
}

func (e *pipelineExecutor) restSinkConfig(
	act pipeline.Activity,
	sink map[string]json.RawMessage,
	resolve func(json.RawMessage) (any, error),
) (*restSinkCfg, error) {
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

	url, err := e.restURL(act, "RestSink", sink, resolve)
	if err != nil {
		return nil, err
	}

	cfg := &restSinkCfg{url: url, method: http.MethodPost, batchSize: restSinkBatch,
		timeout: restDefaultTimeout}

	if m, err := str("requestMethod"); err != nil {
		return nil, err
	} else if m != "" {
		cfg.method = strings.ToUpper(m)
	}
	// Fabric permits POST, PUT and PATCH on a sink and nothing else. A DELETE
	// reaching a customer's endpoint because the emulator was permissive is not a
	// failure mode worth risking.
	switch cfg.method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
	default:
		return nil, fmt.Errorf("copy %q: RestSink requestMethod %q is not allowed "+
			"(Fabric permits POST, PUT and PATCH)", act.Name, cfg.method)
	}

	if s, err := str("writeBatchSize"); err != nil {
		return nil, err
	} else if s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("copy %q: RestSink writeBatchSize %q is not a positive number",
				act.Name, s)
		}
		cfg.batchSize = n
	}

	if s, err := str("httpRequestTimeout"); err != nil {
		return nil, err
	} else if s != "" {
		d, ok := pipeline.ParseTimeout(s)
		if !ok || d <= 0 {
			return nil, fmt.Errorf("copy %q: RestSink httpRequestTimeout %q is not a TimeSpan "+
				"(expected hh:mm:ss)", act.Name, s)
		}
		cfg.timeout = d
	}

	if s, err := str("httpCompressionType"); err != nil {
		return nil, err
	} else if s != "" && !strings.EqualFold(s, "none") {
		if !strings.EqualFold(s, "gzip") {
			return nil, fmt.Errorf("copy %q: RestSink httpCompressionType %q is not supported "+
				"(Fabric allows none and gzip)", act.Name, s)
		}
		cfg.gzip = true
	}

	if s, err := str("requestInterval"); err != nil {
		return nil, err
	} else if s != "" {
		ms, err := strconv.Atoi(s)
		if err != nil {
			return nil, fmt.Errorf("copy %q: RestSink requestInterval %q is not a number of "+
				"milliseconds", act.Name, s)
		}
		d := time.Duration(ms) * time.Millisecond
		// Fabric documents the valid band. Silently clamping would make a
		// mistyped 6_000_000 look accepted.
		if d < restMinInterval || d > restMaxInterval {
			return nil, fmt.Errorf("copy %q: RestSink requestInterval %dms is outside Fabric's "+
				"[%d, %d] range", act.Name, ms, restMinInterval/time.Millisecond,
				restMaxInterval/time.Millisecond)
		}
		cfg.intervalTotal = d
	}

	tmpl, _ := http.NewRequest(cfg.method, url, nil)
	if err := restHeaders(act, "RestSink", sink, resolve, tmpl); err != nil {
		return nil, err
	}
	cfg.header = tmpl.Header.Clone()
	cfg.header.Set("Content-Type", "application/json")
	if cfg.gzip {
		cfg.header.Set("Content-Encoding", "gzip")
	}
	return cfg, nil
}

// restSinkSend writes one batch.
func (e *pipelineExecutor) restSinkSend(act pipeline.Activity, cfg *restSinkCfg, batch []map[string]any) error {
	payload, err := json.Marshal(batch)
	if err != nil {
		return fmt.Errorf("copy %q: encoding a batch: %v", act.Name, err)
	}
	body := payload
	if cfg.gzip {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		if _, err := zw.Write(payload); err != nil {
			return fmt.Errorf("copy %q: gzip: %v", act.Name, err)
		}
		if err := zw.Close(); err != nil {
			return fmt.Errorf("copy %q: gzip: %v", act.Name, err)
		}
		body = buf.Bytes()
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, cfg.method, cfg.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("copy %q: %w", act.Name, err)
	}
	req.Header = cfg.header.Clone()

	resp, err := e.a.webClient().Do(req)
	if err != nil {
		return fmt.Errorf("copy %q: %s %s: %w", act.Name, cfg.method, cfg.url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, ok := httpx.ReadBounded(resp.Body, restMaxBody)
	if !ok {
		return fmt.Errorf("copy %q: the endpoint's response is unreadable or exceeds %d bytes",
			act.Name, restMaxBody)
	}
	// A non-2xx fails the activity, as it does on the source side. A partially
	// written export reported as Succeeded is the worst outcome available here.
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("copy %q: %s %s returned %d: %s",
			act.Name, cfg.method, cfg.url, resp.StatusCode, snippet(raw))
	}
	return nil
}

// restRowObjects turns the table's positional rows into the objects Fabric
// sends. A column whose value is absent is omitted rather than sent as null:
// null means "set this to nothing" to a good many APIs, which is a different
// instruction from "I have nothing to say about this field".
func restRowObjects(tbl *warehouse.Table) []map[string]any {
	out := make([]map[string]any, 0, len(tbl.Rows))
	for _, row := range tbl.Rows {
		obj := make(map[string]any, len(tbl.Columns))
		for i, col := range tbl.Columns {
			if i < len(row) && row[i] != nil {
				obj[col] = row[i]
			}
		}
		out = append(out, obj)
	}
	return out
}
