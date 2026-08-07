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
)

// The Web activity: a real HTTP call, because a fake one is a false pass.
//
// This used to record `Succeeded` without calling anything, on the reasoning
// that arbitrary network calls break the emulator's offline guarantee. That
// reasoning protected the wrong thing. A pipeline whose next activity branches
// on `@activity('Ping').output.status` got a success and an empty body from a
// URL that was never contacted — green locally, different in Fabric, which is
// the exact failure class this emulator exists to prevent. The offline promise
// is that the EMULATOR makes no calls of its own; it was never a promise that a
// user's pipeline cannot reach the service it names.
//
// Hermetic runs are still available, explicitly: FABRIC_WEB_ACTIVITY=stub
// restores the old behaviour for a CI leg that must not touch the network. It
// is opt-in because the silent version is the dangerous one.

// webResult is Fabric's Web activity output shape. A JSON response body is
// merged into the output at the top level — which is what makes
// `@activity('X').output.someField` work in a downstream expression, and why
// the body cannot simply be nested under a "body" key.
const (
	// Fabric's own header key in the activity output.
	webHeadersKey = "ADFWebActivityResponseHeaders"
	// Fabric defaults an activity's HTTP timeout to 1 minute; policy.timeout
	// overrides it. A request with no ceiling can hang a pipeline forever,
	// which on a test harness looks like the emulator wedged.
	webDefaultTimeout = time.Minute
	// A response body larger than this is refused rather than buffered: the
	// output is held in memory, embedded in the run record, and rendered in the
	// portal. An accidental `GET /large.parquet` should fail cleanly.
	webMaxBody = 8 << 20 // 8 MiB
)

// webActivity performs the call and shapes the result the way Fabric does.
//
// `resolve` is the pipeline's expression evaluator, so `url`, `headers` and
// `body` may all be expressions — which is the normal case: a URL built from
// `@pipeline().parameters.endpoint` is the reason this activity is used at all.
func (e *pipelineExecutor) webActivity(
	act pipeline.Activity,
	tp map[string]json.RawMessage,
	resolve func(json.RawMessage) (any, error),
) (map[string]any, error) {
	method := "GET"
	if raw, ok := tp["method"]; ok && len(raw) > 0 {
		v, err := resolve(raw)
		if err != nil {
			return nil, fmt.Errorf("web activity %q: method: %w", act.Name, err)
		}
		if s := strings.TrimSpace(fmt.Sprint(v)); s != "" {
			method = strings.ToUpper(s)
		}
	}

	rawURL := ""
	if raw, ok := tp["url"]; ok && len(raw) > 0 {
		v, err := resolve(raw)
		if err != nil {
			return nil, fmt.Errorf("web activity %q: url: %w", act.Name, err)
		}
		rawURL = strings.TrimSpace(fmt.Sprint(v))
	}
	if rawURL == "" {
		return nil, fmt.Errorf("web activity %q: url is required", act.Name)
	}

	var bodyVal any
	if raw, ok := tp["body"]; ok && len(raw) > 0 {
		v, err := resolve(raw)
		if err != nil {
			return nil, fmt.Errorf("web activity %q: body: %w", act.Name, err)
		}
		bodyVal = v
	}

	headers := map[string]string{}
	if raw, ok := tp["headers"]; ok && len(raw) > 0 {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, fmt.Errorf("web activity %q: headers are not an object", act.Name)
		}
		for name, vraw := range fields {
			v, err := resolve(vraw)
			if err != nil {
				return nil, fmt.Errorf("web activity %q: header %q: %w", act.Name, name, err)
			}
			headers[name] = fmt.Sprint(v)
		}
	}
	return e.httpActivity("web", act, method, rawURL, headers, bodyVal)
}

// httpActivity is the shared post-resolution core of the Web and Azure
// Function activities: one real HTTP call, Fabric's output shape, Fabric's
// non-2xx-fails rule, and the bounded body. Split out so Functions cannot
// drift from Web on the mechanics they share — the caller owns everything
// schema-specific (which fields exist, what is required, how the URL is
// assembled) and this owns everything HTTP.
func (e *pipelineExecutor) httpActivity(
	kind string,
	act pipeline.Activity,
	method, rawURL string,
	headers map[string]string,
	bodyVal any,
) (map[string]any, error) {
	if e.a.WebActivityStub {
		// The old behaviour, kept for hermetic CI — and labelled in the output
		// so a run that took this path cannot be mistaken for one that called.
		return map[string]any{
			"status": "Succeeded", "activityType": act.Type, "stubbed": true,
		}, nil
	}
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		// Named rather than handed to the client: `net/http` reports this as
		// `unsupported protocol scheme ""`, which does not say which activity
		// or which value produced it.
		return nil, fmt.Errorf("%s activity %q: url %q is not http(s)", kind, act.Name, rawURL)
	}
	var body io.Reader
	if bodyVal != nil {
		body = bytes.NewReader(webBody(bodyVal))
	}

	timeout := webDefaultTimeout
	if act.Policy != nil && act.Policy.Timeout != "" {
		if d, ok := pipeline.ParseTimeout(act.Policy.Timeout); ok && d > 0 {
			timeout = d
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, fmt.Errorf("%s activity %q: %w", kind, act.Name, err)
	}
	// A JSON body with no declared type is the common case in a pipeline
	// definition, and a receiver that reads Content-Type would otherwise get
	// nothing.
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, v := range headers {
		req.Header.Set(name, v)
	}

	resp, err := e.a.webClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s activity %q: %s %s: %w", kind, act.Name, method, rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// httpx.ReadBounded rather than a bare LimitReader: a LimitReader discards
	// the excess and reports success, so an oversized response would be stored
	// as a fragment. It returns nothing at all on refusal, which is what stops
	// the truncation growing back. (internal/httpx has a guard test for this.)
	raw, ok := httpx.ReadBounded(resp.Body, webMaxBody)
	if !ok {
		return nil, fmt.Errorf("%s activity %q: response body is unreadable or exceeds %d bytes — "+
			"the output is held in memory and in the run record, so this activity "+
			"is not a download mechanism", kind, act.Name, webMaxBody)
	}

	respHeaders := map[string]any{}
	for k := range resp.Header {
		respHeaders[k] = resp.Header.Get(k)
	}

	// Fabric FAILS the activity on a non-2xx, rather than returning the status
	// for the pipeline to inspect. A pipeline that treats 500 as success would
	// behave differently here than in Fabric, so the status decides the outcome.
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("%s activity %q: %s %s returned %d: %s",
			kind, act.Name, method, rawURL, resp.StatusCode, snippet(raw))
	}

	out := map[string]any{}
	// A JSON object is merged at the top level, which is what makes
	// `@activity('Get').output.id` resolve. Anything else — an array, a bare
	// string, HTML — has nowhere to merge, so it is carried unparsed.
	var obj map[string]any
	if len(bytes.TrimSpace(raw)) > 0 && json.Unmarshal(raw, &obj) == nil {
		for k, v := range obj {
			out[k] = v
		}
	} else if len(bytes.TrimSpace(raw)) > 0 {
		out["body"] = string(raw)
	}
	out["status"] = "Succeeded"
	out["statusCode"] = resp.StatusCode
	out[webHeadersKey] = respHeaders
	return out, nil
}

// webBody renders a resolved body value as bytes. A string is sent as-is (a
// pipeline author writing `"body": "raw text"` means that text); anything else
// is JSON, which is what a resolved object expression produces.
func webBody(v any) []byte {
	if s, ok := v.(string); ok {
		return []byte(s)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(fmt.Sprint(v))
	}
	return b
}

// snippet bounds an error message. A failing endpoint that returns a whole HTML
// page should not put it in an activity's error field.
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

// defaultWebClient is shared so connections are reused across a ForEach that
// calls the same host repeatedly. No client-level timeout: the per-request
// context carries it, which is what lets policy.timeout differ per activity.
var defaultWebClient = &http.Client{}

func (a *API) webClient() *http.Client {
	if a.WebHTTP != nil {
		return a.WebHTTP
	}
	return defaultWebClient
}
