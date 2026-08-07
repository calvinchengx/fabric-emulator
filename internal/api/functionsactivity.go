package api

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/pipeline"
)

// The Azure Function activity: a real HTTP call to a function endpoint.
//
// The oracle is ADF's published schema (azure-rest-api-specs, entityTypes/
// Pipeline.json): discriminator `AzureFunctionActivity`; `method` and
// `functionName` required; `body` "Required for POST/PUT method, not allowed
// for GET method"; methods GET/POST/PUT/DELETE/OPTIONS/HEAD/TRACE. The
// mechanics ride the same httpActivity core as Web, so the two cannot drift on
// timeouts, bounds, the non-2xx rule, or output shaping.
//
// CONNECTION STAND-IN, stated per the REST-connector precedent: in ADF the
// function's address and key live on an AzureFunctionLinkedService
// (`functionAppUrl` required, `functionKey` a secret). The emulator models no
// connections, so both are taken from typeProperties directly —
// `functionAppUrl` (required here for exactly the reason it is required on the
// linked service) and optional `functionKey`, sent as `x-functions-key`, which
// is how ADF authenticates the call. A definition exported from real Fabric
// carries a connection reference instead; adapting it means inlining those two
// fields, and the error below names that.

// azureFunctionMethods is the schema's enum, verbatim.
var azureFunctionMethods = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "DELETE": true,
	"OPTIONS": true, "HEAD": true, "TRACE": true,
}

func (e *pipelineExecutor) functionsActivity(
	act pipeline.Activity,
	tp map[string]json.RawMessage,
	resolve func(json.RawMessage) (any, error),
) (map[string]any, error) {
	str := func(key string) (string, error) {
		raw, ok := tp[key]
		if !ok || len(raw) == 0 {
			return "", nil
		}
		v, err := resolve(raw)
		if err != nil {
			return "", fmt.Errorf("function activity %q: %s: %w", act.Name, key, err)
		}
		return strings.TrimSpace(fmt.Sprint(v)), nil
	}

	method, err := str("method")
	if err != nil {
		return nil, err
	}
	method = strings.ToUpper(method)
	if method == "" {
		return nil, fmt.Errorf("function activity %q: method is required", act.Name)
	}
	if !azureFunctionMethods[method] {
		return nil, fmt.Errorf("function activity %q: method %q is not in the activity's enum "+
			"(GET, POST, PUT, DELETE, OPTIONS, HEAD, TRACE)", act.Name, method)
	}

	functionName, err := str("functionName")
	if err != nil {
		return nil, err
	}
	if functionName == "" {
		return nil, fmt.Errorf("function activity %q: functionName is required", act.Name)
	}

	appURL, err := str("functionAppUrl")
	if err != nil {
		return nil, err
	}
	if appURL == "" {
		return nil, fmt.Errorf("function activity %q: functionAppUrl is required — in ADF it "+
			"lives on the AzureFunctionLinkedService; the emulator models no connections, so "+
			"inline it (and optionally functionKey) in typeProperties", act.Name)
	}
	key, err := str("functionKey")
	if err != nil {
		return nil, err
	}

	var bodyVal any
	if raw, ok := tp["body"]; ok && len(raw) > 0 {
		v, rerr := resolve(raw)
		if rerr != nil {
			return nil, fmt.Errorf("function activity %q: body: %w", act.Name, rerr)
		}
		bodyVal = v
	}
	// The schema's own words: "Required for POST/PUT method, not allowed for
	// GET method". Enforced both ways — permissiveness here would certify a
	// definition Fabric rejects.
	if method == "GET" && bodyVal != nil {
		return nil, fmt.Errorf("function activity %q: body is not allowed for GET", act.Name)
	}
	if (method == "POST" || method == "PUT") && bodyVal == nil {
		return nil, fmt.Errorf("function activity %q: body is required for %s", act.Name, method)
	}

	headers := map[string]string{}
	if raw, ok := tp["headers"]; ok && len(raw) > 0 {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, fmt.Errorf("function activity %q: headers are not an object", act.Name)
		}
		for name, vraw := range fields {
			v, herr := resolve(vraw)
			if herr != nil {
				return nil, fmt.Errorf("function activity %q: header %q: %w", act.Name, name, herr)
			}
			headers[name] = fmt.Sprint(v)
		}
	}
	if key != "" {
		headers["x-functions-key"] = key
	}

	url := strings.TrimRight(appURL, "/") + "/api/" + strings.TrimLeft(functionName, "/")
	return e.httpActivity("function", act, method, url, headers, bodyVal)
}
